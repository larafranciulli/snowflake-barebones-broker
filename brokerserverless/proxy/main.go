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

	"gitlab.torproject.org/tpo/anti-censorship/pluggable-transports/snowflake/v2/common/messages"
)

const DATABASE_EXPIRATION = 5 * time.Minute
const CLIENT_OFFER_TIMEOUT = 5 * time.Second

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

	_, err := redisClient.Ping(context.Background()).Result()
	if err != nil {
		log.Fatalf("Failed to connect to Redis: %v", err)
	}
	log.Print("Connected to Redis successfully.")
}

// proxyHandler decodes the request and returns the APIGateway response.
func proxyHandler(ctx context.Context, request events.APIGatewayProxyRequest) (events.APIGatewayProxyResponse, error) {
	log.Print("Received proxy poll request inside proxyHandler")

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
	log.Printf("Body in proxy handler: %s", body)

	remoteAddr := request.RequestContext.Identity.SourceIP

	arg := messages.Arg{
		Body:       []byte(body),
		RemoteAddr: remoteAddr,
	}

	var response []byte
	err := handleProxyPolls(ctx, arg, &response)
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

// addProxyToRedis adds a proxy to the proxy list and waiting_proxies list in
// Redis with the given proxyID and natType.
func addProxyToRedis(ctx context.Context, proxyID, natType string) error {
	err := redisClient.HSet(ctx, fmt.Sprintf("proxy:%s", proxyID),
		"clientId", "", // client is empty initially, meaning no match
		"natType", natType).Err()
	if err != nil {
		return fmt.Errorf("failed to add proxy to Redis: %v", err)
	}

	err = redisClient.Expire(ctx, fmt.Sprintf("proxy:%s", proxyID), DATABASE_EXPIRATION).Err()
	if err != nil {
		return fmt.Errorf("failed to set expiration for proxy data: %v", err)
	}

	// Add the proxyID to a Redis list of proxies waiting for a match
	err = redisClient.RPush(ctx, "waiting_proxies", proxyID).Err()
	if err != nil {
		return fmt.Errorf("failed to add proxy to waiting list: %v", err)
	}
	return nil
}

// waitForMatchedClient waits for a client offer from the proxy with the given id and returns
// the corresponding clientID and client offer if found.
func waitForMatchedClient(ctx context.Context, proxyID string) (string, ClientOffer, error) {
	var clientOffer ClientOffer
	clientOfferQueue := fmt.Sprintf("client_offer_queue:%s", proxyID)

	// Wait for an clientID from the proxy with a timeout
	result, err := redisClient.BLPop(ctx, CLIENT_OFFER_TIMEOUT, clientOfferQueue).Result()
	if err != nil {
		if err == redis.Nil {
			log.Printf("Timeout: No client offer received for proxy %s", proxyID)
			return "", clientOffer, nil
		}
		return "", clientOffer, fmt.Errorf("failed to retrieve client offer: %v", err)
	}
	clientID := result[1]

	clientOfferJSON, err := redisClient.HGet(ctx, fmt.Sprintf("proxy:%s", proxyID), "clientOffer").Result()
	if err != nil {
		return "", clientOffer, fmt.Errorf("failed to retrieve client offer: %v", err)
	}

	err = json.Unmarshal([]byte(clientOfferJSON), &clientOffer)
	if err != nil {
		return "", clientOffer, fmt.Errorf("failed to unmarshal client offer: %v", err)
	}

	return clientID, clientOffer, nil
}

// cleanupProxy fully removes the proxy from Redis.
func cleanupProxy(ctx context.Context, proxyID string) {
	redisClient.Del(ctx, fmt.Sprintf("proxy:%s", proxyID))
	redisClient.LRem(ctx, "waiting_proxies", 0, proxyID)
	log.Printf("Removed proxy %s from Redis", proxyID)
}

// handleProxyPolls handles the proxy poll request and returns the response.
func handleProxyPolls(ctx context.Context, arg messages.Arg, response *[]byte) error {
	sid, proxyType, natType, clients, relayPattern, relayPatternSupported, err := messages.DecodeProxyPollRequestWithRelayPrefix(arg.Body)
	if err != nil {
		return err
	}
	log.Printf("Received proxy poll request: sid=%s, proxyType=%s, natType=%s, clients=%d, relayPattern=%s, relayPatternSupported=%t", sid, proxyType, natType, clients, relayPattern, relayPatternSupported)

	proxyID := sid // assuming 'sid' is the proxy ID
	err = addProxyToRedis(ctx, proxyID, natType)
	if err != nil {
		return fmt.Errorf("error adding proxy to Redis: %v", err)
	}

	clientID, offer, err := waitForMatchedClient(ctx, proxyID)
	if err != nil {
		return fmt.Errorf("error waiting for client match: %v", err)
	}

	var b []byte

	if clientID == "" {
		log.Printf("No client offer found for proxy %s. Removing proxy from Redis.", sid)

		cleanupProxy(ctx, proxyID)
		b, err := messages.EncodePollResponse("", false, "")
		if err != nil {
			return fmt.Errorf("failed to encode poll response: %v", err)
		}
		*response = b
		return nil
	}

	log.Printf("Matched client %s with proxy %s", clientID, proxyID)

	redisClient.LRem(ctx, "waiting_proxies", 0, proxyID)

	var relayURL = ""
	b, err = messages.EncodePollResponseWithRelayURL(string(offer.SDP), true, offer.NatType, relayURL, "")
	if err != nil {
		return messages.ErrInternal
	}
	*response = b
	return nil
}

func main() {
	lambda.Start(proxyHandler)
}
