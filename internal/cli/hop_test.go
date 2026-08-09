package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/janiorvalle/hop/internal/provider/claude"
	"github.com/janiorvalle/hop/internal/provider/codex"
	"github.com/janiorvalle/hop/internal/state"
	"github.com/janiorvalle/hop/internal/vault"
)

func TestSwitchClaudeCopiesBackBeforeInstallingTarget(t *testing.T) {
	manager, stateStore, claudeLive, _, output := newSwitchTestManager(t)
	writeClaudeSlot(t, manager.vault, "old", claudeCredentials("slot-old"))
	writeClaudeSlot(t, manager.vault, "work", claudeCredentials("target"))
	stateStore.value.SetActive("claude", "old")
	claudeLive.credentials = claudeCredentials("live-rotated")

	if err := manager.Switch(context.Background(), "claude", "work"); err != nil {
		t.Fatalf("Switch() error = %v", err)
	}

	assertClaudeSlot(t, manager.vault, "old", "live-rotated")
	if claudeLive.credentials.AccessToken != "target-access" {
		t.Fatalf("live Claude access token = %q, want target-access", claudeLive.credentials.AccessToken)
	}
	if active, _ := stateStore.value.Active("claude"); active != "work" {
		t.Fatalf("active Claude account = %q, want work", active)
	}
	if !strings.Contains(output.String(), "may fail its next token refresh") {
		t.Fatalf("output = %q, want mid-session warning", output.String())
	}
}

func TestFuzzySwitchChangesEveryMatchingProvider(t *testing.T) {
	manager, stateStore, claudeLive, codexLive, output := newSwitchTestManager(t)
	writeClaudeSlot(t, manager.vault, "old-claude", claudeCredentials("claude-old"))
	writeClaudeSlot(t, manager.vault, "work", claudeCredentials("claude-target"))
	writeCodexSlot(t, manager.vault, "old-codex", codexCredentials("codex-old"))
	writeCodexSlot(t, manager.vault, "work", codexCredentials("codex-target"))
	stateStore.value.SetActive("claude", "old-claude")
	stateStore.value.SetActive("codex", "old-codex")
	claudeLive.credentials = claudeCredentials("claude-live")
	codexLive.credentials = codexCredentials("codex-live")
	alignCodexSlotAccountID(t, manager.vault, "old-codex", codexLive.credentials.AccountID)

	if err := manager.Switch(context.Background(), "", "work"); err != nil {
		t.Fatalf("Switch() error = %v", err)
	}

	assertClaudeSlot(t, manager.vault, "old-claude", "claude-live")
	assertCodexSlot(t, manager.vault, "old-codex", "codex-live")
	if claudeLive.credentials.AccessToken != "claude-target-access" {
		t.Errorf("live Claude access token = %q, want claude-target-access", claudeLive.credentials.AccessToken)
	}
	if codexLive.credentials.AccessToken != "codex-target-access" {
		t.Errorf("live Codex access token = %q, want codex-target-access", codexLive.credentials.AccessToken)
	}
	if active, _ := stateStore.value.Active("claude"); active != "work" {
		t.Errorf("active Claude account = %q, want work", active)
	}
	if active, _ := stateStore.value.Active("codex"); active != "work" {
		t.Errorf("active Codex account = %q, want work", active)
	}
	if got := output.String(); !strings.Contains(got, "Switched claude") || !strings.Contains(got, "Switched codex") {
		t.Fatalf("output = %q, want both providers", got)
	}
}

func TestFuzzySwitchChangesOnlyProvidersWithTargetSlot(t *testing.T) {
	manager, stateStore, claudeLive, codexLive, _ := newSwitchTestManager(t)
	writeClaudeSlot(t, manager.vault, "old", claudeCredentials("claude-old"))
	writeClaudeSlot(t, manager.vault, "work", claudeCredentials("claude-target"))
	stateStore.value.SetActive("claude", "old")
	claudeLive.credentials = claudeCredentials("claude-live")
	codexLive.credentials = codexCredentials("codex-live")

	if err := manager.Switch(context.Background(), "", "work"); err != nil {
		t.Fatalf("Switch() error = %v", err)
	}

	if active, _ := stateStore.value.Active("claude"); active != "work" {
		t.Fatalf("active Claude account = %q, want work", active)
	}
	if _, found := stateStore.value.Active("codex"); found {
		t.Fatal("Codex state changed without a matching target slot")
	}
	if len(codexLive.writes) != 0 {
		t.Fatalf("Codex writes = %d, want 0", len(codexLive.writes))
	}
}

