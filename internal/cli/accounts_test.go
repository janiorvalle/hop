package cli

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/janiorvalle/hop/internal/provider"
	"github.com/janiorvalle/hop/internal/provider/claude"
	"github.com/janiorvalle/hop/internal/provider/codex"
	"github.com/janiorvalle/hop/internal/state"
	"github.com/janiorvalle/hop/internal/vault"
)

func TestVaultCatalogDiscoversSortedAccountsAndUsesLiveSourceForActive(t *testing.T) {
	t.Parallel()

	accountVault, err := vault.New(t.TempDir())
	if err != nil {
		t.Fatalf("vault.New() error = %v", err)
	}
	for _, entry := range []struct{ provider, name string }{
		{provider: "codex", name: "zeta"},
		{provider: "claude", name: "work"},
		{provider: "claude", name: "alpha"},
	} {
		if _, err := accountVault.EnsureSlot(entry.provider, entry.name); err != nil {
			t.Fatalf("EnsureSlot(%s, %s) error = %v", entry.provider, entry.name, err)
		}
	}
	activeState := state.New()
	activeState.SetActive("claude", "work")
	catalog := vaultCatalog{
		vault:         accountVault,
		state:         activeState,
		claudeAdapter: claude.New(claude.Config{}),
		codexAdapter:  codex.New(codex.Config{}),
		now:           time.Now,
	}

	accounts, err := catalog.Accounts()
	if err != nil {
		t.Fatalf("Accounts() error = %v", err)
	}
	if len(accounts) != 3 {
		t.Fatalf("Accounts() length = %d, want 3", len(accounts))
	}
	gotOrder := fmt.Sprintf("%s/%s,%s/%s,%s/%s", accounts[0].Provider, accounts[0].Name, accounts[1].Provider, accounts[1].Name, accounts[2].Provider, accounts[2].Name)
	if gotOrder != "claude/alpha,claude/work,codex/zeta" {
		t.Fatalf("account order = %q", gotOrder)
	}
	if !accounts[1].Active {
		t.Fatal("claude/work active = false, want true")
	}
	if _, ok := accounts[1].Source.(claudeLiveSource); !ok {
		t.Fatalf("claude/work fetcher = %T, want live read-only source", accounts[1].Source)
	}
	alphaFetcher, ok := accounts[0].Source.(claudeSlotSource)
	if !ok || alphaFetcher.refreshAllowed {
		t.Fatalf("hand-seeded claude/alpha fetcher = %#v, want read-only slot source", accounts[0].Source)
	}
}

func TestDefaultVaultUsesHopHome(t *testing.T) {
	hopHome := filepath.Join(t.TempDir(), "hop-data")
	t.Setenv("HOP_HOME", hopHome)

	accountVault, err := defaultVault()
	if err != nil {
		t.Fatalf("defaultVault() error = %v", err)
	}
	if got := accountVault.Root(); got != hopHome {
		t.Fatalf("vault root = %q, want %q", got, hopHome)
	}
}

