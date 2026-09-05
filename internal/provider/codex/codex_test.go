package codex

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/janiorvalle/hop/internal/provider"
)

func TestCredentialsNeedsRefreshUsesJWTExpiry(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.August, 8, 6, 0, 0, 0, time.UTC)
	encode := func(payload string) string {
		return "header." + base64.RawURLEncoding.EncodeToString([]byte(payload)) + ".signature"
	}
	testCases := []struct {
		name        string
		accessToken string
		want        bool
	}{
		{name: "expires within skew", accessToken: encode(`{"exp":1786169100}`), want: true},
		{name: "expires after skew", accessToken: encode(`{"exp":1786172400}`), want: false},
		{name: "opaque token", accessToken: "opaque", want: false},
		{name: "invalid payload", accessToken: "header.invalid.signature", want: false},
	}
	for _, testCase := range testCases {
		credentials := Credentials{AccessToken: testCase.accessToken}
		if got := credentials.NeedsRefresh(now, 10*time.Minute); got != testCase.want {
			t.Errorf("%s: NeedsRefresh() = %t, want %t", testCase.name, got, testCase.want)
		}
	}
}

func TestFetchUsageClassifiesWindowsByDurationAndParsesEmail(t *testing.T) {
	t.Parallel()

	fixture, err := os.ReadFile("testdata/usage.json")
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if got := request.Header.Get("Authorization"); got != "Bearer access-token" {
			t.Errorf("Authorization = %q, want bearer token", got)
		}
		if got := request.Header.Get("chatgpt-account-id"); got != "account-id" {
			t.Errorf("chatgpt-account-id = %q, want account-id", got)
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write(fixture)
	}))
	t.Cleanup(server.Close)
	fixedNow := time.Date(2026, 8, 8, 5, 0, 0, 0, time.UTC)

	usage, err := New(Config{UsageURL: server.URL, Now: func() time.Time { return fixedNow }}).FetchUsage(context.Background(), Credentials{AccessToken: "access-token", AccountID: "account-id"})
	if err != nil {
		t.Fatalf("FetchUsage() error = %v", err)
	}
	if usage.Provider != provider.Codex || usage.Plan != "pro" || usage.Email != "owner@example.com" {
		t.Errorf("Usage identity = %+v, want codex pro owner", usage)
	}
	if len(usage.Windows) != 1 || usage.Windows[0].Kind != provider.Weekly {
		t.Fatalf("Windows = %+v, want primary classified as weekly", usage.Windows)
	}
	if len(usage.Limits) != 2 {
		t.Fatalf("Limits length = %d, want two model windows", len(usage.Limits))
	}
	if got := usage.Limits[0]; got.Scope != "gpt-5-codex" || got.Kind != "model_five_hour" || !got.Active {
		t.Errorf("first model limit = %+v, want active five-hour limit", got)
	}
}

func TestFetchUsageStatusMatchesTheRecoveryStep(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name       string
		statusCode int
		want       string
	}{
		{name: "authentication", statusCode: http.StatusUnauthorized, want: "hop login codex <account>"},
		{name: "rate limit", statusCode: http.StatusTooManyRequests, want: "wait and retry 'hop ls'"},
		{name: "provider outage", statusCode: http.StatusServiceUnavailable, want: "usage service is unavailable"},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				writer.WriteHeader(testCase.statusCode)
			}))
			t.Cleanup(server.Close)

			_, err := New(Config{UsageURL: server.URL}).FetchUsage(context.Background(), Credentials{AccessToken: "access-token", AccountID: "account-id"})
			if !errors.Is(err, ErrUsage) || !strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("FetchUsage() error = %v, want %q recovery", err, testCase.want)
			}
		})
	}
}

func TestRefreshUsesFormGrantAndPersistsRotation(t *testing.T) {
	t.Parallel()

	fixedNow := time.Date(2026, 8, 8, 5, 0, 0, 0, time.UTC)
	store := &memoryStore{credentials: Credentials{AuthMode: "chatgpt", IDToken: "old-id", AccessToken: "old-access", RefreshToken: "old-refresh", AccountID: "account"}}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if got := request.Header.Get("Content-Type"); got != "application/x-www-form-urlencoded" {
			t.Errorf("Content-Type = %q, want form encoded", got)
		}
		body, readErr := io.ReadAll(request.Body)
		if readErr != nil {
			t.Errorf("ReadAll() error = %v", readErr)
		}
		form, parseErr := url.ParseQuery(string(body))
		if parseErr != nil {
			t.Errorf("ParseQuery() error = %v", parseErr)
		}
		if form.Get("grant_type") != "refresh_token" || form.Get("refresh_token") != "old-refresh" || form.Get("client_id") != "test-client" {
			t.Errorf("refresh form = %v, want complete grant", form)
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(writer, `{"id_token":"new-id","access_token":"new-access","refresh_token":"new-refresh"}`)
	}))
	t.Cleanup(server.Close)

	got, err := New(Config{TokenURL: server.URL, ClientID: "test-client", Now: func() time.Time { return fixedNow }}).Refresh(context.Background(), store)
	if err != nil {
		t.Fatalf("Refresh() error = %v", err)
	}
	if got.IDToken != "new-id" || got.AccessToken != "new-access" || got.RefreshToken != "new-refresh" || got.AccountID != "account" {
		t.Errorf("Refresh() = %+v, want rotated tokens with account preserved", got)
	}
	if got.LastRefresh != fixedNow.Format(time.RFC3339Nano) || store.writes != 1 {
		t.Errorf("last refresh/writes = %q/%d, want timestamp and one write", got.LastRefresh, store.writes)
	}
}

