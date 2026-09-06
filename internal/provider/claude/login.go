package claude

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	defaultAuthorizeURL = "https://claude.ai/oauth/authorize"
	// DefaultLoginPort is the callback port Anthropic accepts for the public
	// client id. CLIProxyAPI listens on it too while it runs.
	DefaultLoginPort = 54545
	loginScopes      = "user:profile user:inference user:sessions:claude_code user:mcp_servers user:file_upload"
	callbackPath     = "/callback"
)

var (
	ErrLogin        = errors.New("claude login failed")
	ErrCallbackPort = errors.New("claude login callback port is unavailable")
)

// Login is one browser sign-in that hop owns end to end: it never reads or
// writes the live Claude Code login.
type Login struct {
	Port        int
	OpenBrowser func(url string) error
}

// Enrollment is what a finished sign-in hands the account slot.
type Enrollment struct {
	Credentials Credentials
	Profile     Profile
}

// Login signs a Claude account in through the user's browser and returns its
// tokens and identity without touching any credential store.
func (adapter Adapter) Login(ctx context.Context, login Login) (Enrollment, error) {
	verifier, challenge, err := newPKCEPair()
	if err != nil {
		return Enrollment{}, err
	}
	state, err := newState()
	if err != nil {
		return Enrollment{}, err
	}
	listener, err := listenForCallback(login.Port, state)
	if err != nil {
		return Enrollment{}, err
	}
	defer listener.Close()

	if err := login.OpenBrowser(authorizeURL(adapter.clientID, listener.RedirectURI(), challenge, state)); err != nil {
		return Enrollment{}, fmt.Errorf("[CLAUDE_LOGIN_BROWSER] The browser could not be opened: %w. Open the printed URL yourself, or set BROWSER to a command that opens URLs: %w", err, ErrLogin)
	}
	code, err := listener.Await(ctx)
	if err != nil {
		return Enrollment{}, err
	}
	enrollment, err := adapter.exchangeCode(ctx, codeExchange{code: code, verifier: verifier, redirectURI: listener.RedirectURI(), state: state})
	if err != nil {
		return Enrollment{}, err
	}
	enrollment.Credentials.SubscriptionType, err = adapter.subscriptionTypeOf(ctx, enrollment.Credentials)
	if err != nil {
		return Enrollment{}, err
	}
	return enrollment, nil
}

// subscriptionTypeOf asks the profile endpoint which plan the new tokens belong
// to. The plan only labels a row, so a profile call that fails leaves it blank
// instead of failing a sign-in whose tokens already work. A canceled or expired
// context is the user stopping the login, and that still stops it.
func (adapter Adapter) subscriptionTypeOf(ctx context.Context, credentials Credentials) (string, error) {
	profile, err := adapter.FetchProfile(ctx, credentials)
	if ctx.Err() != nil {
		return "", loginStopped(ctx.Err())
	}
	if err != nil {
		return "", nil
	}
	return profile.SubscriptionType, nil
}

func newPKCEPair() (verifier, challenge string, err error) {
	raw := make([]byte, 96)
	if _, err := rand.Read(raw); err != nil {
		return "", "", fmt.Errorf("[CLAUDE_LOGIN_RANDOM] The system random source failed while preparing the sign-in: %w. Retry the login: %w", err, ErrLogin)
	}
	verifier = base64.RawURLEncoding.EncodeToString(raw)
	sum := sha256.Sum256([]byte(verifier))
	return verifier, base64.RawURLEncoding.EncodeToString(sum[:]), nil
}

func newState() (string, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("[CLAUDE_LOGIN_RANDOM] The system random source failed while preparing the sign-in: %w. Retry the login: %w", err, ErrLogin)
	}
	return hex.EncodeToString(raw), nil
}

func authorizeURL(clientID, redirectURI, challenge, state string) string {
	query := url.Values{
		"code":                  {"true"},
		"client_id":             {clientID},
		"response_type":         {"code"},
		"redirect_uri":          {redirectURI},
		"scope":                 {loginScopes},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
		"state":                 {state},
	}
	return defaultAuthorizeURL + "?" + query.Encode()
}

type callbackResult struct {
	code string
	err  error
}

type callbackListener struct {
	listener net.Listener
	server   *http.Server
	state    string
	results  chan callbackResult
}

func listenForCallback(port int, state string) (*callbackListener, error) {
	listener, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", fmt.Sprint(port)))
	if err != nil {
		return nil, fmt.Errorf("[CLAUDE_LOGIN_PORT_IN_USE] Port %d on 127.0.0.1 is taken, so the browser cannot hand the sign-in back to hop (CLIProxyAPI uses this port while it runs). Stop that program or choose another port, then retry: %w: %w", port, err, ErrCallbackPort)
	}
	callback := &callbackListener{listener: listener, state: state, results: make(chan callbackResult, 1)}
	mux := http.NewServeMux()
	mux.HandleFunc(callbackPath, callback.handle)
	callback.server = &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = callback.server.Serve(listener) }()
	return callback, nil
}

func (callback *callbackListener) RedirectURI() string {
	return fmt.Sprintf("http://localhost:%d%s", callback.listener.Addr().(*net.TCPAddr).Port, callbackPath)
}