func TestShowAccountsJSONCarriesClaudePlanFromStoredTier(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte(`{"five_hour":{"utilization":25,"resets_at":"2026-08-08T08:00:00Z"}}`))
	}))
	t.Cleanup(server.Close)
	accountVault, err := vault.New(t.TempDir())
	if err != nil {
		t.Fatalf("vault.New() error = %v", err)
	}
	credentialsPath, err := accountVault.CredentialsPath("claude", "work")
	if err != nil {
		t.Fatalf("CredentialsPath() error = %v", err)
	}
	store := claude.FileStore{Path: credentialsPath}
	if err := store.Write(claude.Credentials{AccessToken: "access", SubscriptionType: "max", RateLimitTier: "default_claude_max_20x"}); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	catalog := vaultCatalog{
		vault:         accountVault,
		state:         state.New(),
		claudeAdapter: claude.New(claude.Config{UsageURL: server.URL}),
		codexAdapter:  codex.New(codex.Config{}),
		now:           time.Now,
	}

	var output bytes.Buffer
	if err := showAccountsFrom(context.Background(), &output, true, catalog, time.Now()); err != nil {
		t.Fatalf("showAccountsFrom() error = %v", err)
	}
	var document struct {
		Accounts []struct {
			Account string `json:"account"`
			Plan    string `json:"plan"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal(output.Bytes(), &document); err != nil {
		t.Fatalf("JSON output is invalid: %v\n%s", err, output.String())
	}
	if len(document.Accounts) != 1 || document.Accounts[0].Account != "work" || document.Accounts[0].Plan != "Max 20x" {
		t.Fatalf("accounts = %+v, want claude/work with plan Max 20x", document.Accounts)
	}
}

func TestLoadSlotMetadataTreatsMissingFileAsReadOnlySlot(t *testing.T) {
	t.Parallel()

	slot := t.TempDir()
	metadata, err := loadSlotMetadata(slot)
	if err != nil || metadata != (slotMetadata{}) {
		t.Fatalf("missing metadata = %+v, %v; want empty, nil", metadata, err)
	}
	if err := os.WriteFile(filepath.Join(slot, slotMetadataFilename), []byte(`{"refresh_policy":"managed","disabled":true}`), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	metadata, err = loadSlotMetadata(slot)
	if err != nil || metadata.RefreshPolicy != managedRefreshPolicy || !metadata.Disabled {
		t.Fatalf("managed metadata = %+v, %v; want managed and disabled", metadata, err)
	}
}

func TestClaudeSlotFetcherRefreshesOnlyManagedSlots(t *testing.T) {
	t.Parallel()

	for _, refreshAllowed := range []bool{false, true} {
		refreshAllowed := refreshAllowed
		t.Run(fmt.Sprintf("managed=%t", refreshAllowed), func(t *testing.T) {
			t.Parallel()
			var refreshCalls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				switch request.URL.Path {
				case "/token":
					refreshCalls.Add(1)
					_, _ = writer.Write([]byte(`{"access_token":"fresh","refresh_token":"rotated","expires_in":3600}`))
				case "/usage":
					_, _ = writer.Write([]byte(`{"five_hour":{"utilization":25,"resets_at":"2026-08-08T08:00:00Z"}}`))
				default:
					http.NotFound(writer, request)
				}
			}))
			t.Cleanup(server.Close)
			store := claude.FileStore{Path: filepath.Join(t.TempDir(), "credentials.json")}
			if err := store.Write(claude.Credentials{AccessToken: "old", RefreshToken: "refresh", ExpiresAt: 1}); err != nil {
				t.Fatalf("Write() error = %v", err)
			}
			adapter := claude.New(claude.Config{UsageURL: server.URL + "/usage", TokenURL: server.URL + "/token"})
			fetcher := claudeSlotSource{adapter: adapter, store: store, refreshAllowed: refreshAllowed, becameActive: neverActive, now: time.Now}
			if err := fetcher.Prepare(context.Background()); err != nil {
				t.Fatalf("Prepare() error = %v", err)
			}
			if _, err := fetchUsageFrom(fetcher); err != nil {
				t.Fatalf("FetchUsage() error = %v", err)
			}
			wantCalls := int32(0)
			if refreshAllowed {
				wantCalls = 1
			}
			if got := refreshCalls.Load(); got != wantCalls {
				t.Fatalf("refresh calls = %d, want %d", got, wantCalls)
			}
		})
	}
}

func TestCodexSlotFetcherRefreshesExpiredManagedJWT(t *testing.T) {
	t.Parallel()

	var refreshCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/token":
			refreshCalls.Add(1)
			_, _ = writer.Write([]byte(`{"access_token":"fresh","refresh_token":"rotated"}`))
		case "/usage":
			_, _ = writer.Write([]byte(`{"plan_type":"pro","email":"owner@example.com","rate_limit":{"primary_window":{"used_percent":10,"limit_window_seconds":18000,"reset_after_seconds":120}}}`))
		default:
			http.NotFound(writer, request)
		}
	}))
	t.Cleanup(server.Close)
	expiredPayload := base64.RawURLEncoding.EncodeToString([]byte(`{"exp":1}`))
	store := codex.FileStore{Path: filepath.Join(t.TempDir(), "credentials.json")}
	if err := store.Write(codex.Credentials{AccessToken: "header." + expiredPayload + ".signature", RefreshToken: "refresh", AccountID: "account"}); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	adapter := codex.New(codex.Config{UsageURL: server.URL + "/usage", ResetCreditsURL: server.URL + "/credits", TokenURL: server.URL + "/token"})
	fetcher := codexSlotSource{adapter: adapter, store: store, refreshAllowed: true, becameActive: neverActive, now: time.Now}
	if err := fetcher.Prepare(context.Background()); err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	if _, err := fetchUsageFrom(fetcher); err != nil {
		t.Fatalf("FetchUsage() error = %v", err)
	}
	if got := refreshCalls.Load(); got != 1 {
		t.Fatalf("refresh calls = %d, want 1", got)
	}
}

func TestClaudeSlotFetcherRetriesSavingRotatedRecoveryCredentials(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/token":
			_, _ = writer.Write([]byte(`{"access_token":"fresh","refresh_token":"rotated","expires_in":3600}`))
		case "/usage":
			_, _ = writer.Write([]byte(`{"five_hour":{"utilization":25,"resets_at":"2026-08-08T08:00:00Z"}}`))
		default:
			http.NotFound(writer, request)
		}
	}))
	t.Cleanup(server.Close)
	store := &failFirstClaudeWriteStore{credentials: claude.Credentials{AccessToken: "old", RefreshToken: "refresh", ExpiresAt: 1}}
	adapter := claude.New(claude.Config{UsageURL: server.URL + "/usage", TokenURL: server.URL + "/token"})
	fetcher := claudeSlotSource{adapter: adapter, store: store, refreshAllowed: true, becameActive: neverActive, now: time.Now}

	if err := fetcher.Prepare(context.Background()); err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	if _, err := fetchUsageFrom(fetcher); err != nil {
		t.Fatalf("FetchUsage() error = %v", err)
	}
	if store.writes != 2 || store.credentials.RefreshToken != "rotated" {
		t.Fatalf("recovery writes = %d, rotated token saved = %t; want 2, true", store.writes, store.credentials.RefreshToken == "rotated")
	}
}

func TestClaudeFileRefreshJournalsRotationWhenPrimaryInstallFails(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "slot", "credentials.json")
	store := claude.FileStore{Path: path}
	if err := store.Write(claude.Credentials{AccessToken: "old", RefreshToken: "refresh", ExpiresAt: 1}); err != nil {
		t.Fatalf("Write(old) error = %v", err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/token":
			if err := os.Remove(path); err != nil {
				t.Errorf("Remove(primary) error = %v", err)
			}
			if err := os.Mkdir(path, 0o700); err != nil {
				t.Errorf("Mkdir(primary collision) error = %v", err)
			}
			_, _ = writer.Write([]byte(`{"access_token":"fresh","refresh_token":"rotated","expires_in":3600}`))
		case "/usage":
			_, _ = writer.Write([]byte(`{"five_hour":{"utilization":25,"resets_at":"2026-08-08T08:00:00Z"}}`))
		default:
			http.NotFound(writer, request)
		}
	}))
	t.Cleanup(server.Close)
	adapter := claude.New(claude.Config{UsageURL: server.URL + "/usage", TokenURL: server.URL + "/token"})
	fetcher := claudeSlotSource{adapter: adapter, store: store, refreshAllowed: true, becameActive: neverActive, now: time.Now}

	if err := fetcher.Prepare(context.Background()); err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	if _, err := fetchUsageFrom(fetcher); err != nil {
		t.Fatalf("FetchUsage() error = %v", err)
	}
	recovered, err := store.Read()
	if err != nil {
		t.Fatalf("Read(recovery) error = %v", err)
	}
	if recovered.RefreshToken != "rotated" {
		t.Fatalf("rotated recovery token saved = false, want true")
	}
}

func TestConcurrentClaudeGlancesSerializeOneManagedSlotRefresh(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "slot", "credentials.json")
	store := claude.FileStore{Path: path}
	if err := store.Write(claude.Credentials{AccessToken: "old", RefreshToken: "refresh", ExpiresAt: 1}); err != nil {
		t.Fatalf("Write(old) error = %v", err)
	}
	var refreshCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/token":
			refreshCalls.Add(1)
			time.Sleep(100 * time.Millisecond)
			_, _ = writer.Write([]byte(`{"access_token":"fresh","refresh_token":"rotated","expires_in":3600}`))
		case "/usage":
			_, _ = writer.Write([]byte(`{"five_hour":{"utilization":25,"resets_at":"2026-08-08T08:00:00Z"}}`))
		default:
			http.NotFound(writer, request)
		}
	}))
	t.Cleanup(server.Close)
	adapter := claude.New(claude.Config{UsageURL: server.URL + "/usage", TokenURL: server.URL + "/token"})
	fetcher := claudeSlotSource{adapter: adapter, store: store, refreshAllowed: true, becameActive: neverActive, now: time.Now}
	start := make(chan struct{})
	results := make(chan error, 2)
	for range 2 {
		go func() {
			<-start
			results <- fetcher.Prepare(context.Background())
		}()
	}
	close(start)
	for range 2 {
		if err := <-results; err != nil {
			t.Fatalf("concurrent Prepare() error = %v", err)
		}
	}
	if got := refreshCalls.Load(); got != 1 {
		t.Fatalf("refresh calls = %d, want 1", got)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(path), ".refresh.lock")); !os.IsNotExist(err) {
		t.Fatalf("refresh lock remains after completion: %v", err)
	}
}

func TestManagedRefreshPersistsRotationAfterGlanceDeadline(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "slot", "credentials.json")
	store := claude.FileStore{Path: path}
	if err := store.Write(claude.Credentials{AccessToken: "old", RefreshToken: "refresh", ExpiresAt: 1}); err != nil {
		t.Fatalf("Write(old) error = %v", err)
	}
	tokenStarted := make(chan struct{})
	releaseToken := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/token":
			close(tokenStarted)
			<-releaseToken
			_, _ = writer.Write([]byte(`{"access_token":"fresh","refresh_token":"rotated","expires_in":3600}`))
		case "/usage":
			_, _ = writer.Write([]byte(`{"five_hour":{"utilization":25,"resets_at":"2026-08-08T08:00:00Z"}}`))
		default:
			http.NotFound(writer, request)
		}
	}))
	t.Cleanup(server.Close)
	adapter := claude.New(claude.Config{UsageURL: server.URL + "/usage", TokenURL: server.URL + "/token"})
	fetcher := claudeSlotSource{adapter: adapter, store: store, refreshAllowed: true, becameActive: neverActive, now: time.Now}
	ctx, cancel := context.WithCancel(context.Background())
	type glanceResponse struct {
		document glanceDocument
		err      error
	}
	result := make(chan glanceResponse, 1)
	go func() {
		document, err := fetchGlance(ctx, staticCatalog{{Provider: provider.Claude, Name: "managed", Source: fetcher}}, time.Now())
		result <- glanceResponse{document: document, err: err}
	}()
	<-tokenStarted
	cancel()
	close(releaseToken)

	response := <-result
	if response.err != nil {
		t.Fatalf("fetchGlance() error = %v", response.err)
	}
	if len(response.document.Accounts) != 1 || response.document.Accounts[0].Error == nil {
		t.Fatalf("glance accounts = %+v, want canceled usage error row", response.document.Accounts)
	}
	recovered, err := store.Read()
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if recovered.RefreshToken != "rotated" {
		t.Fatalf("rotated token persisted after glance deadline = false, want true")
	}
}

func TestManagedRefreshLockWaitHonorsGlanceDeadline(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "slot", "credentials.json")
	store := claude.FileStore{Path: path}
	if err := store.Write(claude.Credentials{AccessToken: "old", RefreshToken: "refresh", ExpiresAt: 1}); err != nil {
		t.Fatalf("Write(old) error = %v", err)
	}
	if err := os.Mkdir(filepath.Join(filepath.Dir(path), ".refresh.lock"), 0o700); err != nil {
		t.Fatalf("Mkdir(refresh lock) error = %v", err)
	}
	var refreshCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		refreshCalls.Add(1)
		_, _ = writer.Write([]byte(`{"access_token":"fresh","refresh_token":"rotated","expires_in":3600}`))
	}))
	t.Cleanup(server.Close)
	adapter := claude.New(claude.Config{UsageURL: server.URL, TokenURL: server.URL})
	fetcher := claudeSlotSource{adapter: adapter, store: store, refreshAllowed: true, becameActive: neverActive, now: time.Now}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	document, err := fetchGlance(ctx, staticCatalog{{Provider: provider.Claude, Name: "locked", Source: fetcher}}, time.Now())
	if err != nil {
		t.Fatalf("fetchGlance() error = %v", err)
	}
	if len(document.Accounts) != 1 || document.Accounts[0].Error == nil {
		t.Fatalf("glance accounts = %+v, want lock-wait error row", document.Accounts)
	}
	if got := refreshCalls.Load(); got != 0 {
		t.Fatalf("refresh calls = %d, want 0 before lock acquisition", got)
	}
}

func TestCodexSlotFetcherRetriesSavingRotatedRecoveryCredentials(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/token":
			_, _ = writer.Write([]byte(`{"access_token":"fresh","refresh_token":"rotated"}`))
		case "/usage":
			_, _ = writer.Write([]byte(`{"plan_type":"pro","email":"owner@example.com","rate_limit":{"primary_window":{"used_percent":10,"limit_window_seconds":18000,"reset_after_seconds":120}}}`))
		default:
			http.NotFound(writer, request)
		}
	}))
	t.Cleanup(server.Close)
	expiredPayload := base64.RawURLEncoding.EncodeToString([]byte(`{"exp":1}`))
	store := &failFirstCodexWriteStore{credentials: codex.Credentials{AccessToken: "header." + expiredPayload + ".signature", RefreshToken: "refresh", AccountID: "account"}}
	adapter := codex.New(codex.Config{UsageURL: server.URL + "/usage", ResetCreditsURL: server.URL + "/credits", TokenURL: server.URL + "/token"})
	fetcher := codexSlotSource{adapter: adapter, store: store, refreshAllowed: true, becameActive: neverActive, now: time.Now}

	if err := fetcher.Prepare(context.Background()); err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	if _, err := fetchUsageFrom(fetcher); err != nil {
		t.Fatalf("FetchUsage() error = %v", err)
	}
	if store.writes != 2 || store.credentials.RefreshToken != "rotated" {
		t.Fatalf("recovery writes = %d, rotated token saved = %t; want 2, true", store.writes, store.credentials.RefreshToken == "rotated")
	}
}

type unwritableClaudeStore struct {
	credentials claude.Credentials
}

func (store *unwritableClaudeStore) Read() (claude.Credentials, error) {
	return store.credentials, nil
}

func (store *unwritableClaudeStore) Write(claude.Credentials) error {
	return fmt.Errorf("injected write failure")
}

type failFirstClaudeWriteStore struct {
	credentials claude.Credentials
	writes      int
}

func (store *failFirstClaudeWriteStore) Read() (claude.Credentials, error) {
	return store.credentials, nil
}

func (store *failFirstClaudeWriteStore) Write(credentials claude.Credentials) error {
	store.writes++
	if store.writes == 1 {
		return fmt.Errorf("injected first write failure")
	}
	store.credentials = credentials
	return nil
}

type failFirstCodexWriteStore struct {
	credentials codex.Credentials
	writes      int
}

func (store *failFirstCodexWriteStore) Read() (codex.Credentials, error) {
	return store.credentials, nil
}

func (store *failFirstCodexWriteStore) Write(credentials codex.Credentials) error {
	store.writes++
	if store.writes == 1 {
		return fmt.Errorf("injected first write failure")
	}
	store.credentials = credentials
	return nil
}

var _ provider.Source = claudeSlotSource{}
var _ provider.Source = codexSlotSource{}

func TestClaudeSlotFetcherRotatesBeforeRefreshTokenExpires(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.September, 5, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	for _, scenario := range []struct {
		name           string
		refreshAllowed bool
		expiresIn      time.Duration
		wantCalls      int32
	}{
		{name: "managed six days out", refreshAllowed: true, expiresIn: 6 * 24 * time.Hour, wantCalls: 1},
		{name: "managed eight days out", refreshAllowed: true, expiresIn: 8 * 24 * time.Hour, wantCalls: 0},
		{name: "unmanaged six days out", refreshAllowed: false, expiresIn: 6 * 24 * time.Hour, wantCalls: 0},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			t.Parallel()
			var refreshCalls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				refreshCalls.Add(1)
				_, _ = writer.Write([]byte(`{"access_token":"fresh","refresh_token":"rotated","expires_in":3600,"refresh_token_expires_in":2592000}`))
			}))
			t.Cleanup(server.Close)
			store := claude.FileStore{Path: filepath.Join(t.TempDir(), "credentials.json")}
			seeded := claude.Credentials{
				AccessToken:           "valid",
				RefreshToken:          "refresh",
				ExpiresAt:             now.Add(time.Hour).UnixMilli(),
				RefreshTokenExpiresAt: now.Add(scenario.expiresIn).UnixMilli(),
			}
			if err := store.Write(seeded); err != nil {
				t.Fatalf("Write() error = %v", err)
			}
			adapter := claude.New(claude.Config{TokenURL: server.URL, Now: clock})
			fetcher := claudeSlotSource{adapter: adapter, store: store, refreshAllowed: scenario.refreshAllowed, becameActive: neverActive, now: clock}
			if err := fetcher.Prepare(context.Background()); err != nil {
				t.Fatalf("Prepare() error = %v", err)
			}
			if got := refreshCalls.Load(); got != scenario.wantCalls {
				t.Fatalf("refresh calls = %d, want %d", got, scenario.wantCalls)
			}
			stored, err := store.Read()
			if err != nil {
				t.Fatalf("Read() error = %v", err)
			}
			wantExpiry := seeded.RefreshTokenExpiresAt
			if scenario.wantCalls == 1 {
				wantExpiry = now.Add(30 * 24 * time.Hour).UnixMilli()
			}
			if stored.RefreshTokenExpiresAt != wantExpiry {
				t.Fatalf("stored refreshTokenExpiresAt = %d, want %d", stored.RefreshTokenExpiresAt, wantExpiry)
			}
		})
	}
}

func TestClaudeSlotFetcherKeepsUsableAccessTokenWhenEarlyRotationFails(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.September, 5, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	for _, scenario := range []struct {
		name            string
		accessExpiresIn time.Duration
		wantPrepareErr  bool
	}{
		{name: "access token still valid", accessExpiresIn: time.Hour, wantPrepareErr: false},
		{name: "access token expired", accessExpiresIn: -time.Hour, wantPrepareErr: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if request.URL.Path == "/usage" {
					_, _ = writer.Write([]byte(`{"five_hour":{"utilization":25,"resets_at":"2026-08-08T08:00:00Z"}}`))
					return
				}
				http.Error(writer, "token endpoint down", http.StatusBadGateway)
			}))
			t.Cleanup(server.Close)
			store := claude.FileStore{Path: filepath.Join(t.TempDir(), "credentials.json")}
			if err := store.Write(claude.Credentials{
				AccessToken:           "valid",
				RefreshToken:          "refresh",
				ExpiresAt:             now.Add(scenario.accessExpiresIn).UnixMilli(),
				RefreshTokenExpiresAt: now.Add(6 * 24 * time.Hour).UnixMilli(),
			}); err != nil {
				t.Fatalf("Write() error = %v", err)
			}
			adapter := claude.New(claude.Config{UsageURL: server.URL + "/usage", TokenURL: server.URL + "/token", Now: clock})
			fetcher := claudeSlotSource{adapter: adapter, store: store, refreshAllowed: true, becameActive: neverActive, now: clock}
			err := fetcher.Prepare(context.Background())
			if (err != nil) != scenario.wantPrepareErr {
				t.Fatalf("Prepare() error = %v, want error %t", err, scenario.wantPrepareErr)
			}
			if scenario.wantPrepareErr {
				return
			}
			opened, err := fetcher.Open(context.Background())
			if err != nil {
				t.Fatalf("Open() error = %v", err)
			}
			if _, err := opened.FetchUsage(context.Background()); err != nil {
				t.Fatalf("FetchUsage() error = %v", err)
			}
			if expiresAt := opened.Enrollment().RefreshTokenExpiresAt; !expiresAt.Equal(now.Add(6 * 24 * time.Hour)) {
				t.Fatalf("enrollment refresh token expiry = %s, want the unrotated six-day expiry", expiresAt)
			}
		})
	}
}

func TestClaudeSlotFetcherSurfacesRotatedTokensItCouldNotSave(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.September, 5, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte(`{"access_token":"fresh","refresh_token":"rotated","expires_in":3600,"refresh_token_expires_in":2592000}`))
	}))
	t.Cleanup(server.Close)
	store := &unwritableClaudeStore{credentials: claude.Credentials{
		AccessToken:           "valid",
		RefreshToken:          "refresh",
		ExpiresAt:             now.Add(time.Hour).UnixMilli(),
		RefreshTokenExpiresAt: now.Add(6 * 24 * time.Hour).UnixMilli(),
	}}
	adapter := claude.New(claude.Config{TokenURL: server.URL, Now: clock})
	fetcher := claudeSlotSource{adapter: adapter, store: store, refreshAllowed: true, becameActive: neverActive, now: clock}

	err := fetcher.Prepare(context.Background())
	if !errors.Is(err, errRotatedTokensLost) {
		t.Fatalf("Prepare() error = %v, want the lost-rotation error even though the access token is still valid", err)
	}
}

func TestCodexSlotFetcherRotatesWhenLastRefreshAgesOut(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.September, 5, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	for _, scenario := range []struct {
		name      string
		age       time.Duration
		wantCalls int32
	}{
		{name: "nine days old", age: 9 * 24 * time.Hour, wantCalls: 1},
		{name: "one day old", age: 24 * time.Hour, wantCalls: 0},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			t.Parallel()
			var refreshCalls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				refreshCalls.Add(1)
				_, _ = writer.Write([]byte(`{"access_token":"fresh","refresh_token":"rotated"}`))
			}))
			t.Cleanup(server.Close)
			validPayload := base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"exp":%d}`, now.Add(time.Hour).Unix())))
			store := codex.FileStore{Path: filepath.Join(t.TempDir(), "credentials.json")}
			lastRefresh := now.Add(-scenario.age).Format(time.RFC3339Nano)
			if err := store.Write(codex.Credentials{AccessToken: "header." + validPayload + ".signature", RefreshToken: "refresh", AccountID: "account", LastRefresh: lastRefresh}); err != nil {
				t.Fatalf("Write() error = %v", err)
			}
			adapter := codex.New(codex.Config{TokenURL: server.URL, Now: clock})
			fetcher := codexSlotSource{adapter: adapter, store: store, refreshAllowed: true, becameActive: neverActive, now: clock}
			if err := fetcher.Prepare(context.Background()); err != nil {
				t.Fatalf("Prepare() error = %v", err)
			}
			if got := refreshCalls.Load(); got != scenario.wantCalls {
				t.Fatalf("refresh calls = %d, want %d", got, scenario.wantCalls)
			}
			stored, err := store.Read()
			if err != nil {
				t.Fatalf("Read() error = %v", err)
			}
			wantLastRefresh := lastRefresh
			if scenario.wantCalls == 1 {
				wantLastRefresh = now.Format(time.RFC3339Nano)
			}
			if stored.LastRefresh != wantLastRefresh {
				t.Fatalf("stored last_refresh = %q, want %q", stored.LastRefresh, wantLastRefresh)
			}
		})
	}
}

