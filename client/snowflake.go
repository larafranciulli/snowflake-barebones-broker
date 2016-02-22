// Client transport plugin for the Snowflake pluggable transport.
package main

import (
	"bufio"
	"errors"
	"flag"
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"git.torproject.org/pluggable-transports/goptlib.git"
	"github.com/keroserene/go-webrtc"
)

var ptInfo pt.ClientInfo
var logFile *os.File
var brokerURL string
var frontDomain string

// When a connection handler starts, +1 is written to this channel; when it
// ends, -1 is written.
var handlerChan = make(chan int)

const (
	ReconnectTimeout = 5
)

func copyLoop(a, b net.Conn) {
	var wg sync.WaitGroup
	wg.Add(2)
	// TODO fix copy loop recovery
	go func() {
		io.Copy(b, a)
		log.Println("copy loop b-a break")
		wg.Done()
	}()
	go func() {
		io.Copy(a, b)
		log.Println("copy loop a-b break")
		wg.Done()
	}()
	wg.Wait()
	log.Println("copy loop ended")
}

// Interface that matches both webrc.DataChannel and for testing.
type SnowflakeChannel interface {
	Send([]byte)
	Close() error
}

// Initialize a WebRTC Connection.
func dialWebRTC(config *webrtc.Configuration, broker *BrokerChannel) (
	*webRTCConn, error) {
	connection := new(webRTCConn)
	connection.config = config
	connection.broker = broker
	connection.offerChannel = make(chan *webrtc.SessionDescription)
	connection.answerChannel = make(chan *webrtc.SessionDescription)
	connection.writeChannel = make(chan []byte)
	connection.errorChannel = make(chan error)
	connection.reset = make(chan struct{})
	connection.BytesInfo = &BytesInfo{
		inboundChan: make(chan int), outboundChan: make(chan int),
		inbound: 0, outbound: 0, inEvents: 0, outEvents: 0,
	}
	go connection.BytesInfo.Log()

	// Pipes remain the same even when DataChannel gets switched.
	connection.recvPipe, connection.writePipe = io.Pipe()

	go connection.ConnectLoop()
	go connection.SendLoop()
	return connection, nil
}

func endWebRTC() {
	log.Printf("WebRTC: interruped")
	if nil == webrtcRemote {
		return
	}
	if nil != webrtcRemote.snowflake {
		s := webrtcRemote.snowflake
		webrtcRemote.snowflake = nil
		log.Printf("WebRTC: closing DataChannel")
		s.Close()
	}
	if nil != webrtcRemote.pc {
		log.Printf("WebRTC: closing PeerConnection")
		webrtcRemote.pc.Close()
	}
}

// Establish a WebRTC channel for SOCKS connections.
func handler(conn *pt.SocksConn) error {
	handlerChan <- 1
	defer func() {
		handlerChan <- -1
	}()
	defer conn.Close()
	log.Println("handler fired:", conn)

	// TODO: [#3] Fetch ICE server information from Broker.
	// TODO: [#18] Consider TURN servers here too.
	config := webrtc.NewConfiguration(
		webrtc.OptionIceServer("stun:stun.l.google.com:19302"))
	broker := NewBrokerChannel(brokerURL, frontDomain)
	if nil == broker {
		conn.Reject()
		return errors.New("Failed to prepare BrokerChannel")
	}
	remote, err := dialWebRTC(config, broker)
	if err != nil {
		conn.Reject()
		return err
	}
	defer remote.Close()
	webrtcRemote = remote

	err = conn.Grant(&net.TCPAddr{IP: net.IPv4zero, Port: 0})
	if err != nil {
		return err
	}

	// TODO: Make SOCKS acceptance more independent from WebRTC so they can
	// be more easily interchanged.
	copyLoop(conn, remote)
	log.Println("----END---")
	return nil
}

func acceptLoop(ln *pt.SocksListener) error {
	defer ln.Close()
	for {
		conn, err := ln.AcceptSocks()
		if err != nil {
			if e, ok := err.(net.Error); ok && e.Temporary() {
				continue
			}
			return err
		}
		go func() {
			err := handler(conn)
			if err != nil {
				log.Printf("handler error: %s", err)
			}
		}()
	}
}

func readSignalingMessages(f *os.File) {
	log.Printf("readSignalingMessages")
	s := bufio.NewScanner(f)
	for s.Scan() {
		msg := s.Text()
		log.Printf("readSignalingMessages loop %+q", msg)
		sdp := webrtc.DeserializeSessionDescription(msg)
		if sdp == nil {
			log.Printf("ignoring invalid signal message %+q", msg)
			continue
		}
		webrtcRemote.answerChannel <- sdp
	}
	log.Printf("close answerChannel")
	close(webrtcRemote.answerChannel)
	if err := s.Err(); err != nil {
		log.Printf("signal FIFO: %s", err)
	}
}

func main() {
	var err error
	webrtc.SetLoggingVerbosity(1)
	flag.StringVar(&brokerURL, "url", "", "URL of signaling broker")
	flag.StringVar(&frontDomain, "front", "", "front domain")
	flag.Parse()
	logFile, err = os.OpenFile("snowflake.log", os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		log.Fatal(err)
	}
	defer logFile.Close()
	log.SetOutput(logFile)
	log.Println("\nStarting Snowflake Client...")

	// Expect user to copy-paste if
	// TODO: Maybe just get rid of copy-paste entirely.
	if "" != brokerURL {
		log.Println("Rendezvous using Broker at: ", brokerURL)
		if "" != frontDomain {
			log.Println("Domain fronting using:", frontDomain)
		}
	} else {
		log.Println("No HTTP signaling detected. Waiting for a \"signal\" pipe...")
		// This FIFO receives signaling messages.
		err := syscall.Mkfifo("signal", 0600)
		if err != nil {
			if err.(syscall.Errno) != syscall.EEXIST {
				log.Fatal(err)
			}
		}
		signalFile, err := os.OpenFile("signal", os.O_RDONLY, 0600)
		if err != nil {
			log.Fatal(err)
		}
		defer signalFile.Close()
		go readSignalingMessages(signalFile)
	}

	ptInfo, err = pt.ClientSetup(nil)
	if err != nil {
		log.Fatal(err)
	}

	if ptInfo.ProxyURL != nil {
		pt.ProxyError("proxy is not supported")
		os.Exit(1)
	}

	listeners := make([]net.Listener, 0)
	for _, methodName := range ptInfo.MethodNames {
		switch methodName {
		case "snowflake":
			ln, err := pt.ListenSocks("tcp", "127.0.0.1:0")
			if err != nil {
				pt.CmethodError(methodName, err.Error())
				break
			}
			go acceptLoop(ln)
			pt.Cmethod(methodName, ln.Version(), ln.Addr())
			listeners = append(listeners, ln)
		default:
			pt.CmethodError(methodName, "no such method")
		}
	}
	pt.CmethodsDone()
	defer endWebRTC()

	var numHandlers int = 0
	var sig os.Signal
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	// wait for first signal
	sig = nil
	for sig == nil {
		select {
		case n := <-handlerChan:
			numHandlers += n
		case sig = <-sigChan:
		}
	}
	for _, ln := range listeners {
		ln.Close()
	}

	// wait for second signal or no more handlers
	sig = nil
	for sig == nil && numHandlers != 0 {
		select {
		case n := <-handlerChan:
			numHandlers += n
		case sig = <-sigChan:
		}
	}
}
