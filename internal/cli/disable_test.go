package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/janiorvalle/hop/internal/provider"
	"github.com/janiorvalle/hop/internal/provider/claude"
	"github.com/janiorvalle/hop/internal/provider/codex"
	"github.com/janiorvalle/hop/internal/state"
)

func TestDisableMarksSlotAndEnableRestoresIt(t *testing.T) {
	t.Parallel()

	accountVault := newTestVault(t)
	slotPath, err := accountVault.EnsureSlot("claude", "paused")
	if err != nil {
		t.Fatalf("EnsureSlot() error = %v", err)
	}
	if err := writeManagedSlotMetadata(slotPath, claude.Profile{Email: "paused@example.com"}); err != nil {
		t.Fatalf("writeManagedSlotMetadata() error = %v", err)
	}
	before, err := os.ReadFile(filepath.Join(slotPath, slotMetadataFilename))
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	var stdout bytes.Buffer
	parker := accountParker{vault: accountVault, stdout: &stdout}

	if err := parker.Disable("claude", "paused"); err != nil {
		t.Fatalf("Disable() error = %v", err)
	}
	metadata := readSlotMetadata(t, slotPath)
	if !metadata.Disabled || metadata.RefreshPolicy != managedRefreshPolicy || metadata.Email != "paused@example.com" {
		t.Fatalf("metadata after disable = %+v, want disabled with policy and email kept", metadata)
	}
	if got := stdout.String(); got != "Disabled claude account \"paused\".\n" {
		t.Fatalf("stdout = %q, want disable receipt", got)
	}

	stdout.Reset()
	if err := parker.Enable("claude", "paused"); err != nil {
		t.Fatalf("Enable() error = %v", err)
	}
	after, err := os.ReadFile(filepath.Join(slotPath, slotMetadataFilename))
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("slot.json after enable = %s, want the original %s", after, before)
	}
	if got := stdout.String(); got != "Enabled claude account \"paused\".\n" {
		t.Fatalf("stdout = %q, want enable receipt", got)
	}
}

func TestLoginRewriteKeepsSlotDisabled(t *testing.T) {
	t.Parallel()

	accountVault := newTestVault(t)
	seedActiveClaudeAccount(t, accountVault, "work")
	if err := (accountParker{vault: accountVault, stdout: io.Discard}).Disable("claude", "work"); err != nil {
		t.Fatalf("Disable() error = %v", err)
	}
	slotPath, _ := accountVault.SlotPath("claude", "work")

	if err := writeManagedSlotMetadata(slotPath, claude.Profile{Email: "renamed@example.com"}); err != nil {
		t.Fatalf("writeManagedSlotMetadata() error = %v", err)
	}
	metadata := readSlotMetadata(t, slotPath)
	if !metadata.Disabled || metadata.Email != "renamed@example.com" || metadata.RefreshPolicy != managedRefreshPolicy {
		t.Fatalf("metadata after login rewrite = %+v, want disabled kept with the new email", metadata)
	}
}

func TestDisableIsIdempotentAndWorksOnHandSeededSlot(t *testing.T) {
	t.Parallel()

	accountVault := newTestVault(t)
	slotPath, err := accountVault.EnsureSlot("codex", "seeded")
	if err != nil {
		t.Fatalf("EnsureSlot() error = %v", err)
	}
	parker := accountParker{vault: accountVault, stdout: io.Discard}
	for range 2 {
		if err := parker.Disable("codex", "seeded"); err != nil {
			t.Fatalf("Disable() error = %v", err)
		}
	}
	contents, err := os.ReadFile(filepath.Join(slotPath, slotMetadataFilename))
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if got := strings.TrimSpace(string(contents)); got != "{\n  \"disabled\": true\n}" {
		t.Fatalf("slot.json = %s, want only the disabled key", contents)
	}
}

func TestDisableActiveAccountLeavesStateAndLiveLoginAlone(t *testing.T) {
	t.Parallel()

	accountVault := newTestVault(t)
	seedActiveClaudeAccount(t, accountVault, "work")
	var stdout bytes.Buffer
	parker := accountParker{vault: accountVault, stdout: &stdout}

	if err := parker.Disable("claude", "work"); err != nil {
		t.Fatalf("Disable() error = %v", err)
	}
	loaded, err := state.Load(accountVault.Root())
	if err != nil {
		t.Fatalf("state.Load() error = %v", err)
	}
	if active, found := loaded.Active("claude"); !found || active != "work" {
		t.Fatalf("active Claude account = %q, %t; want work kept", active, found)
	}
	if got := stdout.String(); !strings.Contains(got, "The live provider login was not changed.") {
		t.Fatalf("stdout = %q, want live-login receipt", got)
	}
}

func TestDisableExplainsMissingSlot(t *testing.T) {
	t.Parallel()

	parker := accountParker{vault: newTestVault(t), stdout: io.Discard}
	err := parker.Disable("codex", "missing")
	if err == nil || !strings.Contains(err.Error(), "run 'hop ls'") {
		t.Fatalf("Disable() error = %v, want account-list next step", err)
	}
}

