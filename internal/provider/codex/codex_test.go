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

func TestFetchUsageRejectsUnknownWindowDuration(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(writer, `{"rate_limit":{"primary_window":{"used_percent":1,"limit_window_seconds":3600,"reset_after_seconds":60}}}`)
	}))
	t.Cleanup(server.Close)

	_, err := New(Config{UsageURL: server.URL}).FetchUsage(context.Background(), Credentials{AccessToken: "access", AccountID: "account"})
	if !errors.Is(err, ErrUsage) || !strings.Contains(err.Error(), "3600-second") {
		t.Fatalf("FetchUsage() error = %v, want actionable unknown-window error", err)
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
