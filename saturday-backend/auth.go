package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	"google.golang.org/api/drive/v3"
)

// driveScopes is read-only on purpose: saturday-backend's whole job is
// consuming notes, never managing them. A leaked token under this scope
// can't write or delete anything in the user's Drive.
var driveScopes = []string{drive.DriveReadonlyScope}

// loadOAuthConfig reads the OAuth client secret downloaded from Google
// Cloud Console (an OAuth 2.0 Desktop-app client ID) and returns it as an
// oauth2.Config scoped to driveScopes.
func loadOAuthConfig(credentialsPath string) (*oauth2.Config, error) {
	data, err := os.ReadFile(credentialsPath)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", credentialsPath, err)
	}
	cfg, err := google.ConfigFromJSON(data, driveScopes...)
	if err != nil {
		return nil, fmt.Errorf("parse credentials: %w", err)
	}
	return cfg, nil
}

// loadToken reads a previously cached token from tokenPath. Returns an
// error if none exists yet — callers should point the user at --drive-login
// rather than trying to bootstrap consent from a background poll loop.
func loadToken(tokenPath string) (*oauth2.Token, error) {
	data, err := os.ReadFile(tokenPath)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w (run --drive-login first)", tokenPath, err)
	}
	var tok oauth2.Token
	if err := json.Unmarshal(data, &tok); err != nil {
		return nil, fmt.Errorf("parse token: %w", err)
	}
	return &tok, nil
}

// saveToken writes tok to tokenPath at 0600 — it carries a refresh token,
// which is a standing credential, not a transient one.
func saveToken(tokenPath string, tok *oauth2.Token) error {
	if err := os.MkdirAll(filepath.Dir(tokenPath), 0o755); err != nil {
		return err
	}
	data, err := json.Marshal(tok)
	if err != nil {
		return err
	}
	return os.WriteFile(tokenPath, data, 0o600)
}

// runLogin performs the one-time interactive OAuth consent: it briefly
// listens on localhost to catch Google's redirect (the "oob" copy-paste
// flow was deprecated in 2022 — loopback redirect is the current standard
// for installed apps), prints the consent URL for the user to open, waits
// for the authorization code, exchanges it for a token, and caches the
// token to tokenPath.
//
// AccessTypeOffline + ApprovalForce: without both, Google either omits the
// refresh token entirely or only issues one on the *first ever* consent for
// that client+account pair — forcing consent here guarantees this run
// actually gets one, so the backend can run headless indefinitely after.
func runLogin(cfg *oauth2.Config, tokenPath string) error {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("listen for OAuth redirect: %w", err)
	}
	defer ln.Close()
	cfg.RedirectURL = fmt.Sprintf("http://127.0.0.1:%d", ln.Addr().(*net.TCPAddr).Port)

	codeCh := make(chan string, 1)
	errCh := make(chan error, 1)
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		code := r.URL.Query().Get("code")
		if code == "" {
			errCh <- fmt.Errorf("no code in redirect: %s", r.URL.RawQuery)
			fmt.Fprintln(w, "Authorization failed — no code received. Check the terminal and try again.")
			return
		}
		fmt.Fprintln(w, "Authorization received — you can close this tab.")
		codeCh <- code
	})}
	go func() { _ = srv.Serve(ln) }()
	defer srv.Close()

	authURL := cfg.AuthCodeURL("saturday-backend", oauth2.AccessTypeOffline, oauth2.ApprovalForce)
	fmt.Println("Open this URL in a browser and approve access:")
	fmt.Println()
	fmt.Println(authURL)
	fmt.Println()
	fmt.Println("Waiting for authorization...")

	var code string
	select {
	case code = <-codeCh:
	case err := <-errCh:
		return err
	}

	tok, err := cfg.Exchange(context.Background(), code)
	if err != nil {
		return fmt.Errorf("exchange code: %w", err)
	}
	if tok.RefreshToken == "" {
		return fmt.Errorf("no refresh token in response — Google only issues one on first consent per client+account; revoke access at https://myaccount.google.com/permissions and try again")
	}
	if err := saveToken(tokenPath, tok); err != nil {
		return fmt.Errorf("save token: %w", err)
	}
	fmt.Printf("Token saved to %s. saturday-backend can now run headless.\n", tokenPath)
	return nil
}