func TestFirstCodexSwitchFindsAndCopiesBackTheLiveAccount(t *testing.T) {
	manager, stateStore, _, codexLive, _ := newSwitchTestManager(t)
	writeCodexSlot(t, manager.vault, "personal", codexCredentials("personal-stale"))
	writeCodexSlot(t, manager.vault, "work", codexCredentials("work"))
	codexLive.credentials = codexCredentials("personal-live")
	personalPath, err := manager.vault.CredentialsPath("codex", "personal")
	if err != nil {
		t.Fatal(err)
	}
	personalCredentials, err := (codex.FileStore{Path: personalPath}).Read()
	if err != nil {
		t.Fatal(err)
	}
	personalCredentials.AccountID = codexLive.credentials.AccountID
	if err := (codex.FileStore{Path: personalPath}).Write(personalCredentials); err != nil {
		t.Fatal(err)
	}

	if err := manager.Switch(context.Background(), "codex", "work"); err != nil {
		t.Fatalf("Switch() error = %v", err)
	}

	assertCodexSlot(t, manager.vault, "personal", "personal-live")
	if active, _ := stateStore.value.Active("codex"); active != "work" {
		t.Fatalf("active Codex account = %q, want work", active)
	}
}

func TestFirstCodexSwitchRefusesToDiscardAnUnenrolledLiveAccount(t *testing.T) {
	manager, stateStore, _, codexLive, _ := newSwitchTestManager(t)
	writeCodexSlot(t, manager.vault, "work", codexCredentials("work"))
	codexLive.credentials = codexCredentials("untracked")

	err := manager.Switch(context.Background(), "codex", "work")
	if err == nil || !strings.Contains(err.Error(), "live login does not match an enrolled slot") {
		t.Fatalf("Switch() error = %v, want safe-adoption guidance", err)
	}
	if len(codexLive.writes) != 0 {
		t.Fatalf("Codex writes = %d, want 0", len(codexLive.writes))
	}
	if _, found := stateStore.value.Active("codex"); found {
		t.Fatal("Codex active state changed after refused switch")
	}
}

