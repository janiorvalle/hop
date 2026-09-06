package claude

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type fakeBrowser struct {
	t        *testing.T
	answer   func(authorize url.Values, redirectURI string) url.Values
	opened   url.Values
	redirect string
}

func (browser *fakeBrowser) Open(rawURL string) error {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return err
	}
	browser.opened = parsed.Query()
	browser.redirect = parsed.Query().Get("redirect_uri")
	if browser.answer == nil {
		return nil
	}
	callback := browser.answer(browser.opened, browser.redirect)
	go func() {
		response, err := http.Get(browser.redirect + "?" + callback.Encode())
		if err != nil {
			browser.t.Errorf("callback GET error = %v", err)
			return
		}
		_ = response.Body.Close()
	}()
	return nil
}

func approve(authorize url.Values, _ string) url.Values {
	return url.Values{"code": {"granted-code"}, "state": {authorize.Get("state")}}
}

func TestLoginExchangesTheBrowserCodeForTokensAndIdentity(t *testing.T) {
	t.Parallel()

	var exchanged tokenRequest
	var exchangeHeaders http.Header
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		exchangeHeaders = request.Header.Clone()
		if err := json.NewDecoder(request.Body).Decode(&exchanged); err != nil {
			t.Errorf("decode exchange body: %v", err)
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(writer, `{"access_token":"new-access","refresh_token":"new-refresh","expires_in":28800,"refresh_token_expires_in":2540000,"token_type":"Bearer","scope":"user:profile user:inference","account":{"uuid":"account-uuid","email_address":"person@example.com"},"organization":{"uuid":"org-uuid","name":"Org"}}`)
	}))
	t.Cleanup(server.Close)

	now := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	browser := &fakeBrowser{t: t, answer: approve}
	enrollment, err := New(Config{TokenURL: server.URL, ProfileURL: unreachableProfileURL(t), Now: func() time.Time { return now }}).Login(context.Background(), Login{OpenBrowser: browser.Open})
	if err != nil {
		t.Fatalf("Login() error = %v", err)
	}

	if got := browser.opened.Get("client_id"); got != defaultClientID {
		t.Errorf("authorize client_id = %q, want %q", got, defaultClientID)
	}
	if got := browser.opened.Get("scope"); got != loginScopes {
		t.Errorf("authorize scope = %q, want %q", got, loginScopes)
	}
	if got := browser.opened.Get("code_challenge_method"); got != "S256" {
		t.Errorf("authorize code_challenge_method = %q, want S256", got)
	}
	if !strings.HasPrefix(browser.redirect, "http://localhost:") || !strings.HasSuffix(browser.redirect, "/callback") {
		t.Errorf("redirect_uri = %q, want http://localhost:<port>/callback", browser.redirect)
	}
	sum := sha256.Sum256([]byte(exchanged.CodeVerifier))
	if got := base64.RawURLEncoding.EncodeToString(sum[:]); got != browser.opened.Get("code_challenge") {
		t.Errorf("exchange code_verifier does not hash to the authorize code_challenge")
	}
	want := tokenRequest{GrantType: "authorization_code", Code: "granted-code", State: browser.opened.Get("state"), ClientID: defaultClientID, RedirectURI: browser.redirect, CodeVerifier: exchanged.CodeVerifier}
	if exchanged != want {
		t.Errorf("exchange body = %#v, want %#v", exchanged, want)
	}
	if got := exchangeHeaders.Get("anthropic-beta"); got != betaHeaderValue {
		t.Errorf("anthropic-beta = %q, want %q", got, betaHeaderValue)
	}
	wantCredentials := Credentials{
		AccessToken:           "new-access",
		RefreshToken:          "new-refresh",
		ExpiresAt:             now.Add(28800 * time.Second).UnixMilli(),
		RefreshTokenExpiresAt: now.Add(2540000 * time.Second).UnixMilli(),
		Scopes:                []string{"user:profile", "user:inference"},
	}
	if enrollment.Credentials.AccessToken != wantCredentials.AccessToken || enrollment.Credentials.RefreshToken != wantCredentials.RefreshToken || enrollment.Credentials.ExpiresAt != wantCredentials.ExpiresAt || enrollment.Credentials.RefreshTokenExpiresAt != wantCredentials.RefreshTokenExpiresAt || strings.Join(enrollment.Credentials.Scopes, " ") != strings.Join(wantCredentials.Scopes, " ") {
		t.Errorf("credentials = %#v, want %#v", enrollment.Credentials, wantCredentials)
	}
	if enrollment.Profile != (Profile{AccountUUID: "account-uuid", Email: "person@example.com"}) {
		t.Errorf("profile = %#v, want the account block", enrollment.Profile)
	}
}

func unreachableProfileURL(t *testing.T) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(server.Close)
	return server.URL
}

