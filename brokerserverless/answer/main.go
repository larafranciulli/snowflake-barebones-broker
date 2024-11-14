package main

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"log"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"
	"github.com/go-redis/redis/v8"

	"gitlab.torproject.org/tpo/anti-censorship/pluggable-transports/snowflake/v2/common/messages"
)

// Client offer contains an SDP, bridge fingerprint and the NAT type of the client
type ClientOffer struct {
	NatType     string `json:"natType"`
	SDP         []byte `json:"sdp"`
	Fingerprint []byte `json:"fingerprint"`
}

func answerHandler(ctx context.Context, request events.APIGatewayProxyRequest) (events.APIGatewayProxyResponse, error) {
	log.Print("Received proxy answer request inside answer Handler- hiiiiiiiiiii")

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
	log.Printf("Body in answer handler: %s", body)

	// TO-DO: Fix later
	remoteAddr := request.RequestContext.Identity.SourceIP

	arg := messages.Arg{
		Body:       []byte(body),
		RemoteAddr: remoteAddr,
	}

	var response []byte
	err := handleProxyAnswers(ctx, arg, &response)
	if err != nil {
		log.Printf("Error processing proxy poll: %v", err)
		return events.APIGatewayProxyResponse{
			StatusCode: 500,
			Body:       fmt.Sprintf("Internal Server Error: %v", err),
		}, nil
	}

	return events.APIGatewayProxyResponse{
		StatusCode: 200,
		Body:       string(response),
	}, nil
}

var redisClient *redis.Client

func init() {
	log.Print("Initializing Redis client...")
	redisClient = redis.NewClient(&redis.Options{
		Addr:     "proxyclientmatching-vuhw23.serverless.use1.cache.amazonaws.com:6379",
		Password: "",
		DB:       0,
		TLSConfig: &tls.Config{
			InsecureSkipVerify: true, // Set to false in production if you require strict certificate validation
		},
	})

	// Check if Redis is reachable
	_, err := redisClient.Ping(context.Background()).Result()
	if err != nil {
		log.Fatalf("Failed to connect to Redis: %v", err)
	}
	log.Print("Connected to Redis successfully.")
}

func handleProxyAnswers(ctx context.Context, arg messages.Arg, response *[]byte) error {
	// Decode the SDP and validate it
	answer, proxyID, err := messages.DecodeAnswerRequest(arg.Body)
	if err != nil || answer == "" {
		log.Printf("Invalid SDP or answer: %v", err)
		return fmt.Errorf("invalid SDP or answer")
	}
	// Extract the client ID associated with this proxy (presumably stored in a Redis set or database)
	// For simplicity, let's assume we have a mechanism to retrieve the clientID from the proxyID (e.g., from a Redis mapping)
	clientID, err := getClientIDFromProxyID(ctx, proxyID)
	if err != nil {
		log.Printf("Error fetching client ID from proxy ID: %v", err)
		return fmt.Errorf("error fetching client ID from proxy ID")
	}

	// Use the client ID to identify the queue for this client
	clientQueue := fmt.Sprintf("client_queue:%s", clientID)

	// Push the proxy answer to the client's queue in Redis
	err = redisClient.LPush(ctx, clientQueue, answer).Err()
	if err != nil {
		log.Printf("Error pushing answer to Redis for client %s: %v", clientID, err)
		return fmt.Errorf("failed to store proxy answer for client %s", clientID)
	}

	// Encode the response message based on success
	b, err := messages.EncodeAnswerResponse(true) // Assume success for now
	if err != nil {
		log.Printf("Error encoding answer: %s", err)
		return fmt.Errorf("error encoding answer: %v", err)
	}
	*response = b

	return nil
}

// Fetch the client ID associated with a proxy ID from Redis
func getClientIDFromProxyID(ctx context.Context, proxyID string) (string, error) {
	// Check if the "clientId" field exists in the hash
	exists, err := redisClient.HExists(ctx, fmt.Sprintf("proxy:%s", proxyID), "clientId").Result()
	if err != nil {
		return "", fmt.Errorf("error checking if clientId exists: %v", err)
	}
	if !exists {
		// ERRORING ON THIS LINE
		return "", fmt.Errorf("no clientId associated with proxy ID %s", proxyID)
	}

	// Fetch the clientID from the Redis hash for the proxy
	clientID, err := redisClient.HGet(ctx, fmt.Sprintf("proxy:%s", proxyID), "clientId").Result()
	if err == redis.Nil {
		return "", fmt.Errorf("no client associated with proxy ID %s", proxyID)
	} else if err != nil {
		return "", fmt.Errorf("error fetching client ID from Redis: %v", err)
	}

	// Return the client ID if found
	return clientID, nil
}

func main() {
	lambda.Start(answerHandler)
}
