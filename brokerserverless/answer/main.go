package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"
	"github.com/go-redis/redis/v8"

	"gitlab.torproject.org/tpo/anti-censorship/pluggable-transports/snowflake/v2/common/messages"
)

const DATABASE_EXPIRATION = 5 * time.Minute

// ClientOffer contains an SDP, bridge fingerprint and the NAT type of the client.
type ClientOffer struct {
	NatType     string `json:"natType"`
	SDP         []byte `json:"sdp"`
	Fingerprint []byte `json:"fingerprint"`
}

var redisClient *redis.Client

// init initializes the Redis client.
func init() {
	log.Print("Initializing Redis client...")
	redisClient = redis.NewClient(&redis.Options{
		Addr:     os.Getenv("REDIS_ADDRESS"),
		Password: "",
		DB:       0,
		// TLSConfig: &tls.Config{
		// 	InsecureSkipVerify: true,
		// },
	})

	// Check if Redis is reachable.
	_, err := redisClient.Ping(context.Background()).Result()
	if err != nil {
		log.Fatalf("Failed to connect to Redis: %v", err)
	}
	log.Print("Connected to Redis successfully.")
}

// answerHandler decodes the request and returns the APIGateway response.
func answerHandler(ctx context.Context, request events.APIGatewayProxyRequest) (events.APIGatewayProxyResponse, error) {
	log.Print("Received proxy answer request inside answer Handler")

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

	// TO-DO: Fix later with proper source IP address
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

// handleProxyAnswers handles stores the proxy answer request in Redis.
func handleProxyAnswers(ctx context.Context, arg messages.Arg, response *[]byte) error {
	// Decode the SDP and validate it
	answer, proxyID, err := messages.DecodeAnswerRequest(arg.Body)
	if err != nil || answer == "" {
		log.Printf("Invalid SDP or answer: %v", err)
		return fmt.Errorf("invalid SDP or answer")
	}

	clientID, err := getClientIDFromProxyID(ctx, proxyID)
	if err != nil {
		log.Printf("Error fetching client ID from proxy ID: %v", err)
		return fmt.Errorf("error fetching client ID from proxy ID")
	}

	clientQueue := fmt.Sprintf("client_queue:%s", clientID)

	// Push the proxy answer to the client's queue in Redis
	err = redisClient.LPush(ctx, clientQueue, answer).Err()
	if err != nil {
		log.Printf("Error pushing answer to Redis for client %s: %v", clientID, err)
		return fmt.Errorf("failed to store proxy answer for client %s", clientID)
	}

	err = redisClient.Expire(ctx, clientQueue, DATABASE_EXPIRATION).Err()
	if err != nil {
		log.Printf("Error setting expiry for client queue %s: %v", clientQueue, err)
		return fmt.Errorf("failed to set expiry for client queue %s", clientQueue)
	}

	b, err := messages.EncodeAnswerResponse(true)
	if err != nil {
		log.Printf("Error encoding answer: %s", err)
		return fmt.Errorf("error encoding answer: %v", err)
	}
	*response = b

	return nil
}

// getClientIDFromProxyID fetches the client ID associated with a proxy ID from Redis.
func getClientIDFromProxyID(ctx context.Context, proxyID string) (string, error) {
	exists, err := redisClient.HExists(ctx, fmt.Sprintf("proxy:%s", proxyID), "clientId").Result()
	if err != nil {
		return "", fmt.Errorf("error checking if clientId exists: %v", err)
	}
	if !exists {
		return "", fmt.Errorf("no clientId associated with proxy ID %s", proxyID)
	}

	clientID, err := redisClient.HGet(ctx, fmt.Sprintf("proxy:%s", proxyID), "clientId").Result()
	if err == redis.Nil {
		return "", fmt.Errorf("no client associated with proxy ID %s", proxyID)
	} else if err != nil {
		return "", fmt.Errorf("error fetching client ID from Redis: %v", err)
	}

	return clientID, nil
}

func main() {
	lambda.Start(answerHandler)
}