func TestRotationSkipsSlotThatBecameActiveWhileWaitingForItsRefreshLock(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.September, 5, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	var refreshCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		refreshCalls.Add(1)
		_, _ = writer.Write([]byte(`{"access_token":"rotated-access","refresh_token":"rotated-refresh","expires_in":3600}`))
	}))
	t.Cleanup(server.Close)
	accountVault, err := vault.New(filepath.Join(t.TempDir(), "hop-home"))
	if err != nil {
		t.Fatalf("vault.New() error = %v", err)
	}
	writeClaudeSlot(t, accountVault, "old", claudeCredentials("old"))
	writeClaudeSlot(t, accountVault, "next", claude.Credentials{AccessToken: "next-access", RefreshToken: "next-refresh", ExpiresAt: now.Add(-time.Hour).UnixMilli()})
	recorded := state.New()
	recorded.SetActive("claude", "old")
	if err := recorded.Save(accountVault.Root()); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	accountCatalog := vaultCatalog{
		vault:         accountVault,
		state:         recorded,
		claudeAdapter: claude.New(claude.Config{TokenURL: server.URL, Now: clock}),
		codexAdapter:  codex.New(codex.Config{}),
		now:           clock,
	}
	rotator, ok := findAccount(t, accountCatalog, "claude", "next").Source.(slotRotator)
	if !ok {
		t.Fatal("idle managed slot is not rotatable")
	}

	claudeLive := &gatedClaudeKeychain{installing: make(chan struct{}), proceed: make(chan struct{})}
	claudeLive.credentials = claudeCredentials("old")
	manager := switchManager{
		vault:       accountVault,
		state:       fileActiveStateStore{root: accountVault.Root()},
		claudeLive:  claudeLive,
		codexLive:   &fakeCodexLiveStore{},
		claudeEmail: func(context.Context) (string, error) { return "owner@example.test", nil },
		claudeProfile: func(context.Context, claude.Credentials) (claude.Profile, error) {
			return claude.Profile{}, errors.New("profile unavailable")
		},
		stdout: &bytes.Buffer{},
	}
	switched := make(chan error, 1)
	go func() { switched <- manager.Switch(context.Background(), "claude", "next") }()
	<-claudeLive.installing

	rotated := make(chan rotationResult, 1)
	go func() {
		outcome, err := rotator.Rotate(context.Background())
		rotated <- rotationResult{outcome: outcome, err: err}
	}()
	select {
	case result := <-rotated:
		t.Fatalf("Rotate() = %+v, %v before the switch released the refresh lock", result.outcome, result.err)
	case <-time.After(200 * time.Millisecond):
	}
	close(claudeLive.proceed)
	if err := <-switched; err != nil {
		t.Fatalf("Switch() error = %v", err)
	}
	result := <-rotated
	t.Logf("Rotate() after the switch = %+v, %v", result.outcome, result.err)
	if result.err != nil || result.outcome.Outcome != rotationBecameActive {
		t.Fatalf("Rotate() = %+v, %v; want the became-active outcome", result.outcome, result.err)
	}
	if got := refreshCalls.Load(); got != 0 {
		t.Errorf("token server rotated the slot %d times after it became the live login", got)
	}
	slot, err := (claude.FileStore{Path: slotCredentialsPath(t, accountVault, "claude", "next")}).Read()
	if err != nil {
		t.Fatalf("Read(next) error = %v", err)
	}
	if slot.RefreshToken != claudeLive.credentials.RefreshToken {
		t.Fatalf("slot next holds refresh token %q but the live login holds %q, which the rotation invalidated", slot.RefreshToken, claudeLive.credentials.RefreshToken)
	}
}

