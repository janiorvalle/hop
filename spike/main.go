// Command spike proves whether hop can own the Claude PKCE login with the
// public Claude Code client id. It is throwaway: it never links into hop.
//
// It prints the authorize URL, waits for the callback on localhost, checks
// state, exchanges the code, and prints the token shape with every secret
// redacted to its first six characters plus its length.
package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"
)

const (
	// Same client id hop's adapter and CLIProxyAPI already use. Public, not
	// issued to hop.
	clientID = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"

	authorizeURL = "https://claude.ai/oauth/authorize"
	// hop refreshes here. CLIProxyAPI exchanges at
	// https://api.anthropic.com/v1/oauth/token; -token-url switches.
	hopTokenURL = "https://platform.claude.com/v1/oauth/token"
	betaHeader  = "oauth-2025-04-20"

	// CLIProxyAPI's scope list, verbatim.
	scopes = "user:profile user:inference user:sessions:claude_code user:mcp_servers user:file_upload"
)

// Only keys in this set are treated as non-secret and printed in full.
var plainKeys = map[string]bool{
	"token_type": true, "expires_in": true, "refresh_token_expires_in": true,
	"scope": true, "scopes": true, "uuid": true, "name": true,
	"email_address": true, "organization": true, "account": true,
	"subscription_type": true, "rate_limit_tier": true, "billing_type": true,
}

func main() {
	port := flag.Int("port", 54545, "localhost port for the callback (CLIProxyAPI uses 54545)")
	tokenURL := flag.String("token-url", hopTokenURL, "token endpoint for the code exchange")
	timeout := flag.Duration("timeout", 10*time.Minute, "how long to wait for the callback")
	flag.Parse()

	if err := run(*port, *tokenURL, *timeout); err != nil {
		fmt.Fprintln(os.Stderr, "spike failed:", err)
		os.Exit(1)
	}
}

func run(port int, tokenURL string, timeout time.Duration) error {
	verifier, challenge, err := pkce()
	if err != nil {
		return err
	}
	state, err := randomHex(16)
	if err != nil {
		return err
	}
	redirectURI := fmt.Sprintf("http://localhost:%d/callback", port)

	listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return fmt.Errorf("listen on %s (is another login waiting on this port?): %w", redirectURI, err)
	}

	authURL := buildAuthorizeURL(redirectURI, challenge, state)

	fmt.Println("spike-claude-pkce")
	fmt.Println("client_id:     ", clientID)
	fmt.Println("redirect_uri:  ", redirectURI)
	fmt.Println("scopes:        ", scopes)
	fmt.Println("token_url:     ", tokenURL)
	fmt.Println("code_challenge:", redact(challenge))
	fmt.Println("state:         ", redact(state))
	fmt.Println()
	fmt.Println("Open this URL in a browser and sign in:")
	fmt.Println(authURL)
	fmt.Println()
	fmt.Printf("Waiting up to %s for the callback on %s ...\n", timeout, redirectURI)

	code, err := waitForCallback(listener, state, timeout)
	if err != nil {
		return err
	}
	fmt.Println("callback received: state matched, code", redact(code))

	body, status, err := exchange(tokenURL, code, verifier, redirectURI, state)
	if err != nil {
		return err
	}
	fmt.Println("token endpoint HTTP", status)
	if status != http.StatusOK {
		fmt.Println("response:", string(body))
		return errors.New("code exchange failed")
	}
	return printTokenShape(body)
}

func pkce() (verifier, challenge string, err error) {
	raw := make([]byte, 96)
	if _, err := rand.Read(raw); err != nil {
		return "", "", fmt.Errorf("generate PKCE verifier: %w", err)
	}
	verifier = base64.RawURLEncoding.EncodeToString(raw)
	sum := sha256.Sum256([]byte(verifier))
	challenge = base64.RawURLEncoding.EncodeToString(sum[:])
	return verifier, challenge, nil
}

func randomHex(n int) (string, error) {
	raw := make([]byte, n)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate state: %w", err)
	}
	return hex.EncodeToString(raw), nil
}

func buildAuthorizeURL(redirectURI, challenge, state string) string {
	params := url.Values{
		"code":                  {"true"},
		"client_id":             {clientID},
		"response_type":         {"code"},
		"redirect_uri":          {redirectURI},
		"scope":                 {scopes},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
		"state":                 {state},
	}
	return authorizeURL + "?" + params.Encode()
}

