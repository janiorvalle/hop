package cli

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
	if _, ok := accounts[1].Fetcher.(claudeLiveFetcher); !ok {
		t.Fatalf("claude/work fetcher = %T, want live read-only fetcher", accounts[1].Fetcher)
	}
	alphaFetcher, ok := accounts[0].Fetcher.(claudeSlotFetcher)
	if !ok || alphaFetcher.refreshAllowed {
		t.Fatalf("hand-seeded claude/alpha fetcher = %#v, want read-only slot fetcher", accounts[0].Fetcher)
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

func TestSlotAllowsRefreshOnlyForExplicitManagedMetadata(t *testing.T) {
	t.Parallel()

	slot := t.TempDir()
	allowed, err := slotAllowsRefresh(slot)
	if err != nil || allowed {
		t.Fatalf("missing metadata = %t, %v; want false, nil", allowed, err)
	}
	if err := os.WriteFile(filepath.Join(slot, slotMetadataFilename), []byte(`{"refresh_policy":"managed"}`), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	allowed, err = slotAllowsRefresh(slot)
	if err != nil || !allowed {
		t.Fatalf("managed metadata = %t, %v; want true, nil", allowed, err)
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
			fetcher := claudeSlotFetcher{adapter: adapter, store: store, refreshAllowed: refreshAllowed, now: time.Now}
			if err := fetcher.Prepare(context.Background()); err != nil {
				t.Fatalf("Prepare() error = %v", err)
			}
			if _, err := fetcher.FetchUsage(context.Background()); err != nil {
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
	adapter := codex.New(codex.Config{UsageURL: server.URL + "/usage", TokenURL: server.URL + "/token"})
	fetcher := codexSlotFetcher{adapter: adapter, store: store, refreshAllowed: true, now: time.Now}
	if err := fetcher.Prepare(context.Background()); err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	if _, err := fetcher.FetchUsage(context.Background()); err != nil {
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
	fetcher := claudeSlotFetcher{adapter: adapter, store: store, refreshAllowed: true, now: time.Now}

	if err := fetcher.Prepare(context.Background()); err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	if _, err := fetcher.FetchUsage(context.Background()); err != nil {
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
	fetcher := claudeSlotFetcher{adapter: adapter, store: store, refreshAllowed: true, now: time.Now}

	if err := fetcher.Prepare(context.Background()); err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	if _, err := fetcher.FetchUsage(context.Background()); err != nil {
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
	fetcher := claudeSlotFetcher{adapter: adapter, store: store, refreshAllowed: true, now: time.Now}
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
	fetcher := claudeSlotFetcher{adapter: adapter, store: store, refreshAllowed: true, now: time.Now}
	ctx, cancel := context.WithCancel(context.Background())
	type glanceResponse struct {
		document glanceDocument
		err      error
	}
	result := make(chan glanceResponse, 1)
	go func() {
		document, err := fetchGlance(ctx, staticCatalog{{Provider: provider.Claude, Name: "managed", Fetcher: fetcher}})
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
	fetcher := claudeSlotFetcher{adapter: adapter, store: store, refreshAllowed: true, now: time.Now}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	document, err := fetchGlance(ctx, staticCatalog{{Provider: provider.Claude, Name: "locked", Fetcher: fetcher}})
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
	adapter := codex.New(codex.Config{UsageURL: server.URL + "/usage", TokenURL: server.URL + "/token"})
	fetcher := codexSlotFetcher{adapter: adapter, store: store, refreshAllowed: true, now: time.Now}

	if err := fetcher.Prepare(context.Background()); err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	if _, err := fetcher.FetchUsage(context.Background()); err != nil {
		t.Fatalf("FetchUsage() error = %v", err)
	}
	if store.writes != 2 || store.credentials.RefreshToken != "rotated" {
		t.Fatalf("recovery writes = %d, rotated token saved = %t; want 2, true", store.writes, store.credentials.RefreshToken == "rotated")
	}
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

var _ provider.Fetcher = claudeSlotFetcher{}
var _ provider.Fetcher = codexSlotFetcher{}