func TestGlanceSilentlySkipsRotatingSlotThatBecameActive(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.September, 5, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	var refreshCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/token":
			refreshCalls.Add(1)
			_, _ = writer.Write([]byte(`{"access_token":"rotated-access","refresh_token":"rotated-refresh","expires_in":3600}`))
		case "/usage":
			_, _ = writer.Write([]byte(`{"five_hour":{"utilization":25,"resets_at":"2026-08-08T08:00:00Z"}}`))
		default:
			http.NotFound(writer, request)
		}
	}))
	t.Cleanup(server.Close)
	accountVault, err := vault.New(filepath.Join(t.TempDir(), "hop-home"))
	if err != nil {
		t.Fatalf("vault.New() error = %v", err)
	}
	seedClaudeSlot(t, accountVault, "next", claude.Credentials{AccessToken: "next-access", RefreshToken: "next-refresh", ExpiresAt: now.Add(-time.Hour).UnixMilli()}, managedRefreshPolicy)
	accountCatalog := vaultCatalog{
		vault:         accountVault,
		state:         state.New(),
		claudeAdapter: claude.New(claude.Config{UsageURL: server.URL + "/usage", TokenURL: server.URL + "/token", Now: clock}),
		codexAdapter:  codex.New(codex.Config{}),
		now:           clock,
	}
	before := readSlotFile(t, accountVault, "claude", "next")
	switchedState := state.New()
	switchedState.SetActive("claude", "next")
	if err := switchedState.Save(accountVault.Root()); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	document, err := fetchGlance(context.Background(), accountCatalog, now)
	if err != nil {
		t.Fatalf("fetchGlance() error = %v", err)
	}
	if len(document.Accounts) != 1 || document.Accounts[0].Error != nil {
		t.Fatalf("glance rows = %+v, want one row for next without an error", document.Accounts)
	}
	if got := refreshCalls.Load(); got != 0 {
		t.Errorf("token server rotated the slot %d times after it became the live login", got)
	}
	if after := readSlotFile(t, accountVault, "claude", "next"); !bytes.Equal(before, after) {
		t.Fatalf("slot next changed on disk after it became the live login:\n%s", after)
	}
}

type rotationResult struct {
	outcome rotation
	err     error
}

type gatedClaudeKeychain struct {
	fakeClaudeKeychain
	installing chan struct{}
	proceed    chan struct{}
	once       sync.Once
}

func (store *gatedClaudeKeychain) Write(ctx context.Context, credentials claude.Credentials) error {
	store.once.Do(func() { close(store.installing) })
	<-store.proceed
	return store.fakeClaudeKeychain.Write(ctx, credentials)
}

func findAccount(t *testing.T, accountCatalog catalog, providerName provider.Name, name string) account {
	t.Helper()
	accounts, err := accountCatalog.Accounts()
	if err != nil {
		t.Fatalf("Accounts() error = %v", err)
	}
	for _, current := range accounts {
		if current.Provider == providerName && current.Name == name {
			return current
		}
	}
	t.Fatalf("account %s %s is not in the catalog", providerName, name)
	return account{}
}

func neverActive() (bool, error) { return false, nil }

func fetchUsageFrom(source provider.Source) (provider.Usage, error) {
	fetcher, err := source.Open(context.Background())
	if err != nil {
		return provider.Usage{}, err
	}
	return fetcher.FetchUsage(context.Background())
}
