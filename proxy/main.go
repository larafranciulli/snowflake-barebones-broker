package main

import (
	"flag"
	"io"
	"log"
	"os"

	"git.torproject.org/pluggable-transports/snowflake.git/v2/common/safelog"
	sf "git.torproject.org/pluggable-transports/snowflake.git/v2/proxy/lib"
)

func main() {
	capacity := flag.Uint("capacity", 0, "maximum concurrent clients")
	stunURL := flag.String("stun", sf.DefaultSTUNURL, "broker URL")
	logFilename := flag.String("log", "", "log filename")
	rawBrokerURL := flag.String("broker", sf.DefaultBrokerURL, "broker URL")
	unsafeLogging := flag.Bool("unsafe-logging", false, "prevent logs from being scrubbed")
	keepLocalAddresses := flag.Bool("keep-local-addresses", false, "keep local LAN address ICE candidates")
	relayURL := flag.String("relay", sf.DefaultRelayURL, "websocket relay URL")
	NATTypeMeasurementIntervalSecond := flag.Uint("nat-type-measurement-interval-second", 0,
		"the time interval in second before NAT type is retested, 0 disables retest")

	flag.Parse()

	proxy := sf.SnowflakeProxy{
		Capacity:           uint(*capacity),
		STUNURL:            *stunURL,
		BrokerURL:          *rawBrokerURL,
		KeepLocalAddresses: *keepLocalAddresses,
		RelayURL:           *relayURL,

		NATTypeMeasurementIntervalSecond: *NATTypeMeasurementIntervalSecond,
	}

	var logOutput io.Writer = os.Stderr
	log.SetFlags(log.LstdFlags | log.LUTC)

	log.SetFlags(log.LstdFlags | log.LUTC)
	if *logFilename != "" {
		f, err := os.OpenFile(*logFilename, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
		if err != nil {
			log.Fatal(err)
		}
		defer f.Close()
		logOutput = io.MultiWriter(os.Stderr, f)
	}
	if *unsafeLogging {
		log.SetOutput(logOutput)
	} else {
		log.SetOutput(&safelog.LogScrubber{Output: logOutput})
	}

	err := proxy.Start()
	if err != nil {
		log.Fatal(err)
	}
}