func TestDisableWaitsForManagedRefreshBeforeWritingMetadata(t *testing.T) {
	t.Parallel()

	accountVault := newTestVault(t)
	slotPath, err := accountVault.EnsureSlot("codex", "busy")
	if err != nil {
		t.Fatalf("EnsureSlot() error = %v", err)
	}
	releaseRefresh, err := acquireRefreshLock(context.Background(), slotPath)
	if err != nil {
		t.Fatalf("acquireRefreshLock() error = %v", err)
	}
	parker := accountParker{vault: accountVault, stdout: io.Discard}
	result := make(chan error, 1)
	go func() { result <- parker.Disable("codex", "busy") }()

	select {
	case err := <-result:
		t.Fatalf("Disable() returned before refresh released: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	releaseRefresh()
	if err := <-result; err != nil {
		t.Fatalf("Disable() error = %v", err)
	}
	if !readSlotMetadata(t, slotPath).Disabled {
		t.Fatal("slot was not disabled after the refresh released")
	}
}

func TestSwitchRefusesDisabledSlotAndNamesEnable(t *testing.T) {
	t.Parallel()

	manager, _, _, _, _ := newSwitchTestManager(t)
	writeClaudeSlot(t, manager.vault, "work", claudeCredentials("work"))
	writeCodexSlot(t, manager.vault, "work", codexCredentials("work"))
	parker := accountParker{vault: manager.vault, stdout: io.Discard}
	if err := parker.Disable("codex", "work"); err != nil {
		t.Fatalf("Disable() error = %v", err)
	}

	for _, providerName := range []string{"", "codex"} {
		_, err := manager.providersFor(providerName, "work")
		if err == nil || !strings.Contains(err.Error(), "[ACCOUNT_DISABLED]") || !strings.Contains(err.Error(), "hop enable codex work") {
			t.Fatalf("providersFor(%q) error = %v, want disabled refusal naming hop enable", providerName, err)
		}
	}
	providers, err := manager.providersFor("claude", "work")
	if err != nil || len(providers) != 1 {
		t.Fatalf("providersFor(claude) = %v, %v; want the enabled slot alone", providers, err)
	}
}

func TestSwitchCommandRefusesDisabledAccountBeforeTouchingLiveCredentials(t *testing.T) {
	hopHome := t.TempDir()
	t.Setenv("HOP_HOME", hopHome)
	accountVault, err := defaultVault()
	if err != nil {
		t.Fatalf("defaultVault() error = %v", err)
	}
	writeClaudeSlot(t, accountVault, "work", claudeCredentials("work"))
	var stdout, stderr bytes.Buffer
	if exitCode := Run([]string{"disable", "claude", "work"}, &stdout, &stderr); exitCode != 0 {
		t.Fatalf("hop disable exit code = %d, stderr = %q", exitCode, stderr.String())
	}
	stderr.Reset()
	if exitCode := Run([]string{"work"}, &stdout, &stderr); exitCode != 2 {
		t.Fatalf("hop work exit code = %d, want 2", exitCode)
	}
	if got := stderr.String(); !strings.Contains(got, "[ACCOUNT_DISABLED]") || !strings.Contains(got, "hop enable claude work") {
		t.Fatalf("stderr = %q, want disabled refusal naming hop enable", got)
	}
}

func TestGlanceSkipsFetchAndRefreshForDisabledSlot(t *testing.T) {
	t.Parallel()

	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests.Add(1)
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
	accountVault := newTestVault(t)
	for _, name := range []string{"live", "parked"} {
		credentialsPath, _ := accountVault.CredentialsPath("claude", name)
		if err := (claude.FileStore{Path: credentialsPath}).Write(claude.Credentials{AccessToken: "old", RefreshToken: "refresh", ExpiresAt: 1}); err != nil {
			t.Fatalf("Write(%s) error = %v", name, err)
		}
		if err := writeManagedSlotMetadata(filepath.Dir(credentialsPath), claude.Profile{Email: name + "@example.com"}); err != nil {
			t.Fatalf("writeManagedSlotMetadata(%s) error = %v", name, err)
		}
	}
	if err := (accountParker{vault: accountVault, stdout: io.Discard}).Disable("claude", "parked"); err != nil {
		t.Fatalf("Disable() error = %v", err)
	}
	adapter := claude.New(claude.Config{UsageURL: server.URL + "/usage", TokenURL: server.URL + "/token"})
	catalog := vaultCatalog{vault: accountVault, state: state.New(), claudeAdapter: adapter, codexAdapter: codex.New(codex.Config{}), now: time.Now}

	document, err := fetchGlance(context.Background(), catalog)
	if err != nil {
		t.Fatalf("fetchGlance() error = %v", err)
	}
	if got := requests.Load(); got != 2 {
		t.Fatalf("server requests = %d, want refresh and usage for the live slot only", got)
	}
	if len(document.Accounts) != 2 || document.Accounts[0].Account != "live" || document.Accounts[0].Disabled || document.Accounts[0].Error != nil {
		t.Fatalf("live account = %+v, want fetched and enabled", document.Accounts[0])
	}
	parked := document.Accounts[1]
	if parked.Account != "parked" || !parked.Disabled || parked.Error != nil || len(parked.Windows) != 0 {
		t.Fatalf("parked account = %+v, want disabled with nothing fetched", parked)
	}
}

func TestShowAccountsJSONCarriesDisabledOnEveryAccount(t *testing.T) {
	t.Parallel()

	catalog := staticCatalog{
		{Provider: provider.Claude, Name: "on", Fetcher: fetchFunc(func(context.Context) (provider.Usage, error) { return provider.Usage{}, nil })},
		{Provider: provider.Claude, Name: "off", Disabled: true},
	}
	var output bytes.Buffer
	if err := showAccountsFrom(context.Background(), &output, true, catalog, time.Now()); err != nil {
		t.Fatalf("showAccountsFrom() error = %v", err)
	}
	var document struct {
		Schema   string `json:"schema"`
		Accounts []map[string]any
	}
	if err := json.Unmarshal(output.Bytes(), &document); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	if document.Schema != "hop.ls/v1" {
		t.Fatalf("schema = %q, want hop.ls/v1 kept", document.Schema)
	}
	for index, want := range []bool{false, true} {
		if got, present := document.Accounts[index]["disabled"]; !present || got != want {
			t.Fatalf("accounts[%d].disabled = %v (present %t), want %t", index, got, present, want)
		}
	}
}
