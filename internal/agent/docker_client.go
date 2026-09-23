//go:build !windows

package agent

import (
	"context"
	"net"
	"net/http"
	"os"
	"time"
)

func dockerHTTPClient() *http.Client {
	endpoint := os.Getenv("DOCKER_HOST")
	if endpoint == "" {
		endpoint = "/var/run/docker.sock"
	}
	transport := &http.Transport{DisableKeepAlives: true, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return dialDocker(ctx, endpoint)
	}}
	return &http.Client{Transport: transport, Timeout: 30 * time.Second}
}

func dialDocker(ctx context.Context, endpoint string) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(ctx, "unix", endpoint)
}