func TestRefreshFailureLeavesSlotUnchanged(t *testing.T) {
	t.Parallel()

	store := &memoryStore{credentials: Credentials{AccessToken: "old-access", RefreshToken: "old-refresh", AccountID: "account"}}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(server.Close)

	_, err := New(Config{TokenURL: server.URL}).Refresh(context.Background(), store)
	if !errors.Is(err, ErrRefresh) {
		t.Fatalf("Refresh() error = %v, want ErrRefresh", err)
	}
	if store.writes != 0 || store.credentials.RefreshToken != "old-refresh" {
		t.Errorf("failed refresh changed store: %+v, writes %d", store.credentials, store.writes)
	}
	if !strings.Contains(err.Error(), "slot was not changed") {
		t.Errorf("Refresh() error = %q, want retry state", err)
	}
}

func TestRefreshWriteFailureReturnsRecoveryCopy(t *testing.T) {
	t.Parallel()

	store := &memoryStore{credentials: Credentials{AccessToken: "old-access", RefreshToken: "old-refresh", AccountID: "account"}, writeErr: errors.New("disk full")}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(writer, `{"access_token":"new-access","refresh_token":"new-refresh"}`)
	}))
	t.Cleanup(server.Close)

	got, err := New(Config{TokenURL: server.URL}).Refresh(context.Background(), store)
	if !errors.Is(err, ErrRefresh) {
		t.Fatalf("Refresh() error = %v, want ErrRefresh", err)
	}
	if got.AccessToken != "new-access" || got.RefreshToken != "new-refresh" {
		t.Errorf("recovery copy = %q/%q, want rotated tokens", got.AccessToken, got.RefreshToken)
	}
}

func TestFetchUsageKeepsUnknownWindowsAsScopedLimits(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(writer, `{
			"plan_type": "pro",
			"rate_limit": {
				"primary_window": {"used_percent": 45, "limit_window_seconds": 604800, "reset_after_seconds": 1200},
				"secondary_window": {"used_percent": 20, "limit_window_seconds": 2592000, "reset_after_seconds": 86400}
			},
			"code_review_rate_limit": {
				"primary_window": {"used_percent": 10, "limit_window_seconds": 604800, "reset_after_seconds": 600}
			},
			"additional_rate_limits": [{
				"limit_name": "gpt-5-codex",
				"rate_limit": {"primary_window": {"used_percent": 5, "limit_window_seconds": 2592000, "reset_after_seconds": 600}}
			}]
		}`)
	}))
	t.Cleanup(server.Close)

	usage, err := New(Config{UsageURL: server.URL}).FetchUsage(context.Background(), Credentials{AccessToken: "access", AccountID: "account"})
	if err != nil {
		t.Fatalf("FetchUsage() error = %v", err)
	}
	if len(usage.Windows) != 1 || usage.Windows[0].Kind != provider.Weekly {
		t.Fatalf("Windows = %+v, want only the weekly window", usage.Windows)
	}
	if len(usage.Limits) != 3 {
		t.Fatalf("Limits = %+v, want monthly, code review, and model limits", usage.Limits)
	}
	monthly := usage.Limits[0]
	if monthly.Kind != "account_30d" || monthly.Group != "account" || monthly.Scope != "30d" || !monthly.Active || monthly.UsedPercent != 20 {
		t.Errorf("monthly limit = %+v, want active account_30d scoped 30d", monthly)
	}
	codeReview := usage.Limits[1]
	if codeReview.Kind != "code_review_weekly" || codeReview.Group != "code_review" || codeReview.Scope != "code review" || !codeReview.Active {
		t.Errorf("code review limit = %+v, want active code_review_weekly even below the account's weekly usage", codeReview)
	}
	model := usage.Limits[2]
	if model.Kind != "model_30d" || model.Scope != "gpt-5-codex" || model.Active {
		t.Errorf("model limit = %+v, want inactive model_30d below the account's monthly usage", model)
	}
}

func TestFetchUsageRejectsMalformedWindow(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name string
		body string
		want string
	}{
		{name: "empty window", body: `{"rate_limit":{"primary_window":{}}}`, want: "limit_window_seconds is missing"},
		{name: "missing reset", body: `{"rate_limit":{"primary_window":{"used_percent":1,"limit_window_seconds":18000}}}`, want: "reset_at and reset_after_seconds are missing"},
	}
	for _, testCase := range testCases {
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(writer, testCase.body)
		}))
		t.Cleanup(server.Close)

		_, err := New(Config{UsageURL: server.URL}).FetchUsage(context.Background(), Credentials{AccessToken: "access", AccountID: "account"})
		if !errors.Is(err, ErrUsage) || !strings.Contains(err.Error(), testCase.want) || !strings.Contains(err.Error(), "update hop") {
			t.Errorf("%s: FetchUsage() error = %v, want ErrUsage naming %q with a next step", testCase.name, err, testCase.want)
		}
	}
}

func TestDurationLabelUsesTheLargestWholeUnit(t *testing.T) {
	t.Parallel()

	testCases := map[int64]string{2_592_000: "30d", 86_400: "1d", 3_600: "1h", 5_400: "90m", 90: "90s"}
	for seconds, want := range testCases {
		if got := durationLabel(seconds); got != want {
			t.Errorf("durationLabel(%d) = %q, want %q", seconds, got, want)
		}
	}
}

type memoryStore struct {
	credentials Credentials
	writes      int
	writeErr    error
}

func (store *memoryStore) Read() (Credentials, error) {
	return store.credentials, nil
}

func (store *memoryStore) Write(credentials Credentials) error {
	if store.writeErr != nil {
		return store.writeErr
	}
	store.credentials = credentials
	store.writes++
	return nil
}
