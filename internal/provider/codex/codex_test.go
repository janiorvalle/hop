package codex

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
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

	usage, err := New(Config{UsageURL: server.URL, ResetCreditsURL: server.URL, Now: func() time.Time { return fixedNow }}).FetchUsage(context.Background(), Credentials{AccessToken: "access-token", AccountID: "account-id"})
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

	usage, err := New(Config{UsageURL: server.URL, ResetCreditsURL: server.URL}).FetchUsage(context.Background(), Credentials{AccessToken: "access", AccountID: "account"})
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

const usageWithCredits = `{
	"plan_type": "pro",
	"rate_limit": {"primary_window": {"used_percent": 45, "limit_window_seconds": 604800, "reset_after_seconds": 1200}},
	"rate_limit_reset_credits": {
		"available_count": 2,
		"credits": [
			{"id": "a", "status": "available", "reset_type": "codex_rate_limits", "granted_at": "2026-08-01T00:00:00Z", "expires_at": "2026-08-30T00:00:00Z"},
			{"id": "b", "status": "available", "reset_type": "codex_rate_limits", "granted_at": "2026-08-02T00:00:00Z", "expires_at": "2026-08-20T00:00:00Z"},
			{"id": "c", "status": "consumed", "reset_type": "codex_rate_limits", "granted_at": "2026-07-01T00:00:00Z", "expires_at": "2026-08-25T00:00:00Z"},
			{"id": "d", "status": "available", "reset_type": "other", "granted_at": "2026-07-01T00:00:00Z", "expires_at": "2026-08-10T00:00:00Z"}
		]
	}
}`

const usageWithoutCredits = `{
	"plan_type": "pro",
	"rate_limit": {"primary_window": {"used_percent": 45, "limit_window_seconds": 604800, "reset_after_seconds": 1200}}
}`

func TestFetchUsageReadsResetCreditsFromTheUsagePayload(t *testing.T) {
	t.Parallel()

	creditsCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/credits" {
			creditsCalls++
		}
		_, _ = io.WriteString(writer, usageWithCredits)
	}))
	t.Cleanup(server.Close)

	usage, err := New(Config{UsageURL: server.URL + "/usage", ResetCreditsURL: server.URL + "/credits"}).FetchUsage(context.Background(), Credentials{AccessToken: "access", AccountID: "account"})
	if err != nil {
		t.Fatalf("FetchUsage() error = %v", err)
	}
	if creditsCalls != 0 {
		t.Fatalf("credits endpoint called %d times, want none when the usage payload carries them", creditsCalls)
	}
	assertTwoAvailableCredits(t, usage)
}

func TestFetchUsageFallsBackToTheResetCreditsEndpoint(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/credits" {
			_, _ = io.WriteString(writer, usageWithoutCredits)
			return
		}
		if got := request.Header.Get("Authorization"); got != "Bearer access" {
			t.Errorf("Authorization = %q, want the usage bearer", got)
		}
		if got := request.Header.Get("chatgpt-account-id"); got != "account" {
			t.Errorf("chatgpt-account-id = %q, want account", got)
		}
		if got := request.Header.Get("OpenAI-Beta"); got != "codex-1" {
			t.Errorf("OpenAI-Beta = %q, want codex-1", got)
		}
		if got := request.Header.Get("Originator"); got != "Codex Desktop" {
			t.Errorf("Originator = %q, want Codex Desktop", got)
		}
		_, _ = io.WriteString(writer, `{
			"available_count": 2,
			"credits": [
				{"id": "a", "status": "available", "reset_type": "codex_rate_limits", "granted_at": "2026-08-01T00:00:00Z", "expires_at": "2026-08-30T00:00:00Z"},
				{"id": "b", "status": "available", "reset_type": "codex_rate_limits", "granted_at": "2026-08-02T00:00:00Z", "expires_at": "2026-08-20T00:00:00Z"}
			]
		}`)
	}))
	t.Cleanup(server.Close)

	usage, err := New(Config{UsageURL: server.URL + "/usage", ResetCreditsURL: server.URL + "/credits"}).FetchUsage(context.Background(), Credentials{AccessToken: "access", AccountID: "account"})
	if err != nil {
		t.Fatalf("FetchUsage() error = %v", err)
	}
	assertTwoAvailableCredits(t, usage)
}

