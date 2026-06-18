package transport

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"rtt-mcp-server/internal/config"
	"rtt-mcp-server/internal/rttcore"
	"rtt-mcp-server/internal/tools"
)

// defaultHost / defaultPort mirror the Python daemon.
const (
	defaultHost = "127.0.0.1"
	defaultPort = 8765
)

// RunDaemon is the single J-Link owner: it serves the RTT tools over SSE so any
// number of clients share one probe, and exposes POST /shutdown to release the
// probe and stop cleanly (the supported teardown path on Windows, where a hard
// kill would strand the probe).
func RunDaemon(ctx context.Context, host string, port int) error {
	cfg := config.Load()
	if host == "" {
		host = defaultHost
	}
	if port == 0 {
		port = defaultPort
	}

	srv := mcp.NewServer(impl(), nil)
	tools.Register(srv)

	sse := mcp.NewSSEHandler(func(*http.Request) *mcp.Server { return srv }, nil)

	mux := http.NewServeMux()
	// GET opens the SSE stream; POST (with ?sessionid=) delivers client messages.
	// The extension's liveness probe (GET /sse) and the bridge both hit this path.
	mux.Handle("/sse", sse)

	hs := &http.Server{
		Addr:              net.JoinHostPort(host, strconv.Itoa(port)),
		ReadHeaderTimeout: 5 * time.Second,
	}

	mux.HandleFunc("/shutdown", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
			return
		}
		// Release the probe first so a subsequent hard kill can't strand it.
		if c := rttcore.Get(); c.IsConnected() {
			c.Disconnect()
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "shutting down"})
		// Stop the server shortly after the response flushes.
		go func() {
			time.Sleep(100 * time.Millisecond)
			_ = hs.Shutdown(context.Background())
		}()
	})

	handler := http.Handler(mux)
	if cfg.AuthToken != "" {
		handler = authMiddleware(cfg.AuthToken, mux)
	}
	hs.Handler = handler

	// Graceful teardown on ctx cancellation or SIGINT/SIGTERM: release the probe.
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		if c := rttcore.Get(); c.IsConnected() {
			c.Disconnect()
		}
		_ = hs.Shutdown(context.Background())
	}()

	authNote := "(auth: OPEN)"
	if cfg.AuthToken != "" {
		authNote = "(auth: bearer token required)"
	}
	fmt.Fprintf(os.Stderr, "[rtt-mcp] SSE daemon on http://%s:%d/sse (shared J-Link owner) %s\n", host, port, authNote)
	fmt.Fprintln(os.Stderr, "[rtt-mcp] POST /shutdown to release the J-Link and stop cleanly.")

	if err := hs.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// authMiddleware gates every endpoint with a bearer token, matching the
// Python RTT_AUTH_TOKEN behaviour. Disabled (open) when the token is unset.
func authMiddleware(token string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "unauthorized"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

// authTransport returns an http.RoundTripper that injects the bearer token on
// every outbound request, or nil (default transport) when auth is disabled.
func authTransport(token string) http.RoundTripper {
	if token == "" {
		return http.DefaultTransport
	}
	return &bearerRoundTripper{token: token, base: http.DefaultTransport}
}

type bearerRoundTripper struct {
	token string
	base  http.RoundTripper
}

func (b *bearerRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	clone := r.Clone(r.Context())
	clone.Header.Set("Authorization", "Bearer "+b.token)
	return b.base.RoundTrip(clone)
}