func TestLoginTakesThePlanFromTheProfileAndLeavesItBlankOtherwise(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name          string
		profileStatus int
		profileBody   string
		want          string
	}{
		{name: "max account", profileStatus: http.StatusOK, profileBody: `{"account":{"uuid":"account-uuid","email":"person@example.com","has_claude_max":true,"has_claude_pro":true}}`, want: "max"},
		{name: "profile without plan flags", profileStatus: http.StatusOK, profileBody: `{"account":{"uuid":"account-uuid","email":"person@example.com"}}`, want: ""},
		{name: "profile call fails", profileStatus: http.StatusInternalServerError, profileBody: `{"error":"boom"}`, want: ""},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			tokenServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(writer, `{"access_token":"new-access","refresh_token":"new-refresh","expires_in":28800,"scope":"user:profile","account":{"uuid":"account-uuid","email_address":"person@example.com"}}`)
			}))
			t.Cleanup(tokenServer.Close)
			var profileAuthorization string
			profileServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				profileAuthorization = request.Header.Get("Authorization")
				writer.WriteHeader(testCase.profileStatus)
				_, _ = io.WriteString(writer, testCase.profileBody)
			}))
			t.Cleanup(profileServer.Close)

			browser := &fakeBrowser{t: t, answer: approve}
			enrollment, err := New(Config{TokenURL: tokenServer.URL, ProfileURL: profileServer.URL}).Login(context.Background(), Login{OpenBrowser: browser.Open})
			if err != nil {
				t.Fatalf("Login() error = %v, want the sign-in to succeed regardless of the profile", err)
			}
			if profileAuthorization != "Bearer new-access" {
				t.Errorf("profile Authorization = %q, want the freshly exchanged access token", profileAuthorization)
			}
			if enrollment.Credentials.SubscriptionType != testCase.want {
				t.Errorf("SubscriptionType = %q, want %q", enrollment.Credentials.SubscriptionType, testCase.want)
			}
			if enrollment.Credentials.AccessToken != "new-access" || enrollment.Profile.Email != "person@example.com" {
				t.Errorf("enrollment = %#v, want the token response kept intact", enrollment)
			}
		})
	}
}

func TestLoginStopsWhenCanceledDuringTheProfileLookup(t *testing.T) {
	t.Parallel()

	tokenServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(writer, `{"access_token":"new-access","refresh_token":"new-refresh","expires_in":28800,"scope":"user:profile","account":{"uuid":"account-uuid","email_address":"person@example.com"}}`)
	}))
	t.Cleanup(tokenServer.Close)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	profileServer := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
		cancel()
		<-request.Context().Done()
	}))
	t.Cleanup(profileServer.Close)

	browser := &fakeBrowser{t: t, answer: approve}
	enrollment, err := New(Config{TokenURL: tokenServer.URL, ProfileURL: profileServer.URL}).Login(ctx, Login{OpenBrowser: browser.Open})
	if !errors.Is(err, ErrLogin) || !strings.Contains(err.Error(), "[CLAUDE_LOGIN_CANCELED]") {
		t.Fatalf("Login() error = %v, want the cancellation to stop the sign-in", err)
	}
	if enrollment.Credentials.AccessToken != "" {
		t.Fatalf("enrollment = %#v, want nothing handed back after a cancellation", enrollment)
	}
}

func TestLoginRefusesACallbackWhoseStateItDidNotIssue(t *testing.T) {
	t.Parallel()

	var exchanges atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { exchanges.Add(1) }))
	t.Cleanup(server.Close)

	browser := &fakeBrowser{t: t, answer: func(url.Values, string) url.Values {
		return url.Values{"code": {"granted-code"}, "state": {"forged"}}
	}}
	_, err := New(Config{TokenURL: server.URL}).Login(context.Background(), Login{OpenBrowser: browser.Open})
	if err == nil || !strings.Contains(err.Error(), "[CLAUDE_LOGIN_STATE_MISMATCH]") || !errors.Is(err, ErrLogin) {
		t.Fatalf("Login() error = %v, want state mismatch refusal", err)
	}
	if exchanges.Load() != 0 {
		t.Fatalf("exchanges = %d, want none after a state mismatch", exchanges.Load())
	}
}

func TestLoginReportsADeniedAuthorization(t *testing.T) {
	t.Parallel()

	browser := &fakeBrowser{t: t, answer: func(authorize url.Values, _ string) url.Values {
		return url.Values{"error": {"access_denied"}, "error_description": {"user said no"}, "state": {authorize.Get("state")}}
	}}
	_, err := New(Config{}).Login(context.Background(), Login{OpenBrowser: browser.Open})
	if err == nil || !strings.Contains(err.Error(), "[CLAUDE_LOGIN_DENIED]") || !strings.Contains(err.Error(), "access_denied") {
		t.Fatalf("Login() error = %v, want denied authorization", err)
	}
}

