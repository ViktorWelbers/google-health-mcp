package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

// Scopes we request. Read-only throughout — this server never writes health data.
var scopes = []string{
	"https://www.googleapis.com/auth/googlehealth.activity_and_fitness.readonly",
	"https://www.googleapis.com/auth/googlehealth.health_metrics_and_measurements.readonly",
	"https://www.googleapis.com/auth/googlehealth.sleep.readonly",
}

const callbackAddr = "127.0.0.1:3000"

// tokenPath returns where the OAuth token is persisted. In a cluster, point
// HEALTH_TOKEN_PATH at a mounted secret or PVC.
func tokenPath() string {
	if p := os.Getenv("HEALTH_TOKEN_PATH"); p != "" {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "token.json"
	}
	return filepath.Join(home, ".config", "health-mcp", "token.json")
}

func oauthConfig() (*oauth2.Config, error) {
	id := os.Getenv("GOOGLE_CLIENT_ID")
	secret := os.Getenv("GOOGLE_CLIENT_SECRET")
	if id == "" || secret == "" {
		return nil, fmt.Errorf("GOOGLE_CLIENT_ID and GOOGLE_CLIENT_SECRET must be set")
	}
	return &oauth2.Config{
		ClientID:     id,
		ClientSecret: secret,
		Endpoint:     google.Endpoint,
		RedirectURL:  "http://" + callbackAddr + "/callback",
		Scopes:       scopes,
	}, nil
}

func loadToken() (*oauth2.Token, error) {
	b, err := os.ReadFile(tokenPath())
	if err != nil {
		return nil, err
	}
	var t oauth2.Token
	if err := json.Unmarshal(b, &t); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", tokenPath(), err)
	}
	return &t, nil
}

func saveToken(t *oauth2.Token) error {
	p := tokenPath()
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return err
	}
	// 0600 — this file grants access to personal health data.
	return os.WriteFile(p, b, 0o600)
}

// persistingSource wraps a TokenSource and writes the token back to disk
// whenever it is refreshed, so a long-lived server keeps working.
type persistingSource struct {
	mu   sync.Mutex
	src  oauth2.TokenSource
	last string
}

func (p *persistingSource) Token() (*oauth2.Token, error) {
	t, err := p.src.Token()
	if err != nil {
		return nil, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if t.AccessToken != p.last {
		p.last = t.AccessToken
		if err := saveToken(t); err != nil {
			// Non-fatal: we still have a usable token in memory.
			fmt.Fprintf(os.Stderr, "warning: could not persist refreshed token: %v\n", err)
		}
	}
	return t, nil
}

// httpClient returns an HTTP client that automatically attaches and refreshes
// the OAuth token.
func httpClient(ctx context.Context) (*http.Client, error) {
	cfg, err := oauthConfig()
	if err != nil {
		return nil, err
	}
	tok, err := loadToken()
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("no token at %s — run `health-mcp login` first", tokenPath())
		}
		return nil, err
	}
	ps := &persistingSource{src: cfg.TokenSource(ctx, tok), last: tok.AccessToken}
	return oauth2.NewClient(ctx, ps), nil
}

// login runs the interactive OAuth flow against a local callback listener.
func login(ctx context.Context) error {
	cfg, err := oauthConfig()
	if err != nil {
		return err
	}

	state := fmt.Sprintf("%d", time.Now().UnixNano())
	codeCh := make(chan string, 1)
	errCh := make(chan error, 1)

	mux := http.NewServeMux()
	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("state"); got != state {
			http.Error(w, "state mismatch", http.StatusBadRequest)
			errCh <- fmt.Errorf("state mismatch — possible CSRF, aborting")
			return
		}
		if e := r.URL.Query().Get("error"); e != "" {
			http.Error(w, e, http.StatusBadRequest)
			errCh <- fmt.Errorf("authorization denied: %s", e)
			return
		}
		code := r.URL.Query().Get("code")
		if code == "" {
			http.Error(w, "no code", http.StatusBadRequest)
			errCh <- fmt.Errorf("no authorization code in callback")
			return
		}
		fmt.Fprintln(w, "Authorized. You can close this tab and return to the terminal.")
		codeCh <- code
	})

	ln, err := net.Listen("tcp", callbackAddr)
	if err != nil {
		return fmt.Errorf("cannot listen on %s (is something else using port 3000?): %w", callbackAddr, err)
	}
	srv := &http.Server{Handler: mux}
	go srv.Serve(ln)
	defer srv.Shutdown(context.Background())

	// AccessTypeOffline + prompt=consent guarantees we get a refresh token,
	// which is what lets the server keep running unattended.
	authURL := cfg.AuthCodeURL(state,
		oauth2.AccessTypeOffline,
		oauth2.SetAuthURLParam("prompt", "consent"),
	)

	fmt.Println("Opening your browser to authorize. If it doesn't open, visit:")
	fmt.Println()
	fmt.Println("  " + authURL)
	fmt.Println()
	openBrowser(authURL)

	var code string
	select {
	case code = <-codeCh:
	case err := <-errCh:
		return err
	case <-time.After(5 * time.Minute):
		return fmt.Errorf("timed out waiting for authorization")
	case <-ctx.Done():
		return ctx.Err()
	}

	tok, err := cfg.Exchange(ctx, code)
	if err != nil {
		return fmt.Errorf("exchanging code for token: %w", err)
	}
	if tok.RefreshToken == "" {
		fmt.Fprintln(os.Stderr, "warning: no refresh token returned — the server will stop working when the access token expires.")
	}
	if err := saveToken(tok); err != nil {
		return err
	}
	fmt.Printf("Token saved to %s (mode 0600).\n", tokenPath())
	return nil
}

func openBrowser(url string) {
	var cmd string
	switch runtime.GOOS {
	case "darwin":
		cmd = "open"
	case "windows":
		cmd = "explorer"
	default:
		cmd = "xdg-open"
	}
	_ = exec.Command(cmd, url).Start()
}
