package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

var version = "0.1.0"

func main() {
	httpAddr := flag.String("http", "", "serve over streamable HTTP on this address (e.g. :8080); default is stdio")
	flag.Usage = usage
	// Allow a bare subcommand before flags: `health-mcp login`.
	args := os.Args[1:]
	cmd := "serve"
	if len(args) > 0 && len(args[0]) > 0 && args[0][0] != '-' {
		cmd = args[0]
		args = args[1:]
	}
	if err := flag.CommandLine.Parse(args); err != nil {
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	switch cmd {
	case "login":
		if err := login(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "login failed: %v\n", err)
			os.Exit(1)
		}
	case "serve":
		if err := serve(ctx, *httpAddr); err != nil {
			fmt.Fprintf(os.Stderr, "server failed: %v\n", err)
			os.Exit(1)
		}
	case "version":
		fmt.Println(version)
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintf(os.Stderr, `google-health-mcp %s — MCP server for the Google Health API v4

Usage:
  health-mcp login            authorise against Google (opens a browser)
  health-mcp serve            run the MCP server on stdio (default)
  health-mcp serve -http :8080  run over streamable HTTP, for cluster deployment
  health-mcp version

Environment:
  GOOGLE_CLIENT_ID       OAuth client id       (required)
  GOOGLE_CLIENT_SECRET   OAuth client secret   (required)
  HEALTH_TOKEN_PATH      token location        (default ~/.config/health-mcp/token.json)

`, version)
}

func newServer() *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{
		Name:    "google-health",
		Version: version,
	}, nil)
	a := &app{}
	a.register(s)
	return s
}

func serve(ctx context.Context, httpAddr string) error {
	if httpAddr == "" {
		// stdio: log to stderr only — stdout carries the JSON-RPC stream.
		log.SetOutput(os.Stderr)
		return newServer().Run(ctx, &mcp.StdioTransport{})
	}

	// One server instance reused across sessions; the SDK gives each connection
	// its own session on top of it.
	srv := newServer()
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil)

	mux := http.NewServeMux()
	mux.Handle("/mcp", handler)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, "ok")
	})
	// readyz additionally reports whether we hold a usable token, so a cluster
	// can surface "running but not authorised" instead of silently failing.
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		tok, err := loadToken()
		if err != nil || (!tok.Valid() && tok.RefreshToken == "") {
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprintln(w, "not authorised — run `health-mcp login` and mount the token")
			return
		}
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, "ready")
	})

	hs := &http.Server{
		Addr:              httpAddr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = hs.Shutdown(shutdownCtx)
	}()

	log.Printf("google-health-mcp %s listening on %s (endpoint /mcp)", version, httpAddr)
	if err := hs.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}