func TestLoginReportsAFailedExchange(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name   string
		status int
		body   string
		code   string
	}{
		{name: "rejected", status: http.StatusBadRequest, body: `{"error":"invalid_grant"}`, code: "[CLAUDE_LOGIN_EXCHANGE_FAILED]"},
		{name: "unreadable", status: http.StatusOK, body: `not json`, code: "[CLAUDE_LOGIN_RESPONSE_INVALID]"},
		{name: "missing tokens", status: http.StatusOK, body: `{"access_token":"a","expires_in":10}`, code: "[CLAUDE_LOGIN_RESPONSE_INVALID]"},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				writer.WriteHeader(testCase.status)
				_, _ = io.WriteString(writer, testCase.body)
			}))
			t.Cleanup(server.Close)

			browser := &fakeBrowser{t: t, answer: approve}
			_, err := New(Config{TokenURL: server.URL}).Login(context.Background(), Login{OpenBrowser: browser.Open})
			if err == nil || !strings.Contains(err.Error(), testCase.code) || !errors.Is(err, ErrLogin) {
				t.Fatalf("Login() error = %v, want %s", err, testCase.code)
			}
		})
	}
}

func TestLoginRefusesAPortAlreadyInUse(t *testing.T) {
	t.Parallel()

	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("occupy a port: %v", err)
	}
	t.Cleanup(func() { _ = occupied.Close() })

	browser := &fakeBrowser{t: t}
	_, err = New(Config{}).Login(context.Background(), Login{Port: occupied.Addr().(*net.TCPAddr).Port, OpenBrowser: browser.Open})
	if !errors.Is(err, ErrCallbackPort) || !strings.Contains(err.Error(), "[CLAUDE_LOGIN_PORT_IN_USE]") {
		t.Fatalf("Login() error = %v, want port in use", err)
	}
	if browser.opened != nil {
		t.Fatal("browser opened although the callback port was unavailable")
	}
}

func TestLoginStopsWhenTheContextEnds(t *testing.T) {
	t.Parallel()

	timedOut, cancelTimeout := context.WithTimeout(context.Background(), 20*time.Millisecond)
	t.Cleanup(cancelTimeout)
	canceled, cancel := context.WithCancel(context.Background())
	cancel()

	testCases := []struct {
		name string
		ctx  context.Context
		code string
	}{
		{name: "timeout", ctx: timedOut, code: "[CLAUDE_LOGIN_TIMEOUT]"},
		{name: "canceled", ctx: canceled, code: "[CLAUDE_LOGIN_CANCELED]"},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			browser := &fakeBrowser{t: t}
			_, err := New(Config{}).Login(testCase.ctx, Login{OpenBrowser: browser.Open})
			if err == nil || !strings.Contains(err.Error(), testCase.code) {
				t.Fatalf("Login() error = %v, want %s", err, testCase.code)
			}
		})
	}
}

func TestLoginReportsABrowserThatCouldNotOpen(t *testing.T) {
	t.Parallel()

	_, err := New(Config{}).Login(context.Background(), Login{OpenBrowser: func(string) error { return errors.New("no opener") }})
	if err == nil || !strings.Contains(err.Error(), "[CLAUDE_LOGIN_BROWSER]") || !strings.Contains(err.Error(), "no opener") {
		t.Fatalf("Login() error = %v, want browser failure", err)
	}
}

func TestLoginChecksTheStateBeforeTrustingAnErrorCallback(t *testing.T) {
	t.Parallel()

	browser := &fakeBrowser{t: t, answer: func(url.Values, string) url.Values {
		return url.Values{"error": {"access_denied"}, "error_description": {"\x1b[31mforged\x1b[0m"}}
	}}
	_, err := New(Config{}).Login(context.Background(), Login{OpenBrowser: browser.Open})
	if err == nil || !strings.Contains(err.Error(), "[CLAUDE_LOGIN_STATE_MISMATCH]") || strings.Contains(err.Error(), "\x1b") {
		t.Fatalf("Login() error = %q, want a state mismatch that echoes nothing from the callback", err)
	}
}

func TestLoginQuotesWhatAnthropicReportsInADeniedCallback(t *testing.T) {
	t.Parallel()

	browser := &fakeBrowser{t: t, answer: func(authorize url.Values, _ string) url.Values {
		return url.Values{"error": {"access_denied"}, "error_description": {"no\x1bthanks"}, "state": {authorize.Get("state")}}
	}}
	_, err := New(Config{}).Login(context.Background(), Login{OpenBrowser: browser.Open})
	if err == nil || strings.Contains(err.Error(), "\x1b") || !strings.Contains(err.Error(), `"no\x1bthanks"`) {
		t.Fatalf("Login() error = %q, want the description quoted with escapes", err)
	}
}