func (callback *callbackListener) handle(writer http.ResponseWriter, request *http.Request) {
	query := request.URL.Query()
	result := callbackResult{code: query.Get("code")}
	switch {
	case query.Get("state") != callback.state:
		result.err = fmt.Errorf("[CLAUDE_LOGIN_STATE_MISMATCH] The browser callback carried a state hop did not issue, so the sign-in was discarded. Retry the login and finish it from the URL hop printed: %w", ErrLogin)
	case query.Get("error") != "":
		result.err = fmt.Errorf("[CLAUDE_LOGIN_DENIED] Anthropic did not complete the sign-in: %q (%q). Retry the login and approve the request in the browser: %w", query.Get("error"), query.Get("error_description"), ErrLogin)
	case result.code == "":
		result.err = fmt.Errorf("[CLAUDE_LOGIN_CALLBACK_INVALID] The browser callback carried no authorization code. Retry the login: %w", ErrLogin)
	}
	if result.err != nil {
		http.Error(writer, "hop could not accept this sign-in. Go back to the terminal for the reason.", http.StatusBadRequest)
	} else {
		writer.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(writer, "<!doctype html><title>hop</title><p>Signed in. You can close this tab and go back to the terminal.</p>")
	}
	select {
	case callback.results <- result:
	default:
	}
}

func (callback *callbackListener) Await(ctx context.Context) (string, error) {
	select {
	case result := <-callback.results:
		return result.code, result.err
	case <-ctx.Done():
		return "", loginStopped(ctx.Err())
	}
}

func loginStopped(cause error) error {
	if errors.Is(cause, context.DeadlineExceeded) {
		return fmt.Errorf("[CLAUDE_LOGIN_TIMEOUT] The browser sign-in did not finish in time. Retry the login and complete it in the browser: %w", ErrLogin)
	}
	return fmt.Errorf("[CLAUDE_LOGIN_CANCELED] The login was canceled before the browser sign-in finished. Retry when you are ready: %w", ErrLogin)
}

func (callback *callbackListener) Close() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = callback.server.Shutdown(ctx)
}

type codeExchange struct {
	code        string
	verifier    string
	redirectURI string
	state       string
}

func (adapter Adapter) exchangeCode(ctx context.Context, exchange codeExchange) (Enrollment, error) {
	payload, err := json.Marshal(tokenRequest{
		GrantType:    "authorization_code",
		Code:         exchange.code,
		State:        exchange.state,
		ClientID:     adapter.clientID,
		RedirectURI:  exchange.redirectURI,
		CodeVerifier: exchange.verifier,
	})
	if err != nil {
		return Enrollment{}, fmt.Errorf("encode Claude token request: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, adapter.tokenURL, bytes.NewReader(payload))
	if err != nil {
		return Enrollment{}, fmt.Errorf("build Claude token request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	request.Header.Set("anthropic-beta", betaHeaderValue)

	response, err := adapter.client.Do(request)
	if err != nil {
		return Enrollment{}, fmt.Errorf("[CLAUDE_LOGIN_NETWORK] Claude's token endpoint could not be reached: %w. Check the network and retry the login: %w", err, ErrLogin)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return Enrollment{}, fmt.Errorf("[CLAUDE_LOGIN_EXCHANGE_FAILED] Claude's token endpoint answered HTTP %d when hop traded the sign-in for tokens. Retry the login; if it keeps failing, update hop: %w", response.StatusCode, ErrLogin)
	}
	var token tokenResponse
	if err := json.NewDecoder(io.LimitReader(response.Body, responseLimit)).Decode(&token); err != nil {
		return Enrollment{}, fmt.Errorf("[CLAUDE_LOGIN_RESPONSE_INVALID] Claude's token endpoint returned an unreadable response: %w. Retry the login; if it keeps failing, update hop: %w", err, ErrLogin)
	}
	if token.AccessToken == "" || token.RefreshToken == "" || token.ExpiresIn <= 0 {
		return Enrollment{}, fmt.Errorf("[CLAUDE_LOGIN_RESPONSE_INVALID] Claude's token endpoint omitted the access token, refresh token, or expiry. Retry the login; if it keeps failing, update hop: %w", ErrLogin)
	}
	now := adapter.now()
	return Enrollment{
		Credentials: Credentials{
			AccessToken:           token.AccessToken,
			RefreshToken:          token.RefreshToken,
			ExpiresAt:             expiryMilli(now, token.ExpiresIn),
			RefreshTokenExpiresAt: expiryMilli(now, token.RefreshTokenExpiresIn),
			Scopes:                strings.Fields(token.Scope),
		},
		Profile: Profile{
			AccountUUID: strings.TrimSpace(token.Account.UUID),
			Email:       strings.TrimSpace(token.Account.EmailAddress),
		},
	}, nil
}

type tokenRequest struct {
	GrantType    string `json:"grant_type"`
	Code         string `json:"code"`
	State        string `json:"state"`
	ClientID     string `json:"client_id"`
	RedirectURI  string `json:"redirect_uri"`
	CodeVerifier string `json:"code_verifier"`
}

type tokenResponse struct {
	AccessToken           string `json:"access_token"`
	RefreshToken          string `json:"refresh_token"`
	ExpiresIn             int64  `json:"expires_in"`
	RefreshTokenExpiresIn int64  `json:"refresh_token_expires_in"`
	Scope                 string `json:"scope"`
	Account               struct {
		UUID         string `json:"uuid"`
		EmailAddress string `json:"email_address"`
	} `json:"account"`
}