func TestSwitchRejectsMissingAndUnusableTargetsWithoutChangingLiveCredentials(t *testing.T) {
	t.Run("missing", func(t *testing.T) {
		manager, _, claudeLive, _, _ := newSwitchTestManager(t)
		claudeLive.credentials = claudeCredentials("live")

		err := manager.Switch(context.Background(), "claude", "missing")
		if err == nil || !strings.Contains(err.Error(), "hop login claude missing") {
			t.Fatalf("Switch() error = %v, want login next step", err)
		}
		if len(claudeLive.writes) != 0 {
			t.Fatalf("live writes = %d, want 0", len(claudeLive.writes))
		}
	})

	t.Run("unusable", func(t *testing.T) {
		manager, stateStore, claudeLive, _, _ := newSwitchTestManager(t)
		writeClaudeSlot(t, manager.vault, "old", claudeCredentials("old"))
		path, err := manager.vault.EnsureSlot("claude", "dead")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(path, vault.CredentialsFilename), []byte("{}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		stateStore.value.SetActive("claude", "old")
		claudeLive.credentials = claudeCredentials("live")

		err = manager.Switch(context.Background(), "claude", "dead")
		if err == nil || !strings.Contains(err.Error(), "hop rm claude dead") {
			t.Fatalf("Switch() error = %v, want repair next step", err)
		}
		assertClaudeSlot(t, manager.vault, "old", "old")
		if len(claudeLive.writes) != 0 {
			t.Fatalf("live writes = %d, want 0", len(claudeLive.writes))
		}
	})
}

func TestSwitchRefusesToCopyBackAChangedLiveIdentity(t *testing.T) {
	t.Run("claude", func(t *testing.T) {
		manager, stateStore, claudeLive, _, _ := newSwitchTestManager(t)
		writeClaudeSlot(t, manager.vault, "personal", claudeCredentials("personal"))
		writeClaudeSlot(t, manager.vault, "work", claudeCredentials("work"))
		stateStore.value.SetActive("claude", "personal")
		claudeLive.credentials = claudeCredentials("other-live")
		manager.claudeEmail = func(context.Context) (string, error) { return "other@example.test", nil }

		err := manager.Switch(context.Background(), "claude", "work")
		if err == nil || !strings.Contains(err.Error(), "different identity") && !strings.Contains(err.Error(), "signed in as") {
			t.Fatalf("Switch() error = %v, want identity mismatch", err)
		}
		assertClaudeSlot(t, manager.vault, "personal", "personal")
		if len(claudeLive.writes) != 0 {
			t.Fatalf("Claude live writes = %d, want 0", len(claudeLive.writes))
		}
	})

	t.Run("codex", func(t *testing.T) {
		manager, stateStore, _, codexLive, _ := newSwitchTestManager(t)
		writeCodexSlot(t, manager.vault, "personal", codexCredentials("personal"))
		writeCodexSlot(t, manager.vault, "work", codexCredentials("work"))
		stateStore.value.SetActive("codex", "personal")
		codexLive.credentials = codexCredentials("other-live")

		err := manager.Switch(context.Background(), "codex", "work")
		if err == nil || !strings.Contains(err.Error(), "different identity") {
			t.Fatalf("Switch() error = %v, want identity mismatch", err)
		}
		assertCodexSlot(t, manager.vault, "personal", "personal")
		if len(codexLive.writes) != 0 {
			t.Fatalf("Codex live writes = %d, want 0", len(codexLive.writes))
		}
	})
}

func TestMultiProviderSwitchRollsBackLiveCredentialsWhenInstallFails(t *testing.T) {
	manager, stateStore, claudeLive, codexLive, _ := newSwitchTestManager(t)
	writeClaudeSlot(t, manager.vault, "old", claudeCredentials("claude-old"))
	writeClaudeSlot(t, manager.vault, "work", claudeCredentials("claude-target"))
	writeCodexSlot(t, manager.vault, "old", codexCredentials("codex-old"))
	writeCodexSlot(t, manager.vault, "work", codexCredentials("codex-target"))
	stateStore.value.SetActive("claude", "old")
	stateStore.value.SetActive("codex", "old")
	claudeLive.credentials = claudeCredentials("claude-live")
	codexLive.credentials = codexCredentials("codex-live")
	alignCodexSlotAccountID(t, manager.vault, "old", codexLive.credentials.AccountID)
	codexLive.failWrites = 1

	err := manager.Switch(context.Background(), "", "work")
	if err == nil || !strings.Contains(err.Error(), "previous live credentials were restored") {
		t.Fatalf("Switch() error = %v, want rollback confirmation", err)
	}
	if claudeLive.credentials.AccessToken != "claude-live-access" {
		t.Errorf("live Claude access token = %q, want rollback value", claudeLive.credentials.AccessToken)
	}
	if codexLive.credentials.AccessToken != "codex-live-access" {
		t.Errorf("live Codex access token = %q, want rollback value", codexLive.credentials.AccessToken)
	}
	if active, _ := stateStore.value.Active("claude"); active != "old" {
		t.Errorf("active Claude account = %q, want old", active)
	}
	if active, _ := stateStore.value.Active("codex"); active != "old" {
		t.Errorf("active Codex account = %q, want old", active)
	}
}

func TestSwitchRollsBackLiveCredentialsWhenStateSaveFails(t *testing.T) {
	manager, stateStore, claudeLive, _, _ := newSwitchTestManager(t)
	writeClaudeSlot(t, manager.vault, "old", claudeCredentials("old"))
	writeClaudeSlot(t, manager.vault, "work", claudeCredentials("target"))
	stateStore.value.SetActive("claude", "old")
	stateStore.failSaves = 1
	claudeLive.credentials = claudeCredentials("live")

	err := manager.Switch(context.Background(), "claude", "work")
	if err == nil || !strings.Contains(err.Error(), "previous live credentials were restored") {
		t.Fatalf("Switch() error = %v, want rollback confirmation", err)
	}
	if claudeLive.credentials.AccessToken != "live-access" {
		t.Fatalf("live Claude access token = %q, want live-access", claudeLive.credentials.AccessToken)
	}
	if active, _ := stateStore.value.Active("claude"); active != "old" {
		t.Fatalf("active Claude account = %q, want old", active)
	}
}

func TestSwitchRecoversInterruptedInstallBeforeCopyingBackAgain(t *testing.T) {
	manager, stateStore, claudeLive, _, output := newSwitchTestManager(t)
	writeClaudeSlot(t, manager.vault, "old", claudeCredentials("old-live"))
	writeClaudeSlot(t, manager.vault, "work", claudeCredentials("work"))
	writeClaudeSlot(t, manager.vault, "other", claudeCredentials("other"))
	stateStore.value.SetActive("claude", "old")
	claudeLive.credentials = claudeCredentials("work")
	transaction := switchTransaction{Steps: []switchTransactionStep{{
		Provider:       "claude",
		Previous:       "old",
		Target:         "work",
		HadActiveState: true,
	}}}
	if err := manager.writeSwitchTransaction(transaction); err != nil {
		t.Fatal(err)
	}

	if err := manager.Switch(context.Background(), "claude", "other"); err != nil {
		t.Fatalf("Switch() error = %v", err)
	}

	assertClaudeSlot(t, manager.vault, "old", "old-live")
	if len(claudeLive.writes) < 2 || claudeLive.writes[0].AccessToken != "old-live-access" {
		t.Fatalf("live write sequence = %#v, want interrupted account restored first", claudeLive.writes)
	}
	if !strings.Contains(output.String(), "Recovered an interrupted account switch") {
		t.Fatalf("output = %q, want recovery receipt", output.String())
	}
	if _, err := os.Stat(manager.transactionPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("transaction file still exists: %v", err)
	}
}

func TestSwitchRecoveryKeepsCommittedTarget(t *testing.T) {
	manager, stateStore, claudeLive, _, _ := newSwitchTestManager(t)
	writeClaudeSlot(t, manager.vault, "old", claudeCredentials("old"))
	writeClaudeSlot(t, manager.vault, "work", claudeCredentials("work"))
	stateStore.value.SetActive("claude", "work")
	claudeLive.credentials = claudeCredentials("work")
	transaction := switchTransaction{
		Committed: true,
		Steps: []switchTransactionStep{{
			Provider:       "claude",
			Previous:       "old",
			Target:         "work",
			HadActiveState: true,
		}},
	}
	if err := manager.writeSwitchTransaction(transaction); err != nil {
		t.Fatal(err)
	}

	if err := manager.Switch(context.Background(), "claude", "work"); err != nil {
		t.Fatalf("Switch() error = %v", err)
	}

	if claudeLive.credentials.AccessToken != "work-access" {
		t.Fatalf("live Claude access token = %q, want committed target", claudeLive.credentials.AccessToken)
	}
	assertClaudeSlot(t, manager.vault, "old", "old")
}

func TestGlanceRecoversInterruptedSwitchBeforeReadingState(t *testing.T) {
	hopHome := t.TempDir()
	codexLivePath := filepath.Join(t.TempDir(), "auth.json")
	t.Setenv("HOP_HOME", hopHome)
	t.Setenv(codexAuthFileOverride, codexLivePath)
	accountVault, err := vault.New(hopHome)
	if err != nil {
		t.Fatal(err)
	}
	writeCodexSlot(t, accountVault, "old", codexCredentials("old-live"))
	writeCodexSlot(t, accountVault, "work", codexCredentials("work"))
	if err := (codex.FileStore{Path: codexLivePath}).Write(codexCredentials("work")); err != nil {
		t.Fatal(err)
	}
	activeState := state.New()
	activeState.SetActive("codex", "old")
	if err := activeState.Save(hopHome); err != nil {
		t.Fatal(err)
	}
	manager, err := defaultSwitchManager(&bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.writeSwitchTransaction(switchTransaction{Steps: []switchTransactionStep{{
		Provider:       "codex",
		Previous:       "old",
		Target:         "work",
		HadActiveState: true,
	}}}); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if err := showAccountsSafely(ctx, &stdout, &stderr, true); err != nil {
		t.Fatalf("showAccountsSafely() error = %v", err)
	}
	if !json.Valid(stdout.Bytes()) {
		t.Fatalf("stdout = %q, want one JSON document", stdout.String())
	}
	if !strings.Contains(stderr.String(), "recovered an interrupted") {
		t.Fatalf("stderr = %q, want recovery notice", stderr.String())
	}

	written, err := (codex.FileStore{Path: codexLivePath}).Read()
	if err != nil {
		t.Fatal(err)
	}
	if written.AccessToken != "old-live-access" {
		t.Fatalf("live Codex access token = %q, want recovered old-live-access", written.AccessToken)
	}
	if _, err := os.Stat(manager.transactionPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("transaction file still exists: %v", err)
	}
}

func TestSwitchingActiveAccountPreservesRotatedLiveCredentials(t *testing.T) {
	manager, stateStore, claudeLive, _, _ := newSwitchTestManager(t)
	writeClaudeSlot(t, manager.vault, "work", claudeCredentials("stale"))
	stateStore.value.SetActive("claude", "work")
	claudeLive.credentials = claudeCredentials("rotated")

	if err := manager.Switch(context.Background(), "claude", "work"); err != nil {
		t.Fatalf("Switch() error = %v", err)
	}

	assertClaudeSlot(t, manager.vault, "work", "rotated")
	if claudeLive.credentials.AccessToken != "rotated-access" {
		t.Fatalf("live Claude access token = %q, want rotated-access", claudeLive.credentials.AccessToken)
	}
}

func TestRunUsesSandboxLiveTargetsAndLeavesSharedFilesUntouched(t *testing.T) {
	hopHome := t.TempDir()
	claudeDirectory := t.TempDir()
	codexDirectory := t.TempDir()
	claudeLivePath := filepath.Join(claudeDirectory, ".credentials.json")
	codexLivePath := filepath.Join(codexDirectory, "auth.json")
	t.Setenv("HOP_HOME", hopHome)
	t.Setenv(claudeCredentialsFileOverride, claudeLivePath)
	t.Setenv(claudeAccountEmailOverride, "owner@example.test")
	t.Setenv(codexAuthFileOverride, codexLivePath)

	accountVault, err := vault.New(hopHome)
	if err != nil {
		t.Fatal(err)
	}
	writeClaudeSlot(t, accountVault, "old", claudeCredentials("claude-old"))
	writeClaudeSlot(t, accountVault, "work", claudeCredentials("claude-target"))
	writeCodexSlot(t, accountVault, "old", codexCredentials("codex-old"))
	writeCodexSlot(t, accountVault, "work", codexCredentials("codex-target"))
	if err := (claude.FileStore{Path: claudeLivePath}).Write(claudeCredentials("claude-live")); err != nil {
		t.Fatal(err)
	}
	if err := (codex.FileStore{Path: codexLivePath}).Write(codexCredentials("codex-live")); err != nil {
		t.Fatal(err)
	}
	alignCodexSlotAccountID(t, accountVault, "old", codexCredentials("codex-live").AccountID)
	activeState := state.New()
	activeState.SetActive("claude", "old")
	activeState.SetActive("codex", "old")
	if err := activeState.Save(hopHome); err != nil {
		t.Fatal(err)
	}
	claudeSettings := filepath.Join(claudeDirectory, "settings.json")
	codexHistory := filepath.Join(codexDirectory, "history.jsonl")
	if err := os.WriteFile(claudeSettings, []byte("shared-claude\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(codexHistory, []byte("shared-codex\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if exitCode := Run([]string{"work"}, &stdout, &stderr); exitCode != 0 {
		t.Fatalf("Run() exit code = %d, want 0; stderr = %q", exitCode, stderr.String())
	}

	assertClaudeSlot(t, accountVault, "old", "claude-live")
	assertCodexSlot(t, accountVault, "old", "codex-live")
	assertFileContents(t, claudeSettings, "shared-claude\n")
	assertFileContents(t, codexHistory, "shared-codex\n")
	writtenClaude, err := (claude.FileStore{Path: claudeLivePath}).Read()
	if err != nil || writtenClaude.AccessToken != "claude-target-access" {
		t.Fatalf("sandbox Claude live credentials = %#v, %v", writtenClaude, err)
	}
	writtenCodex, err := (codex.FileStore{Path: codexLivePath}).Read()
	if err != nil || writtenCodex.AccessToken != "codex-target-access" {
		t.Fatalf("sandbox Codex live credentials = %#v, %v", writtenCodex, err)
	}
}

func TestSwitchRecoveryRefusesTransactionFromDifferentLiveTargets(t *testing.T) {
	manager, stateStore, _, codexLive, _ := newSwitchTestManager(t)
	writeCodexSlot(t, manager.vault, "old", codexCredentials("old"))
	writeCodexSlot(t, manager.vault, "work", codexCredentials("work"))
	stateStore.value.SetActive("codex", "old")
	codexLive.credentials = codexCredentials("real-live")
	sandboxManager := manager
	sandboxManager.codexTarget = "/tmp/sandbox/auth.json"
	if err := sandboxManager.writeSwitchTransaction(switchTransaction{Steps: []switchTransactionStep{{
		Provider:       "codex",
		Previous:       "old",
		Target:         "work",
		HadActiveState: true,
	}}}); err != nil {
		t.Fatal(err)
	}

	_, err := manager.recoverInterruptedSwitch(context.Background())
	if err == nil || !strings.Contains(err.Error(), "recorded against live target") {
		t.Fatalf("recoverInterruptedSwitch() error = %v, want live-target mismatch refusal", err)
	}
	if len(codexLive.writes) != 0 {
		t.Fatalf("live writes = %d, want 0 after refused recovery", len(codexLive.writes))
	}
	if _, statErr := os.Stat(manager.transactionPath()); statErr != nil {
		t.Fatalf("transaction file was removed despite refusal: %v", statErr)
	}
}

func TestLoginClaudeSandboxStillConfirmsActiveAccount(t *testing.T) {
	hopHome := t.TempDir()
	credentialsPath := filepath.Join(t.TempDir(), ".credentials.json")
	t.Setenv("HOP_HOME", hopHome)
	t.Setenv(claudeCredentialsFileOverride, credentialsPath)
	t.Setenv(claudeAccountEmailOverride, "sandbox@example.test")
	rotated := claude.Credentials{AccessToken: "rotated-access", RefreshToken: "rotated-refresh", ExpiresAt: 42}
	if err := (claude.FileStore{Path: credentialsPath}).Write(rotated); err != nil {
		t.Fatal(err)
	}
	accountVault, err := vault.New(hopHome)
	if err != nil {
		t.Fatal(err)
	}
	writeClaudeSlot(t, accountVault, "work", claudeCredentials("stale"))
	activeState := state.New()
	activeState.SetActive("claude", "work")
	if err := activeState.Save(hopHome); err != nil {
		t.Fatal(err)
	}

	if exitCode := Run([]string{"login", "claude", "work"}, &bytes.Buffer{}, &bytes.Buffer{}); exitCode != 0 {
		t.Fatalf("Run(login claude work) exit code = %d, want 0 (confirm active account in sandbox)", exitCode)
	}
	assertClaudeSlot(t, accountVault, "work", "rotated")

	var stderr bytes.Buffer
	if exitCode := Run([]string{"login", "claude", "personal"}, &bytes.Buffer{}, &stderr); exitCode == 0 {
		t.Fatal("Run(login claude personal) succeeded, want sandbox refusal for a new account")
	} else if !strings.Contains(stderr.String(), claudeCredentialsFileOverride) {
		t.Fatalf("stderr = %q, want sandbox-override guidance", stderr.String())
	}
}

type memoryStateStore struct {
	value     state.State
	failSaves int
}

func (store *memoryStateStore) Load() (state.State, error) {
	return cloneState(store.value), nil
}

func (store *memoryStateStore) Save(value state.State) error {
	if store.failSaves > 0 {
		store.failSaves--
		return errors.New("disk full")
	}
	store.value = cloneState(value)
	return nil
}

type fakeClaudeKeychain struct {
	credentials claude.Credentials
	writes      []claude.Credentials
	clears      int
	failWrites  int
}

func (store *fakeClaudeKeychain) Read(context.Context) (claude.Credentials, error) {
	return store.credentials, nil
}

func (store *fakeClaudeKeychain) Write(_ context.Context, credentials claude.Credentials) error {
	store.writes = append(store.writes, credentials)
	if store.failWrites > 0 {
		store.failWrites--
		return errors.New("fake Keychain write failed")
	}
	store.credentials = credentials
	return nil
}

func (store *fakeClaudeKeychain) Clear(context.Context) error {
	store.clears++
	store.credentials = claude.Credentials{}
	return nil
}

type fakeCodexLiveStore struct {
	credentials codex.Credentials
	writes      []codex.Credentials
	failWrites  int
}

func (store *fakeCodexLiveStore) Read() (codex.Credentials, error) {
	return store.credentials, nil
}

func (store *fakeCodexLiveStore) Write(credentials codex.Credentials) error {
	store.writes = append(store.writes, credentials)
	if store.failWrites > 0 {
		store.failWrites--
		return errors.New("fake auth.json write failed")
	}
	store.credentials = credentials
	return nil
}

func newSwitchTestManager(t *testing.T) (switchManager, *memoryStateStore, *fakeClaudeKeychain, *fakeCodexLiveStore, *bytes.Buffer) {
	t.Helper()
	accountVault, err := vault.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	stateStore := &memoryStateStore{value: state.New()}
	claudeLive := &fakeClaudeKeychain{}
	codexLive := &fakeCodexLiveStore{}
	output := &bytes.Buffer{}
	return switchManager{
		vault:      accountVault,
		state:      stateStore,
		claudeLive: claudeLive,
		codexLive:  codexLive,
		claudeEmail: func(context.Context) (string, error) {
			return "owner@example.test", nil
		},
		stdout: output,
	}, stateStore, claudeLive, codexLive, output
}

func cloneState(value state.State) state.State {
	cloned := state.New()
	for providerName, accountName := range value.ActiveAccounts {
		cloned.SetActive(providerName, accountName)
	}
	return cloned
}

func claudeCredentials(label string) claude.Credentials {
	return claude.Credentials{AccessToken: label + "-access", RefreshToken: label + "-refresh", ExpiresAt: 42}
}

func codexCredentials(label string) codex.Credentials {
	return codex.Credentials{AuthMode: "chatgpt", AccessToken: label + "-access", RefreshToken: label + "-refresh", AccountID: label + "-account"}
}

func writeClaudeSlot(t *testing.T, accountVault vault.Vault, accountName string, credentials claude.Credentials) {
	t.Helper()
	path, err := accountVault.CredentialsPath("claude", accountName)
	if err != nil {
		t.Fatal(err)
	}
	if err := (claude.FileStore{Path: path}).Write(credentials); err != nil {
		t.Fatal(err)
	}
	if err := writeManagedSlotMetadata(filepath.Dir(path), "owner@example.test"); err != nil {
		t.Fatal(err)
	}
}

func alignCodexSlotAccountID(t *testing.T, accountVault vault.Vault, accountName, accountID string) {
	t.Helper()
	path, err := accountVault.CredentialsPath("codex", accountName)
	if err != nil {
		t.Fatal(err)
	}
	credentials, err := (codex.FileStore{Path: path}).Read()
	if err != nil {
		t.Fatal(err)
	}
	credentials.AccountID = accountID
	if err := (codex.FileStore{Path: path}).Write(credentials); err != nil {
		t.Fatal(err)
	}
}

func writeCodexSlot(t *testing.T, accountVault vault.Vault, accountName string, credentials codex.Credentials) {
	t.Helper()
	path, err := accountVault.CredentialsPath("codex", accountName)
	if err != nil {
		t.Fatal(err)
	}
	if err := (codex.FileStore{Path: path}).Write(credentials); err != nil {
		t.Fatal(err)
	}
}

func assertClaudeSlot(t *testing.T, accountVault vault.Vault, accountName, label string) {
	t.Helper()
	path, err := accountVault.CredentialsPath("claude", accountName)
	if err != nil {
		t.Fatal(err)
	}
	credentials, err := (claude.FileStore{Path: path}).Read()
	if err != nil {
		t.Fatal(err)
	}
	if credentials.AccessToken != label+"-access" {
		t.Fatalf("Claude slot %q access token = %q, want %q", accountName, credentials.AccessToken, label+"-access")
	}
}

func assertCodexSlot(t *testing.T, accountVault vault.Vault, accountName, label string) {
	t.Helper()
	path, err := accountVault.CredentialsPath("codex", accountName)
	if err != nil {
		t.Fatal(err)
	}
	credentials, err := (codex.FileStore{Path: path}).Read()
	if err != nil {
		t.Fatal(err)
	}
	if credentials.AccessToken != label+"-access" {
		t.Fatalf("Codex slot %q access token = %q, want %q", accountName, credentials.AccessToken, label+"-access")
	}
}

func assertFileContents(t *testing.T, path, expected string) {
	t.Helper()
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(contents) != expected {
		t.Fatalf("%s contents = %q, want %q", path, contents, expected)
	}
}

func TestDefaultCodexSwitchStoreHonorsCodexHome(t *testing.T) {
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	t.Setenv(codexAuthFileOverride, "")

	_, target, err := defaultCodexSwitchStore()
	if err != nil {
		t.Fatalf("defaultCodexSwitchStore() error = %v", err)
	}
	if want := filepath.Join(codexHome, "auth.json"); target != want {
		t.Fatalf("codex live target = %q, want %q", target, want)
	}
}
