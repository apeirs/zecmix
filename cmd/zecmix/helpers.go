package main

import (
	"context"
	"crypto/tls"
	"net/http"

	"google.golang.org/grpc/credentials"
	"google.golang.org/protobuf/proto"
)

var errClosed = http.ErrServerClosed

func newGet(ctx context.Context, url string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0")
	return req, nil
}

// insecureTLS is only used by the gateway's self-check against its own
// loopback listener with a self-signed certificate.
func insecureTLS() credentials.TransportCredentials {
	return credentials.NewTLS(&tls.Config{InsecureSkipVerify: true, NextProtos: []string{"h2"}})
}

func protoSize(m proto.Message) int { return proto.Size(m) }