type callback struct {
	code string
	err  error
}

func waitForCallback(listener net.Listener, expectedState string, timeout time.Duration) (string, error) {
	results := make(chan callback, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		if oauthErr := query.Get("error"); oauthErr != "" {
			http.Error(w, "Anthropic returned an error. You can close this tab.", http.StatusBadRequest)
			results <- callback{err: fmt.Errorf("authorize returned error=%q description=%q", oauthErr, query.Get("error_description"))}
			return
		}
		if query.Get("state") != expectedState {
			http.Error(w, "State mismatch. You can close this tab.", http.StatusBadRequest)
			results <- callback{err: errors.New("state mismatch: callback state does not match the one sent")}
			return
		}
		code := query.Get("code")
		if code == "" {
			http.Error(w, "No code in callback. You can close this tab.", http.StatusBadRequest)
			results <- callback{err: errors.New("callback carried no code")}
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, "<!doctype html><title>hop spike</title><p>Signed in. You can close this tab and return to the terminal.</p>")
		results <- callback{code: code}
	})
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = server.Serve(listener) }()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = server.Shutdown(ctx)
	}()

	select {
	case result := <-results:
		return result.code, result.err
	case <-time.After(timeout):
		return "", errors.New("timed out waiting for the callback")
	}
}

func exchange(tokenURL, code, verifier, redirectURI, state string) ([]byte, int, error) {
	// CLIProxyAPI splits a "code#state" pair if the provider sends one.
	code, _, _ = strings.Cut(code, "#")
	payload, err := json.Marshal(map[string]string{
		"grant_type":    "authorization_code",
		"code":          code,
		"state":         state,
		"client_id":     clientID,
		"redirect_uri":  redirectURI,
		"code_verifier": verifier,
	})
	if err != nil {
		return nil, 0, fmt.Errorf("encode exchange request: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(string(payload)))
	if err != nil {
		return nil, 0, fmt.Errorf("build exchange request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	request.Header.Set("anthropic-beta", betaHeader)

	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return nil, 0, fmt.Errorf("reach token endpoint: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return nil, response.StatusCode, fmt.Errorf("read token response: %w", err)
	}
	return body, response.StatusCode, nil
}

func printTokenShape(body []byte) error {
	var fields map[string]any
	if err := json.Unmarshal(body, &fields); err != nil {
		return fmt.Errorf("token response is not a JSON object: %w", err)
	}
	fmt.Println("token shape (secrets redacted to first 6 chars + length):")
	printFields("  ", fields)

	now := time.Now()
	if seconds, ok := fields["expires_in"].(float64); ok {
		fmt.Printf("access token expires at %s (%s from now)\n", now.Add(time.Duration(seconds)*time.Second).Format(time.RFC3339), time.Duration(seconds)*time.Second)
	} else {
		fmt.Println("expires_in: absent")
	}
	if seconds, ok := fields["refresh_token_expires_in"].(float64); ok {
		fmt.Printf("refresh token expires at %s (%s from now)\n", now.Add(time.Duration(seconds)*time.Second).Format(time.RFC3339), time.Duration(seconds)*time.Second)
	} else {
		fmt.Println("refresh_token_expires_in: absent")
	}
	if scope, ok := fields["scope"]; ok {
		fmt.Println("scope granted:", scope)
	} else {
		fmt.Println("scope: absent from token response")
	}
	return nil
}

func printFields(indent string, fields map[string]any) {
	keys := make([]string, 0, len(fields))
	for key := range fields {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		switch value := fields[key].(type) {
		case map[string]any:
			fmt.Printf("%s%s:\n", indent, key)
			printFields(indent+"  ", value)
		case string:
			if plainKeys[key] {
				fmt.Printf("%s%s: %q\n", indent, key, value)
			} else {
				fmt.Printf("%s%s: %s\n", indent, key, redact(value))
			}
		default:
			fmt.Printf("%s%s: %v\n", indent, key, value)
		}
	}
}

func redact(secret string) string {
	if len(secret) <= 6 {
		return fmt.Sprintf("<redacted len=%d>", len(secret))
	}
	return fmt.Sprintf("%s… (len=%d)", secret[:6], len(secret))
}
