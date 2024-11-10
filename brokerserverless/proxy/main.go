package main

import (
	"context"
	"fmt"
	"log"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"
	"gitlab.torproject.org/tpo/anti-censorship/pluggable-transports/snowflake/v2/common/messages"
)

func proxyHandler(ctx context.Context, request events.APIGatewayProxyRequest) (events.APIGatewayProxyResponse, error) {
	body := request.Body
	remoteAddr := request.RequestContext.Identity.SourceIP

	arg := messages.Arg{
		Body:       []byte(body),
		RemoteAddr: remoteAddr,
	}

	var response []byte
	err := handleProxyPolls(arg, &response)
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

func handleProxyPolls(arg messages.Arg, response *[]byte) error {
	sid, proxyType, natType, clients, relayPattern, relayPatternSupported, err := messages.DecodeProxyPollRequestWithRelayPrefix(arg.Body)
	if err != nil {
		return err
	}
	log.Printf("Received proxy poll request: sid=%s, proxyType=%s, natType=%s, clients=%d, relayPattern=%s, relayPatternSupported=%t", sid, proxyType, natType, clients, relayPattern, relayPatternSupported)
	*response = nil
	return nil
}

func main() {
	lambda.Start(proxyHandler)
}
