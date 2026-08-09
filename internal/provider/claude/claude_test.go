package claude

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/janiorvalle/hop/internal/provider"
)

func TestFetchUsageSendsRequiredHeadersAndParsesLimits(t *testing.T) {
	t.Parallel()

	fixture, err := os.ReadFile("testdata/usage.json")
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if got := request.Header.Get("Authorization"); got != "Bearer access-token" {
			t.Errorf("Authorization = %q, want bearer token", got)
		}
		if got := request.Header.Get("anthropic-beta"); got != betaHeaderValue {
			t.Errorf("anthropic-beta = %q, want %q", got, betaHeaderValue)
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write(fixture)
	}))
	t.Cleanup(server.Close)

	usage, err := New(Config{UsageURL: server.URL}).FetchUsage(context.Background(), Credentials{AccessToken: "access-token"})
	if err != nil {
		t.Fatalf("FetchUsage() error = %v", err)
	}
	if usage.Provider != provider.Claude {
		t.Errorf("Provider = %q, want claude", usage.Provider)
	}
	if len(usage.Windows) != 2 {
		t.Fatalf("Windows length = %d, want 2", len(usage.Windows))
	}
	if len(usage.Limits) != 3 {
		t.Fatalf("Limits length = %d, want 3", len(usage.Limits))
	}
	if got := usage.Limits[0]; got.Scope != "" || !got.Active || got.UsedPercent != 35 {
		t.Errorf("first limit = %+v, want active session at 35%%", got)
	}
	if got := usage.Limits[2].Scope; got != `{"model":{"id":null,"display_name":"Fable"},"surface":null}` {
		t.Errorf("object scope = %q, want compact JSON", got)
	}
}

func TestFetchUsageAcceptsIdleWindowAndLimitWithoutReset(t *testing.T) {
	t.Parallel()

	fixture, err := os.ReadFile("testdata/usage_idle.json")
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write(fixture)
	}))
	t.Cleanup(server.Close)

	usage, err := New(Config{UsageURL: server.URL}).FetchUsage(context.Background(), Credentials{AccessToken: "access-token"})
	if err != nil {
		t.Fatalf("FetchUsage() error = %v", err)
	}
	if got := usage.Windows[0]; got.Kind != provider.FiveHour || got.UsedPercent != 0 || !got.ResetsAt.IsZero() {
		t.Fatalf("idle five-hour window = %+v, want 0%% with no reset", got)
	}
	if got := usage.Limits[0]; got.Kind != "session" || got.UsedPercent != 0 || !got.Active || !got.ResetsAt.IsZero() {
		t.Fatalf("idle session limit = %+v, want active 0%% with no reset", got)
	}
}

func TestFetchUsageRejectsMissingResetForActiveUsage(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name string
		body string
	}{
		{name: "used window", body: `{"five_hour":{"utilization":1,"resets_at":null}}`},
		{name: "used limit", body: `{"limits":[{"kind":"session","group":"session","percent":1,"resets_at":null,"is_active":false}]}`},
		{name: "used active limit", body: `{"limits":[{"kind":"session","group":"session","percent":1,"resets_at":null,"is_active":true}]}`},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(writer, testCase.body)
			}))
			t.Cleanup(server.Close)

			_, err := New(Config{UsageURL: server.URL}).FetchUsage(context.Background(), Credentials{AccessToken: "access-token"})
			if !errors.Is(err, ErrUsage) || !strings.Contains(err.Error(), "resets_at may be null") {
				t.Fatalf("FetchUsage() error = %v, want guarded null-reset error", err)
			}
		})
	}
}

func TestFetchUsageStatusMatchesTheRecoveryStep(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name       string
		statusCode int
		want       string
	}{
		{name: "authentication", statusCode: http.StatusUnauthorized, want: "hop login claude <account>"},
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

			_, err := New(Config{UsageURL: server.URL}).FetchUsage(context.Background(), Credentials{AccessToken: "access-token"})
			if !errors.Is(err, ErrUsage) || !strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("FetchUsage() error = %v, want %q recovery", err, testCase.want)
			}
		})
	}
}

func TestCredentialFetcherImplementsSharedContract(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(writer, `{"five_hour":{"utilization":1,"resets_at":"2026-08-08T06:00:00Z"}}`)
	}))
	t.Cleanup(server.Close)

	fetcher := New(Config{UsageURL: server.URL}).Fetcher(Credentials{AccessToken: "access"})
	usage, err := fetcher.FetchUsage(context.Background())
	if err != nil {
		t.Fatalf("FetchUsage() error = %v", err)
	}
	if usage.Provider != provider.Claude {
		t.Errorf("Provider = %q, want claude", usage.Provider)
	}
}

func TestFetchProfileIdentifiesTheBearerTokenOwner(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet {
			t.Errorf("method = %s, want GET", request.Method)
		}
		if got := request.Header.Get("Authorization"); got != "Bearer live-access" {
			t.Errorf("Authorization = %q, want live bearer token", got)
		}
		if got := request.Header.Get("anthropic-beta"); got != betaHeaderValue {
			t.Errorf("anthropic-beta = %q, want %q", got, betaHeaderValue)
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(writer, `{"account":{"uuid":"account-uuid","email":"owner@example.com"},"organization":{"uuid":"shared-org"}}`)
	}))
	t.Cleanup(server.Close)

	profile, err := New(Config{ProfileURL: server.URL}).FetchProfile(context.Background(), Credentials{AccessToken: "live-access"})
	if err != nil {
		t.Fatalf("FetchProfile() error = %v", err)
	}
	if profile.AccountUUID != "account-uuid" || profile.Email != "owner@example.com" {
		t.Fatalf("FetchProfile() = %#v, want the account identity", profile)
	}
}

