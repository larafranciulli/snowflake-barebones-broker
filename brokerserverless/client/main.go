package main

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"
	"github.com/go-redis/redis/v8"
	"github.com/google/uuid"
	"gitlab.torproject.org/tpo/anti-censorship/pluggable-transports/snowflake/v2/common/messages"
)

// Client offer contains an SDP, bridge fingerprint and the NAT type of the client
type ClientOffer struct {
	NatType     string `json:"natType"`
	SDP         []byte `json:"sdp"`
	Fingerprint []byte `json:"fingerprint"`
}

var redisClient *redis.Client

const proxyAnswerTimeout = time.Second * 5

func init() {
	redisClient = redis.NewClient(&redis.Options{
		Addr:     "proxyclientmatching-vuhw23.serverless.use1.cache.amazonaws.com:6379",
		Password: "",
		DB:       0, // default DB
		TLSConfig: &tls.Config{
			InsecureSkipVerify: true, // Set to false in production if you require strict certificate validation
		},
	})

	// Check if Redis is reachable
	_, err := redisClient.Ping(context.Background()).Result()
	if err != nil {
		log.Fatalf("Failed to connect to Redis: %v", err)
	}
}

func clientHandler(ctx context.Context, request events.APIGatewayProxyRequest) (events.APIGatewayProxyResponse, error) {
	log.Print("Received client offer request inside clientHandler- hiiiiiiiiiii")

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
	// Define the Redis list name unique to this client
	clientQueue := fmt.Sprintf("client_queue:%s", clientID)

	// Wait for an answer from the proxy with a timeout
	result, err := redisClient.BLPop(ctx, proxyAnswerTimeout, clientQueue).Result()
	if err != nil {
		if err == redis.Nil {
			// Timeout reached without receiving an answer
			log.Printf("Timeout: No proxy answer received for client %s", clientID)
			return "", fmt.Errorf("timeout waiting for proxy answer")
		}
		// Other Redis error
		return "", fmt.Errorf("failed to retrieve proxy answer: %v", err)
	}

	// BLPOP returns a slice with the key and value; we want the value (the answer)
	answer := result[1]
	return answer, nil
}

// Update the proxy database with the matched client
func updateProxyWithMatchedClient(ctx context.Context, proxyID, clientID string, clientOffer ClientOffer) error {
	// Assuming you're using a Redis hash to store the matched client
	clientOfferJSON, err := json.Marshal(clientOffer)
	if err != nil {
		return fmt.Errorf("failed to marshal client offer: %v", err)
	}

	// Update the database for this proxyId
	err = redisClient.HMSet(ctx, fmt.Sprintf("proxy:%s", proxyID), map[string]interface{}{
		"clientId":    clientID,
		"clientOffer": clientOfferJSON,
	}).Err()
	if err != nil {
		return fmt.Errorf("failed to update matched client in Redis: %v", err)
	}
	log.Printf("Matched proxy %s with client %s", proxyID, clientID)

	return nil
}

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

	// Send successful response with the answer received
	return sendClientResponse(&messages.ClientPollResponse{Answer: answer}, response)
}

// Helper function to send the error response
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