func TestFetchUsageSurvivesAFailedResetCreditsCall(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name    string
		credits http.HandlerFunc
	}{
		{name: "server error", credits: func(writer http.ResponseWriter, _ *http.Request) { writer.WriteHeader(http.StatusInternalServerError) }},
		{name: "malformed body", credits: func(writer http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(writer, "not json") }},
		{name: "hangs past its timeout", credits: func(_ http.ResponseWriter, request *http.Request) { <-request.Context().Done() }},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if request.URL.Path == "/credits" {
					testCase.credits(writer, request)
					return
				}
				_, _ = io.WriteString(writer, usageWithoutCredits)
			}))
			t.Cleanup(server.Close)

			usage, err := New(Config{UsageURL: server.URL + "/usage", ResetCreditsURL: server.URL + "/credits"}).FetchUsage(context.Background(), Credentials{AccessToken: "access", AccountID: "account"})
			if err != nil {
				t.Fatalf("FetchUsage() error = %v, want usage without credits", err)
			}
			if len(usage.Windows) != 1 {
				t.Fatalf("Windows = %+v, want the weekly window kept", usage.Windows)
			}
			if usage.ResetCredits != nil {
				t.Fatalf("ResetCredits = %+v, want unknown credits left nil rather than reported as zero", usage.ResetCredits)
			}
		})
	}
}

func assertTwoAvailableCredits(t *testing.T, usage provider.Usage) {
	t.Helper()
	credits := usage.ResetCredits
	if credits == nil || credits.Count != 2 || len(credits.Credits) != 2 {
		t.Fatalf("ResetCredits = %+v, want count 2 with the two available codex credits", credits)
	}
	wantGranted := time.Date(2026, time.August, 1, 0, 0, 0, 0, time.UTC)
	wantExpires := time.Date(2026, time.August, 30, 0, 0, 0, 0, time.UTC)
	if !credits.Credits[0].GrantedAt.Equal(wantGranted) || !credits.Credits[0].ExpiresAt.Equal(wantExpires) {
		t.Errorf("first credit = %+v, want granted %s expiring %s", credits.Credits[0], wantGranted, wantExpires)
	}
	soonest, ok := credits.SoonestExpiry()
	if !ok || !soonest.Equal(time.Date(2026, time.August, 20, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("SoonestExpiry() = %s, %t, want the August 20 credit", soonest, ok)
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

func TestConsumeResetCreditSendsTheRedeemIDAsTheDesktopApp(t *testing.T) {
	t.Parallel()

	var gotPath, gotMethod, gotBody string
	gotHeaders := http.Header{}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		gotPath, gotMethod = request.URL.Path, request.Method
		body, _ := io.ReadAll(request.Body)
		gotBody = string(body)
		gotHeaders = request.Header.Clone()
		_, _ = io.WriteString(writer, `{}`)
	}))
	t.Cleanup(server.Close)

	err := New(Config{ResetCreditsURL: server.URL + "/credits"}).ConsumeResetCredit(context.Background(), Credentials{AccessToken: "access", AccountID: "account"}, "11111111-2222-4333-8444-555555555555")
	if err != nil {
		t.Fatalf("ConsumeResetCredit() error = %v", err)
	}
	if gotMethod != http.MethodPost || gotPath != "/credits/consume" {
		t.Fatalf("request = %s %s, want POST /credits/consume", gotMethod, gotPath)
	}
	if gotBody != `{"redeem_request_id":"11111111-2222-4333-8444-555555555555"}` {
		t.Fatalf("body = %s", gotBody)
	}
	for header, want := range map[string]string{
		"Authorization":      "Bearer access",
		"Chatgpt-Account-Id": "account",
		"Openai-Beta":        "codex-1",
		"Originator":         "Codex Desktop",
		"Content-Type":       "application/json",
	} {
		if got := gotHeaders.Get(header); got != want {
			t.Errorf("%s = %q, want %q", header, got, want)
		}
	}
}

func TestConsumeResetCreditExplainsRejections(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name   string
		status int
		want   string
	}{
		{name: "expired login", status: http.StatusUnauthorized, want: "hop login codex <account>"},
		{name: "rate limited", status: http.StatusTooManyRequests, want: "wait and retry"},
		{name: "outage", status: http.StatusBadGateway, want: "retry later"},
		{name: "unexpected", status: http.StatusConflict, want: "check 'hop ls'"},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				writer.WriteHeader(testCase.status)
			}))
			t.Cleanup(server.Close)

			err := New(Config{ResetCreditsURL: server.URL}).ConsumeResetCredit(context.Background(), Credentials{AccessToken: "access", AccountID: "account"}, "id")
			if !errors.Is(err, ErrReset) {
				t.Fatalf("ConsumeResetCredit() error = %v, want ErrReset", err)
			}
			if !strings.Contains(err.Error(), fmt.Sprintf("HTTP %d", testCase.status)) || !strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("ConsumeResetCredit() error = %v, want the status and %q", err, testCase.want)
			}
		})
	}
}

func TestConsumeResetCreditReportsAnUnreachableEndpoint(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	server.Close()

	err := New(Config{ResetCreditsURL: server.URL}).ConsumeResetCredit(context.Background(), Credentials{AccessToken: "access", AccountID: "account"}, "id")
	if !errors.Is(err, ErrReset) || !strings.Contains(err.Error(), "reach the Codex reset endpoint") {
		t.Fatalf("ConsumeResetCredit() error = %v, want an unreachable-endpoint ErrReset", err)
	}
}