func TestFetchProfileReturnsARecoverableErrorForAnUnusableResponse(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name       string
		statusCode int
		body       string
	}{
		{name: "invalid token", statusCode: http.StatusUnauthorized, body: `{"error":{"details":{"error_code":"token_invalid"}}}`},
		{name: "missing identity", statusCode: http.StatusOK, body: `{"account":{}}`},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				writer.WriteHeader(testCase.statusCode)
				_, _ = io.WriteString(writer, testCase.body)
			}))
			t.Cleanup(server.Close)

			_, err := New(Config{ProfileURL: server.URL}).FetchProfile(context.Background(), Credentials{AccessToken: "live-access"})
			if !errors.Is(err, ErrProfile) {
				t.Fatalf("FetchProfile() error = %v, want ErrProfile", err)
			}
		})
	}
}

func TestRefreshRotatesAndPersistsTokens(t *testing.T) {
	t.Parallel()

	fixedNow := time.Date(2026, 8, 8, 5, 0, 0, 0, time.UTC)
	store := &memoryStore{credentials: Credentials{AccessToken: "old-access", RefreshToken: "old-refresh", SubscriptionType: "pro"}}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", request.Method)
		}
		body, readErr := io.ReadAll(request.Body)
		if readErr != nil {
			t.Errorf("ReadAll() error = %v", readErr)
		}
		var got refreshRequest
		if unmarshalErr := json.Unmarshal(body, &got); unmarshalErr != nil {
			t.Errorf("Unmarshal() error = %v", unmarshalErr)
		}
		if got.GrantType != "refresh_token" || got.RefreshToken != "old-refresh" || got.ClientID != "test-client" {
			t.Errorf("refresh request = %+v, want complete grant", got)
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(writer, `{"access_token":"new-access","refresh_token":"new-refresh","expires_in":3600,"refresh_token_expires_in":7200}`)
	}))
	t.Cleanup(server.Close)

	got, err := New(Config{TokenURL: server.URL, ClientID: "test-client", Now: func() time.Time { return fixedNow }}).Refresh(context.Background(), store)
	if err != nil {
		t.Fatalf("Refresh() error = %v", err)
	}
	if got.AccessToken != "new-access" || got.RefreshToken != "new-refresh" {
		t.Errorf("Refresh() tokens = %q/%q, want rotated tokens", got.AccessToken, got.RefreshToken)
	}
	if got.SubscriptionType != "pro" {
		t.Errorf("SubscriptionType = %q, want preserved pro", got.SubscriptionType)
	}
	if got.ExpiresAt != fixedNow.Add(time.Hour).UnixMilli() {
		t.Errorf("ExpiresAt = %d, want one hour from fixed time", got.ExpiresAt)
	}
	if store.writes != 1 || store.credentials.RefreshToken != "new-refresh" {
		t.Errorf("store = %+v with %d writes, want persisted rotation", store.credentials, store.writes)
	}
}

func TestRefreshFailureDoesNotOverwriteSlot(t *testing.T) {
	t.Parallel()

	store := &memoryStore{credentials: Credentials{AccessToken: "old-access", RefreshToken: "old-refresh"}}
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

func TestRefreshWriteFailureReturnsRotatedRecoveryCopy(t *testing.T) {
	t.Parallel()

	store := &memoryStore{
		credentials: Credentials{AccessToken: "old-access", RefreshToken: "old-refresh"},
		writeErr:    errors.New("disk full"),
	}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(writer, `{"access_token":"new-access","refresh_token":"new-refresh","expires_in":3600}`)
	}))
	t.Cleanup(server.Close)

	got, err := New(Config{TokenURL: server.URL}).Refresh(context.Background(), store)
	if !errors.Is(err, ErrRefresh) {
		t.Fatalf("Refresh() error = %v, want ErrRefresh", err)
	}
	if got.AccessToken != "new-access" || got.RefreshToken != "new-refresh" {
		t.Fatalf("Refresh() recovery copy = %q/%q, want rotated tokens", got.AccessToken, got.RefreshToken)
	}
	if !strings.Contains(err.Error(), "returned credentials are the recovery copy") {
		t.Errorf("Refresh() error = %q, want recovery instruction", err)
	}
}

func TestFetchUsageRejectsEmptySuccessResponse(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(writer, `{"message":"temporarily unavailable"}`)
	}))
	t.Cleanup(server.Close)

	_, err := New(Config{UsageURL: server.URL}).FetchUsage(context.Background(), Credentials{AccessToken: "access"})
	if !errors.Is(err, ErrUsage) {
		t.Fatalf("FetchUsage() error = %v, want ErrUsage", err)
	}
}

func TestNeedsRefreshUsesSkew(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 8, 5, 0, 0, 0, time.UTC)
	credentials := Credentials{ExpiresAt: now.Add(4 * time.Minute).UnixMilli()}
	if !credentials.NeedsRefresh(now, 5*time.Minute) {
		t.Fatal("NeedsRefresh() = false, want true inside skew")
	}
	if credentials.NeedsRefresh(now, 3*time.Minute) {
		t.Fatal("NeedsRefresh() = true, want false outside skew")
	}
}

func TestCredentialsHasScopeMatchesAnExactGrant(t *testing.T) {
	t.Parallel()

	credentials := Credentials{Scopes: []string{"org:create_api_key", "user:profile"}}
	if !credentials.HasScope("user:profile") {
		t.Fatal("HasScope(user:profile) = false, want true")
	}
	if credentials.HasScope("profile") {
		t.Fatal("HasScope(profile) = true, want an exact-scope match")
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
