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

	// "gitlab.torproject.org/tpo/anti-censorship/pluggable-transports/snowflake/v2/common/bridgefingerprint"
	"gitlab.torproject.org/tpo/anti-censorship/pluggable-transports/snowflake/v2/common/messages"
)

// Client offer contains an SDP, bridge fingerprint and the NAT type of the client
type ClientOffer struct {
	NatType     string `json:"natType"`
	SDP         []byte `json:"sdp"`
	Fingerprint []byte `json:"fingerprint"`
}

func proxyHandler(ctx context.Context, request events.APIGatewayProxyRequest) (events.APIGatewayProxyResponse, error) {
	log.Print("Received proxy poll request inside proxyHandler- hiiiiiiiiiii")

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

func addProxyToRedis(ctx context.Context, proxyID, natType string) error {
	// Create a Redis hash with proxy data
	err := redisClient.HSet(ctx, fmt.Sprintf("proxy:%s", proxyID),
		"client", "", // client is empty initially, meaning no match
		"status", "waiting for client offer", // proxy is waiting for a client
		"natType", natType).Err()
	if err != nil {
		return fmt.Errorf("failed to add proxy to Redis: %v", err)
	}

	// Set the expiration time for the proxy data to 5 minutes (300 seconds)
	err = redisClient.Expire(ctx, fmt.Sprintf("proxy:%s", proxyID), 5*time.Minute).Err()
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

// func waitForClientMatch(ctx context.Context) (string, ClientOffer, error) {
// 	// BLPop blocks until a client is available, returns a slice with 2 elements:
// 	// [0] is the list name ("waiting_clients"), [1] is the clientId (remoteAddr)
// 	result, err := redisClient.BLPop(ctx, 5*time.Second, "waiting_clients").Result()
// 	if err != nil {
// 		if err == redis.Nil {
// 			// Timeout without a match
// 			return "", ClientOffer{}, nil
// 		}
// 		return "", ClientOffer{}, fmt.Errorf("failed to perform BLPop: %v", err)
// 	}

// 	// The clientID is the second element in the slice
// 	clientID := result[1]

// 	// Use the clientId (remoteAddr) to retrieve the offer associated with the client
// 	offerJSON, err := redisClient.HGet(ctx, fmt.Sprintf("client:%s", clientID), "offer").Result()
// 	if err != nil {
// 		return "", ClientOffer{}, fmt.Errorf("failed to get client offer from Redis: %v", err)
// 	}

// 	var offer ClientOffer
// 	err = json.Unmarshal([]byte(offerJSON), &offer)
// 	if err != nil {
// 		return "", ClientOffer{}, fmt.Errorf("failed to unmarshal client offer: %v", err)
// 	}

// 	// Returning the clientId and the associated offer
// 	return clientID, offer, nil
// }

// ProxyPolls waits for the matched client ID and client offer in the database
func waitForMatchedClient(ctx context.Context, proxyID string) (string, ClientOffer, error) {
	// Polling loop to check if clientId and clientOffer have been set
	var clientOffer ClientOffer

	// Timeout after 5 seconds to avoid hanging indefinitely
	timeout := time.After(5 * time.Second)
	ticker := time.NewTicker(500 * time.Millisecond)

	for {
		select {
		case <-timeout:
			// Return empty clientID and nil clientOffer if timeout is reached
			return "", clientOffer, nil
		case <-ticker.C:
			// Get the current clientId and clientOffer from the Redis hash
			clientData, err := redisClient.HGetAll(ctx, fmt.Sprintf("proxy:%s", proxyID)).Result()
			if err != nil {
				return "", clientOffer, fmt.Errorf("failed to retrieve proxy data: %v", err)
			}

			// If both clientId and clientOffer are populated, break the loop
			if clientID, ok := clientData["clientId"]; ok && clientID != "" {
				clientOfferJSON, err := redisClient.HGet(ctx, fmt.Sprintf("proxy:%s", proxyID), "clientOffer").Result()
				if err != nil {
					return "", clientOffer, fmt.Errorf("failed to retrieve client offer: %v", err)
				}

				err = json.Unmarshal([]byte(clientOfferJSON), &clientOffer)
				if err != nil {
					return "", clientOffer, fmt.Errorf("failed to unmarshal client offer: %v", err)
				}

				// Successfully found matched client and offer
				return clientID, clientOffer, nil
			}
		}
	}
}

// Helper function to remove proxy from Redis
func cleanupProxy(ctx context.Context, proxyID string) {
	// Remove proxy hash and waiting list entry
	redisClient.Del(ctx, fmt.Sprintf("proxy:%s", proxyID))
	redisClient.LRem(ctx, "waiting_proxies", 0, proxyID)
	log.Printf("Removed proxy %s from Redis", proxyID)
}

func handleProxyPolls(ctx context.Context, arg messages.Arg, response *[]byte) error {
	sid, proxyType, natType, clients, relayPattern, relayPatternSupported, err := messages.DecodeProxyPollRequestWithRelayPrefix(arg.Body)
	if err != nil {
		return err
	}
	log.Printf("Received proxy poll request: sid=%s, proxyType=%s, natType=%s, clients=%d, relayPattern=%s, relayPatternSupported=%t", sid, proxyType, natType, clients, relayPattern, relayPatternSupported)

	var b []byte

	// Wait for a client to avail an offer to the snowflake, or timeout if nil.

	// MAIN TODO: Implement retrieving offer from broker database and return
	// Add proxy to Redis
	proxyID := sid // assuming 'sid' is the proxy ID
	err = addProxyToRedis(ctx, proxyID, natType)
	if err != nil {
		return fmt.Errorf("error adding proxy to Redis: %v", err)
	}

	// Wait for client match (block for up to 5 seconds)
	clientID, offer, err := waitForMatchedClient(ctx, proxyID)
	if err != nil {
		return fmt.Errorf("error waiting for client match: %v", err)
	}

	// If no client matched, return failure
	if clientID == "" {
		log.Printf("No client offer found for proxy %s. Removing proxy from Redis.", sid)

		cleanupProxy(ctx, proxyID)
		// Encode the response
		b, err := messages.EncodePollResponse("", false, "")
		if err != nil {
			return fmt.Errorf("failed to encode poll response: %v", err)
		}
		*response = b
		return nil
	}

	log.Printf("Matched client %s with proxy %s", clientID, proxyID)

	// Clean up: Remove proxy from the waiting list if matched
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

// MAYBE ADD LATER
// if !i.ctx.CheckProxyRelayPattern(relayPattern, !relayPatternSupported) {
// 	log.Printf("bad request: rejected relay pattern from proxy = %v", messages.ErrBadRequest)
// 	b, err := messages.EncodePollResponseWithRelayURL("", false, "", "", "incorrect relay pattern")
// 	*response = b
// 	if err != nil {
// 		return messages.ErrInternal
// 	}
// 	return nil
// }

// maybe figure this out later
// bridgeFingerprint, err := bridgefingerprint.FingerprintFromBytes(offer.fingerprint)
// if err != nil {
// 	return messages.ErrBadRequest
// }
// if info, err := i.ctx.bridgeList.GetBridgeInfo(bridgeFingerprint); err != nil {
// 	return err
// } else {
// 	relayURL = info.WebSocketAddress
// }
