package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"
	"github.com/go-redis/redis/v8"
	"github.com/google/uuid"
	"gitlab.torproject.org/tpo/anti-censorship/pluggable-transports/snowflake/v2/common/messages"
)

const PROXY_ANSWER_TIMEOUT = time.Second * 5

// ClientOffer contains an SDP, bridge fingerprint and the NAT type of the client.
type ClientOffer struct {
	NatType     string `json:"natType"`
	SDP         []byte `json:"sdp"`
	Fingerprint []byte `json:"fingerprint"`
}

// ClientOfferInfo contains the client ID and the client offer.
type ClientOfferInfo struct {
	ClientID    string `json:"clientID"`
	ClientOffer string `json:"clientOffer"`
}

var redisClient *redis.Client

// init initializes the Redis client.
func init() {
	redisClient = redis.NewClient(&redis.Options{
		Addr:     os.Getenv("REDIS_ADDRESS"),
		Password: "",
		DB:       0,
		// TLSConfig: &tls.Config{
		// 	InsecureSkipVerify: true,
		// },
	})

	_, err := redisClient.Ping(context.Background()).Result()
	if err != nil {
		log.Fatalf("Failed to connect to Redis: %v", err)
	}
}

// clientHandler decodes the request and returns the APIGateway response.
func clientHandler(ctx context.Context, request events.APIGatewayProxyRequest) (events.APIGatewayProxyResponse, error) {
	log.Print("Received client offer request inside clientHandler")

	body := request.Body
	if request.IsBase64Encoded {
		decodedBody, err := base64.StdEncoding.DecodeString(body)
		if err != nil {
			log.Printf("Error decoding base64 body: %v", err)
			return events.APIGatewayProxyResponse{
				StatusCode: 400,
				Body:       "Invalid base64 encoding",
			}, nil
		}
		body = string(decodedBody)
	}

	log.Printf("Body in client handler: %s", body)
	// TODO-LATER: use same strategy in util.GetClientIp(r) to get the remote address
	remoteAddr := request.RequestContext.Identity.SourceIP
	// Make client ID a random string for now
	clientID := uuid.New().String()
	log.Printf("Generated Client ID: %s", clientID)

	arg := messages.Arg{
		Body:             []byte(body),
		RemoteAddr:       remoteAddr,
		RendezvousMethod: messages.RendezvousHttp,
	}

	var response []byte
	err := handleClientOffer(ctx, arg, clientID, &response)
	if err != nil {
		log.Println(err)
		return events.APIGatewayProxyResponse{
			StatusCode: 500,
			Body:       "Internal server error",
		}, nil
	}

	return events.APIGatewayProxyResponse{
		StatusCode: 200,
		Body:       string(response),
	}, nil
}

func waitForProxyAnswer(ctx context.Context, clientID string) (string, error) {
	clientQueue := fmt.Sprintf("client_queue:%s", clientID)

	// Wait for an answer from the proxy with a timeout
	result, err := redisClient.BLPop(ctx, PROXY_ANSWER_TIMEOUT, clientQueue).Result()
	if err != nil {
		if err == redis.Nil {
			log.Printf("Timeout: No proxy answer received for client %s", clientID)
			return "", fmt.Errorf("timeout waiting for proxy answer")
		}
		return "", fmt.Errorf("failed to retrieve proxy answer: %v", err)
	}

	// BLPOP returns a slice with the key and value; we want the value (the answer)
	answer := result[1]
	return answer, nil
}

// Update the proxy database with the matched client
func updateProxyWithMatchedClient(ctx context.Context, proxyID, clientID string, clientOffer ClientOffer) error {
	clientOfferJSON, err := json.Marshal(clientOffer)
	if err != nil {
		return fmt.Errorf("failed to marshal client offer: %v", err)
	}

	err = redisClient.HMSet(ctx, fmt.Sprintf("proxy:%s", proxyID), map[string]interface{}{
		"clientId":    clientID,
		"clientOffer": clientOfferJSON,
	}).Err()
	if err != nil {
		return fmt.Errorf("failed to update matched client in Redis: %v", err)
	}

	clientOfferQueue := fmt.Sprintf("client_offer_queue:%s", proxyID)
	// Push the clientId and clientOffer to the proxy's queue
	err = redisClient.LPush(ctx, clientOfferQueue, clientID).Err()
	if err != nil {
		log.Printf("Error pushing clientID to Redis for proxy %s: %v", proxyID, err)
		return fmt.Errorf("failed to store clientID for proxy %s", proxyID)
	}
	log.Printf("Matched proxy %s with client %s", proxyID, clientID)

	return nil
}

// handleClientOffer checks for an available proxy and waits for the proxy answer returning the answer as a response.
func handleClientOffer(ctx context.Context, arg messages.Arg, clientID string, response *[]byte) error {
	req, err := messages.DecodeClientPollRequest(arg.Body)
	if err != nil {
		return sendClientResponse(&messages.ClientPollResponse{Error: err.Error()}, response)
	}

	offer := &ClientOffer{
		NatType: req.NAT,
		SDP:     []byte(req.Offer),
	}

	// Immediately check for an available proxy using LPop
	// TO-DO: two queues for different restricted
	proxyResult, err := redisClient.LPop(ctx, "waiting_proxies").Result()
	if err != nil {
		if err == redis.Nil {
			// No proxy available
			return sendClientResponse(&messages.ClientPollResponse{Error: "No proxy available"}, response)
		}
		return fmt.Errorf("failed to check available proxies: %v", err)
	}

	proxyID := proxyResult
	log.Printf("Assigned proxy ID: %s for client %s", proxyID, clientID)

	updateProxyWithMatchedClient(ctx, proxyID, clientID, *offer)

	// Wait for proxy answer with a blocking call
	answer, err := waitForProxyAnswer(ctx, clientID)
	if err != nil {
		if err.Error() == "timeout waiting for proxy answer" {
			// Handle timeout case: no proxy answer received within the timeout
			redisClient.Del(ctx, fmt.Sprintf("client:%s", clientID))
			return sendClientResponse(&messages.ClientPollResponse{Error: "No proxy answer received"}, response)
		}
		// Handle other errors
		return fmt.Errorf("error waiting for proxy answer: %v", err)
	}

	return sendClientResponse(&messages.ClientPollResponse{Answer: answer}, response)
}

// sendClientResponse is a helper function to send the error response.
func sendClientResponse(resp *messages.ClientPollResponse, response *[]byte) error {
	respBytes, err := json.Marshal(resp)
	if err != nil {
		return fmt.Errorf("failed to marshal error response: %v", err)
	}
	*response = respBytes
	return nil
}

func main() {
	lambda.Start(clientHandler)
}
