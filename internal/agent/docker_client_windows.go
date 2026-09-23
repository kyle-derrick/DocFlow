//go:build windows

package agent

import (
	"context"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/Microsoft/go-winio"
)

func dockerHTTPClient() *http.Client {
	endpoint := os.Getenv("DOCKER_HOST")
	if endpoint == "" {
		endpoint = `\\.\pipe\docker_engine`
	}
	transport := &http.Transport{DisableKeepAlives: true, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return winio.DialPipe(endpoint, nil)
	}}
	return &http.Client{Transport: transport, Timeout: 30 * time.Second}
}
