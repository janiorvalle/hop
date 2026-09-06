package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/janiorvalle/hop/internal/provider/claude"
	"github.com/janiorvalle/hop/internal/provider/codex"
	"github.com/janiorvalle/hop/internal/state"
	"github.com/janiorvalle/hop/internal/vault"
)

type loginRunnerFunc func(context.Context, loginCommand) error

type failingWriter struct{ err error }

func (writer failingWriter) Write([]byte) (int, error) { return 0, writer.err }

func (run loginRunnerFunc) Run(ctx context.Context, command loginCommand) error {
	return run(ctx, command)
}

func TestLoginClaudeAdoptsSandboxOverrideWithoutReadingKeychain(t *testing.T) {
	hopHome := t.TempDir()
	credentialsPath := filepath.Join(t.TempDir(), ".credentials.json")
	t.Setenv("HOP_HOME", hopHome)
	t.Setenv(claudeCredentialsFileOverride, credentialsPath)
	t.Setenv(claudeAccountEmailOverride, "sandbox@example.test")
	want := claude.Credentials{AccessToken: "sandbox-access", RefreshToken: "sandbox-refresh", ExpiresAt: 42}
	if err := (claude.FileStore{Path: credentialsPath}).Write(want); err != nil {
		t.Fatal(err)
	}

	if err := loginAccount(context.Background(), "claude", "work", strings.NewReader(""), io.Discard, io.Discard); err != nil {
		t.Fatalf("loginAccount() error = %v", err)
	}

	accountVault, err := vault.New(hopHome)
	if err != nil {
		t.Fatal(err)
	}
	slotPath, err := accountVault.CredentialsPath("claude", "work")
	if err != nil {
		t.Fatal(err)
	}
	got, err := (claude.FileStore{Path: slotPath}).Read()
	if err != nil {
		t.Fatal(err)
	}
	if got.AccessToken != want.AccessToken {
		t.Fatalf("slot access token = %q, want sandbox token", got.AccessToken)
	}
	metadata := readSlotMetadata(t, filepath.Dir(slotPath))
	if metadata.Email != "sandbox@example.test" {
		t.Fatalf("slot email = %q, want sandbox@example.test", metadata.Email)
	}
}

type fakeClaudeLiveStore struct {
	credentials claude.Credentials
	reads       int
	writes      []claude.Credentials
	writeErr    error
	blockWrite  bool
}

func (store *fakeClaudeLiveStore) Read(context.Context) (claude.Credentials, error) {
	store.reads++
	return store.credentials, nil
}

func (store *fakeClaudeLiveStore) Write(ctx context.Context, credentials claude.Credentials) error {
	store.writes = append(store.writes, credentials)
	if store.blockWrite {
		<-ctx.Done()
		return ctx.Err()
	}
	if store.writeErr != nil {
		return store.writeErr
	}
	store.credentials = credentials
	return nil
}

func TestLoginCodexUsesIsolatedHomeAndInstallsManagedSlot(t *testing.T) {
	t.Parallel()

	accountVault := newTestVault(t)
	wantCredentials := codex.Credentials{AccessToken: "access", RefreshToken: "refresh", AccountID: "account"}
	var temporaryHome string
	var stdout bytes.Buffer
	manager := loginManager{
		vault: accountVault,
		runner: loginRunnerFunc(func(_ context.Context, command loginCommand) error {
			if command.Name != "codex" || !reflect.DeepEqual(command.Args, []string{"login"}) {
				t.Fatalf("command = %s %v, want codex login", command.Name, command.Args)
			}
			temporaryHome = command.Env["CODEX_HOME"]
			if temporaryHome == "" {
				t.Fatal("CODEX_HOME override is empty")
			}
			return (codex.FileStore{Path: filepath.Join(temporaryHome, "auth.json")}).Write(wantCredentials)
		}),
		stdout: &stdout,
		stderr: io.Discard,
		codexEmail: func(context.Context, codex.Credentials) (string, error) {
			return "owner@example.com", nil
		},
	}

	if err := manager.Login(context.Background(), "codex", "work", strings.NewReader("")); err != nil {
		t.Fatalf("Login() error = %v", err)
	}
	if _, err := os.Stat(temporaryHome); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("isolated CODEX_HOME remains after login: %v", err)
	}
	credentialsPath, _ := accountVault.CredentialsPath("codex", "work")
	gotCredentials, err := (codex.FileStore{Path: credentialsPath}).Read()
	if err != nil || gotCredentials.RefreshToken != wantCredentials.RefreshToken {
		t.Fatalf("installed credentials refresh token preserved = %t, error = %v", gotCredentials.RefreshToken == wantCredentials.RefreshToken, err)
	}
	metadata := readSlotMetadata(t, filepath.Dir(credentialsPath))
	if metadata.RefreshPolicy != managedRefreshPolicy || metadata.Email != "owner@example.com" {
		t.Fatalf("slot metadata = %+v, want managed owner email", metadata)
	}
	if output := stdout.String(); !strings.Contains(output, "owner@example.com") || strings.Contains(output, "refresh") {
		t.Fatalf("stdout = %q, want email and no token material", output)
	}
}

func TestLoginCodexFailureLeavesNoSlot(t *testing.T) {
	t.Parallel()

	accountVault := newTestVault(t)
	var temporaryHome string
	manager := loginManager{
		vault: accountVault,
		runner: loginRunnerFunc(func(_ context.Context, command loginCommand) error {
			temporaryHome = command.Env["CODEX_HOME"]
			return errors.New("browser closed")
		}),
		stdout: io.Discard,
		stderr: io.Discard,
	}

	err := manager.Login(context.Background(), "codex", "work", strings.NewReader(""))
	if err == nil || !strings.Contains(err.Error(), "complete the browser sign-in") {
		t.Fatalf("Login() error = %v, want actionable browser error", err)
	}
	slotPath, _ := accountVault.SlotPath("codex", "work")
	if _, err := os.Stat(slotPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed login slot exists: %v", err)
	}
	if _, err := os.Stat(temporaryHome); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed login CODEX_HOME remains: %v", err)
	}
}

func TestLoginCodexKeepsEnrolledSlotWhenReceiptCannotBeWritten(t *testing.T) {
	t.Parallel()

	accountVault := newTestVault(t)
	manager := loginManager{
		vault: accountVault,
		runner: loginRunnerFunc(func(_ context.Context, command loginCommand) error {
			return (codex.FileStore{Path: filepath.Join(command.Env["CODEX_HOME"], "auth.json")}).Write(codex.Credentials{
				AccessToken: "access", RefreshToken: "refresh", AccountID: "account",
			})
		}),
		stdout: failingWriter{err: errors.New("closed pipe")},
		stderr: io.Discard,
		codexEmail: func(context.Context, codex.Credentials) (string, error) {
			return "owner@example.com", nil
		},
	}

	err := manager.Login(context.Background(), "codex", "work", strings.NewReader(""))
	if err == nil || !strings.Contains(err.Error(), "closed pipe") {
		t.Fatalf("Login() error = %v, want receipt write failure", err)
	}
	credentialsPath, _ := accountVault.CredentialsPath("codex", "work")
	if _, err := (codex.FileStore{Path: credentialsPath}).Read(); err != nil {
		t.Fatalf("enrolled slot was rolled back after receipt failure: %v", err)
	}
}

func TestLoginCodexRejectsIdentityAlreadyEnrolledUnderAnotherName(t *testing.T) {
	t.Parallel()

	accountVault := newTestVault(t)
	existingPath, _ := accountVault.CredentialsPath("codex", "existing")
	if err := (codex.FileStore{Path: existingPath}).Write(codex.Credentials{AccessToken: "old", RefreshToken: "old-refresh", AccountID: "same-account"}); err != nil {
		t.Fatalf("write existing slot: %v", err)
	}
	manager := loginManager{
		vault: accountVault,
		runner: loginRunnerFunc(func(_ context.Context, command loginCommand) error {
			return (codex.FileStore{Path: filepath.Join(command.Env["CODEX_HOME"], "auth.json")}).Write(codex.Credentials{
				AccessToken: "new", RefreshToken: "new-refresh", AccountID: "same-account",
			})
		}),
		stdout: io.Discard,
		stderr: io.Discard,
		codexEmail: func(context.Context, codex.Credentials) (string, error) {
			return "owner@example.com", nil
		},
	}

	err := manager.Login(context.Background(), "codex", "duplicate", strings.NewReader(""))
	if err == nil || !strings.Contains(err.Error(), `already enrolled as account "existing"`) {
		t.Fatalf("Login() error = %v, want duplicate identity guidance", err)
	}
	duplicatePath, _ := accountVault.SlotPath("codex", "duplicate")
	if _, err := os.Stat(duplicatePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("duplicate slot exists: %v", err)
	}
}

func TestSlotReservationPreventsConcurrentEnrollmentAndOwnedCleanup(t *testing.T) {
	t.Parallel()

	manager := loginManager{vault: newTestVault(t)}
	first, err := manager.reserveNewSlot("codex", "work")
	if err != nil {
		t.Fatalf("first reserveNewSlot() error = %v", err)
	}
	if _, err := manager.reserveNewSlot("codex", "work"); err == nil || !strings.Contains(err.Error(), "being enrolled by another hop process") {
		t.Fatalf("second reserveNewSlot() error = %v, want concurrent-enrollment guidance", err)
	}
	first.Cleanup()
	if _, err := os.Stat(first.path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("owned failed reservation remains: %v", err)
	}

	replacement, err := manager.reserveNewSlot("codex", "work")
	if err != nil {
		t.Fatalf("replacement reserveNewSlot() error = %v", err)
	}
	if err := replacement.Commit(); err != nil {
		t.Fatalf("replacement Commit() error = %v", err)
	}
	replacement.Cleanup()
	if _, err := os.Stat(replacement.path); err != nil {
		t.Fatalf("committed slot was removed: %v", err)
	}
}

func TestSlotReservationReclaimsAbandonedEnrollmentWithoutDeletingReplacement(t *testing.T) {
	t.Parallel()

	manager := loginManager{vault: newTestVault(t)}
	abandoned, err := manager.reserveNewSlot("codex", "work")
	if err != nil {
		t.Fatalf("reserveNewSlot() error = %v", err)
	}
	markerPath := filepath.Join(abandoned.path, slotReservationFilename)
	contents, err := json.Marshal(slotReservationRecord{ProcessID: 999999, CreatedAt: time.Now().UTC(), Owner: "abandoned"})
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	if err := os.WriteFile(markerPath, contents, 0o600); err != nil {
		t.Fatalf("WriteFile(stale marker) error = %v", err)
	}

	replacement, err := manager.reserveNewSlot("codex", "work")
	if err != nil {
		t.Fatalf("reserveNewSlot(replacement) error = %v", err)
	}
	abandoned.Cleanup()
	if _, err := os.Stat(replacement.path); err != nil {
		t.Fatalf("old cleanup deleted replacement reservation: %v", err)
	}
	replacement.Cleanup()
}

func TestSlotReservationReclaimsLegacyAbandonedEnrollment(t *testing.T) {
	t.Parallel()

	manager := loginManager{vault: newTestVault(t)}
	legacy, err := manager.reserveNewSlot("codex", "work")
	if err != nil {
		t.Fatalf("reserveNewSlot() error = %v", err)
	}
	markerPath := filepath.Join(legacy.path, slotReservationFilename)
	contents, err := json.Marshal(slotReservationRecord{ProcessID: 999999, CreatedAt: time.Now().UTC()})
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	if err := os.WriteFile(markerPath, contents, 0o600); err != nil {
		t.Fatalf("WriteFile(legacy marker) error = %v", err)
	}

	replacement, err := manager.reserveNewSlot("codex", "work")
	if err != nil {
		t.Fatalf("reserveNewSlot(replacement) error = %v", err)
	}
	legacy.Cleanup()
	if _, err := os.Stat(replacement.path); err != nil {
		t.Fatalf("legacy cleanup deleted replacement reservation: %v", err)
	}
	replacement.Cleanup()
}

func TestLoginClaudeEnrollsCurrentLoginWithoutMutatingLiveSeat(t *testing.T) {
	t.Parallel()

	accountVault := newTestVault(t)
	live := &fakeClaudeLiveStore{credentials: claude.Credentials{AccessToken: "access", RefreshToken: "refresh", Scopes: []string{"user:profile"}}}
	runnerCalls := 0
	manager := loginManager{
		vault:      accountVault,
		claudeLive: live,
		runner: loginRunnerFunc(func(context.Context, loginCommand) error {
			runnerCalls++
			return nil
		}),
		stdout: io.Discard,
		stderr: io.Discard,
		claudeEmail: func(context.Context) (string, error) {
			return "cached@example.com", nil
		},
		claudeProfile: func(_ context.Context, credentials claude.Credentials) (claude.Profile, error) {
			if credentials.AccessToken != "access" {
				t.Fatalf("profile access token = %q, want access", credentials.AccessToken)
			}
			return claude.Profile{AccountUUID: "account-uuid", Email: "claude@example.com"}, nil
		},
	}

	if err := manager.Login(context.Background(), "claude", "work", strings.NewReader("")); err != nil {
		t.Fatalf("Login() error = %v", err)
	}
	if runnerCalls != 0 || len(live.writes) != 0 {
		t.Fatalf("first enrollment runner calls = %d, live writes = %d; want 0, 0", runnerCalls, len(live.writes))
	}
	activeState, err := state.Load(accountVault.Root())
	if err != nil {
		t.Fatalf("state.Load() error = %v", err)
	}
	if active, found := activeState.Active("claude"); !found || active != "work" {
		t.Fatalf("active Claude account = %q, %t; want work, true", active, found)
	}
	credentialsPath, _ := accountVault.CredentialsPath("claude", "work")
	if metadata := readSlotMetadata(t, filepath.Dir(credentialsPath)); metadata.Email != "claude@example.com" || metadata.AccountUUID != "account-uuid" {
		t.Fatalf("slot identity = %#v, want the fresh profile email and account UUID", metadata)
	}
}

func TestLoginClaudeEnrollmentFallsBackToTheStatusEmailWhenProfileFails(t *testing.T) {
	t.Parallel()

	accountVault := newTestVault(t)
	manager := loginManager{
		vault: accountVault,
		claudeLive: &fakeClaudeLiveStore{credentials: claude.Credentials{
			AccessToken: "access",
			Scopes:      []string{"user:profile"},
		}},
		runner: loginRunnerFunc(func(context.Context, loginCommand) error { return nil }),
		stdout: io.Discard,
		stderr: io.Discard,
		claudeEmail: func(context.Context) (string, error) {
			return "owner@example.com", nil
		},
		claudeProfile: func(context.Context, claude.Credentials) (claude.Profile, error) {
			return claude.Profile{}, errors.New("network unavailable")
		},
	}

	if err := manager.Login(context.Background(), "claude", "work", strings.NewReader("")); err != nil {
		t.Fatalf("Login() error = %v, want enrollment to remain network-independent", err)
	}
	credentialsPath, _ := accountVault.CredentialsPath("claude", "work")
	metadata := readSlotMetadata(t, filepath.Dir(credentialsPath))
	if metadata.Email != "owner@example.com" || metadata.AccountUUID != "" {
		t.Fatalf("slot identity = %#v, want status email without an account UUID", metadata)
	}
}

func TestLoginClaudeEnrollmentStopsWhenTheProfileContextIsCanceled(t *testing.T) {
	t.Parallel()

	accountVault := newTestVault(t)
	manager := loginManager{
		vault: accountVault,
		claudeLive: &fakeClaudeLiveStore{credentials: claude.Credentials{
			AccessToken: "access",
			Scopes:      []string{"user:profile"},
		}},
		runner: loginRunnerFunc(func(context.Context, loginCommand) error { return nil }),
		stdout: io.Discard,
		stderr: io.Discard,
		claudeEmail: func(context.Context) (string, error) {
			return "owner@example.com", nil
		},
		claudeProfile: func(ctx context.Context, _ claude.Credentials) (claude.Profile, error) {
			return claude.Profile{}, ctx.Err()
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := manager.Login(ctx, "claude", "work", strings.NewReader(""))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Login() error = %v, want context cancellation", err)
	}
	slotPath, _ := accountVault.SlotPath("claude", "work")
	if _, statErr := os.Stat(slotPath); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("canceled enrollment slot exists: %v", statErr)
	}
}

func TestParseClaudeAccountEmailAllowsSupportedStatusWithoutEmail(t *testing.T) {
	t.Parallel()

	email, err := parseClaudeAccountEmail([]byte(`{"loggedIn":true,"authMethod":"claude.ai","apiProvider":"firstParty"}`))
	if err != nil || email != "" {
		t.Fatalf("parseClaudeAccountEmail() = %q, %v; want empty optional email, nil", email, err)
	}
	if _, err := parseClaudeAccountEmail([]byte(`{"loggedIn":false}`)); err == nil || !strings.Contains(err.Error(), "claude auth login") {
		t.Fatalf("logged-out status error = %v, want login next step", err)
	}
}

func TestLoginClaudeCanExplicitlyConfirmCurrentActiveSlot(t *testing.T) {
	t.Parallel()

	accountVault := newTestVault(t)
	seedActiveClaudeAccount(t, accountVault, "work")
	liveCredentials := claude.Credentials{AccessToken: "current", RefreshToken: "current-refresh"}
	live := &fakeClaudeLiveStore{credentials: liveCredentials}
	manager := loginManager{
		vault:      accountVault,
		claudeLive: live,
		runner: loginRunnerFunc(func(context.Context, loginCommand) error {
			t.Fatal("runner called while confirming active account")
			return nil
		}),
		stdout: io.Discard,
		stderr: io.Discard,
		claudeEmail: func(context.Context) (string, error) {
			return "current@example.com", nil
		},
	}

	if err := manager.Login(context.Background(), "claude", "work", strings.NewReader("")); err != nil {
		t.Fatalf("Login() error = %v", err)
	}
	workPath, _ := accountVault.CredentialsPath("claude", "work")
	workCredentials, err := (claude.FileStore{Path: workPath}).Read()
	if err != nil || workCredentials.RefreshToken != liveCredentials.RefreshToken {
		t.Fatalf("confirmed slot has current credentials = %t, error = %v", workCredentials.RefreshToken == liveCredentials.RefreshToken, err)
	}
	if metadata := readSlotMetadata(t, filepath.Dir(workPath)); metadata.Email != "current@example.com" {
		t.Fatalf("confirmed slot email = %q, want current@example.com", metadata.Email)
	}
	if len(live.writes) != 0 {
		t.Fatalf("live writes = %d, want 0", len(live.writes))
	}
}

func TestLoginClaudeConfirmationKeepsTheRecordedEmailWhenStatusEmailIsStale(t *testing.T) {
	t.Parallel()

	accountVault := newTestVault(t)
	seedActiveClaudeAccount(t, accountVault, "work")
	live := &fakeClaudeLiveStore{credentials: claude.Credentials{AccessToken: "seed", RefreshToken: "seed-refresh"}}
	manager := loginManager{
		vault:      accountVault,
		claudeLive: live,
		runner: loginRunnerFunc(func(context.Context, loginCommand) error {
			t.Fatal("runner called while confirming active account")
			return nil
		}),
		stdout: io.Discard,
		stderr: io.Discard,
		claudeEmail: func(context.Context) (string, error) {
			return "stale@example.com", nil
		},
	}

	if err := manager.Login(context.Background(), "claude", "work", strings.NewReader("")); err != nil {
		t.Fatalf("Login() error = %v", err)
	}
	workPath, _ := accountVault.CredentialsPath("claude", "work")
	if metadata := readSlotMetadata(t, filepath.Dir(workPath)); metadata.Email != "work@example.com" {
		t.Fatalf("confirmed slot email = %q, want the recorded work@example.com kept", metadata.Email)
	}
}

func newTestVault(t *testing.T) vault.Vault {
	t.Helper()
	accountVault, err := vault.New(filepath.Join(t.TempDir(), ".hop"))
	if err != nil {
		t.Fatalf("vault.New() error = %v", err)
	}
	return accountVault
}

func seedActiveClaudeAccount(t *testing.T, accountVault vault.Vault, name string) {
	t.Helper()
	activeState := state.New()
	activeState.SetActive("claude", name)
	if err := activeState.Save(accountVault.Root()); err != nil {
		t.Fatalf("state.Save() error = %v", err)
	}
	credentialsPath, _ := accountVault.CredentialsPath("claude", name)
	if err := (claude.FileStore{Path: credentialsPath}).Write(claude.Credentials{AccessToken: "seed", RefreshToken: "seed-refresh"}); err != nil {
		t.Fatalf("seed Claude slot: %v", err)
	}
	if err := writeManagedSlotMetadata(filepath.Dir(credentialsPath), claude.Profile{Email: "work@example.com"}); err != nil {
		t.Fatalf("seed Claude slot metadata: %v", err)
	}
}

func readSlotMetadata(t *testing.T, slotPath string) slotMetadata {
	t.Helper()
	contents, err := os.ReadFile(filepath.Join(slotPath, slotMetadataFilename))
	if err != nil {
		t.Fatalf("read slot metadata: %v", err)
	}
	var metadata slotMetadata
	if err := json.Unmarshal(contents, &metadata); err != nil {
		t.Fatalf("decode slot metadata: %v", err)
	}
	return metadata
}

func TestLoginClaudeInBrowserEnrollsSecondAccountWithoutTouchingTheLiveLogin(t *testing.T) {
	t.Parallel()

	accountVault := newTestVault(t)
	seedActiveClaudeAccount(t, accountVault, "work")
	live := &fakeClaudeLiveStore{credentials: claude.Credentials{AccessToken: "live", RefreshToken: "live-refresh"}}
	enrolled := claude.Credentials{AccessToken: "new", RefreshToken: "new-refresh", ExpiresAt: 1, RefreshTokenExpiresAt: 2, Scopes: []string{"user:profile"}}
	var stdout bytes.Buffer
	manager := loginManager{
		vault:      accountVault,
		claudeLive: live,
		runner:     loginRunnerFunc(func(context.Context, loginCommand) error { t.Fatal("runner called for a browser login"); return nil }),
		stdout:     &stdout,
		stderr:     io.Discard,
		claudeEmail: func(context.Context) (string, error) {
			t.Fatal("claude auth status consulted for a browser login")
			return "", nil
		},
		claudeLogin: func(context.Context) (claude.Enrollment, error) {
			return claude.Enrollment{Credentials: enrolled, Profile: claude.Profile{AccountUUID: "personal-uuid", Email: "personal@example.com"}}, nil
		},
	}

	if err := manager.Login(context.Background(), "claude", "personal", strings.NewReader("")); err != nil {
		t.Fatalf("Login() error = %v", err)
	}
	if live.reads != 0 || len(live.writes) != 0 {
		t.Fatalf("live reads = %d, writes = %d; want the live login untouched", live.reads, len(live.writes))
	}
	newPath, _ := accountVault.CredentialsPath("claude", "personal")
	got, err := (claude.FileStore{Path: newPath}).Read()
	if err != nil || !reflect.DeepEqual(got, enrolled) {
		t.Fatalf("new slot credentials = %#v, error = %v; want %#v", got, err, enrolled)
	}
	metadata := readSlotMetadata(t, filepath.Dir(newPath))
	if metadata.RefreshPolicy != managedRefreshPolicy || metadata.Email != "personal@example.com" || metadata.AccountUUID != "personal-uuid" {
		t.Fatalf("new slot metadata = %#v, want managed custody with the token response identity", metadata)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(newPath), slotReservationFilename)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("reservation marker remains after enrollment: %v", err)
	}
	if !strings.Contains(stdout.String(), `Enrolled Claude account "personal" (personal@example.com).`) {
		t.Fatalf("stdout = %q, want enrollment receipt", stdout.String())
	}
}

func TestLoginClaudeInBrowserLeavesNoSlotWhenTheSignInFails(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name     string
		loginErr error
		want     string
	}{
		{name: "state mismatch", loginErr: fmt.Errorf("[CLAUDE_LOGIN_STATE_MISMATCH] discarded: %w", claude.ErrLogin), want: "[CLAUDE_LOGIN_STATE_MISMATCH]"},
		{name: "exchange rejected", loginErr: fmt.Errorf("[CLAUDE_LOGIN_EXCHANGE_FAILED] HTTP 400: %w", claude.ErrLogin), want: "[CLAUDE_LOGIN_EXCHANGE_FAILED]"},
		{name: "port busy", loginErr: fmt.Errorf("[CLAUDE_LOGIN_PORT_IN_USE] taken: %w", claude.ErrCallbackPort), want: claudeLoginPortOverride},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			accountVault := newTestVault(t)
			seedActiveClaudeAccount(t, accountVault, "work")
			live := &fakeClaudeLiveStore{}
			manager := loginManager{
				vault:       accountVault,
				claudeLive:  live,
				stdout:      io.Discard,
				stderr:      io.Discard,
				claudeLogin: func(context.Context) (claude.Enrollment, error) { return claude.Enrollment{}, testCase.loginErr },
			}

			err := manager.Login(context.Background(), "claude", "personal", strings.NewReader(""))
			if err == nil || !strings.Contains(err.Error(), testCase.want) || !strings.Contains(err.Error(), "before anything was saved") {
				t.Fatalf("Login() error = %v, want %s with the nothing-saved receipt", err, testCase.want)
			}
			slotPath, _ := accountVault.SlotPath("claude", "personal")
			if _, err := os.Stat(slotPath); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("slot remains after a failed login: %v", err)
			}
			if live.reads != 0 || len(live.writes) != 0 {
				t.Fatalf("live reads = %d, writes = %d; want the live login untouched", live.reads, len(live.writes))
			}
		})
	}
}

func TestLoginClaudeInBrowserRefusesAnExistingAccountBeforeOpeningTheBrowser(t *testing.T) {
	t.Parallel()

	accountVault := newTestVault(t)
	seedActiveClaudeAccount(t, accountVault, "work")
	personalPath, _ := accountVault.CredentialsPath("claude", "personal")
	if err := (claude.FileStore{Path: personalPath}).Write(claude.Credentials{AccessToken: "kept", RefreshToken: "kept-refresh"}); err != nil {
		t.Fatalf("seed existing slot: %v", err)
	}
	manager := loginManager{
		vault:      accountVault,
		claudeLive: &fakeClaudeLiveStore{},
		stdout:     io.Discard,
		stderr:     io.Discard,
		claudeLogin: func(context.Context) (claude.Enrollment, error) {
			t.Fatal("browser login started for an existing account")
			return claude.Enrollment{}, nil
		},
	}

	err := manager.Login(context.Background(), "claude", "personal", strings.NewReader(""))
	if err == nil || !strings.Contains(err.Error(), `claude account "personal" already exists`) {
		t.Fatalf("Login() error = %v, want existing-account refusal", err)
	}
	if got, err := (claude.FileStore{Path: personalPath}).Read(); err != nil || got.RefreshToken != "kept-refresh" {
		t.Fatalf("existing slot = %#v, error = %v; want it untouched", got, err)
	}
}

func TestLoginClaudeInBrowserRejectsIdentityAlreadyEnrolledUnderAnotherName(t *testing.T) {
	t.Parallel()

	accountVault := newTestVault(t)
	seedActiveClaudeAccount(t, accountVault, "work")
	manager := loginManager{
		vault:      accountVault,
		claudeLive: &fakeClaudeLiveStore{},
		stdout:     io.Discard,
		stderr:     io.Discard,
		claudeLogin: func(context.Context) (claude.Enrollment, error) {
			return claude.Enrollment{Credentials: claude.Credentials{AccessToken: "new", RefreshToken: "new-refresh"}, Profile: claude.Profile{Email: "WORK@example.com"}}, nil
		},
	}

	err := manager.Login(context.Background(), "claude", "duplicate", strings.NewReader(""))
	if err == nil || !strings.Contains(err.Error(), `already enrolled as account "work"`) {
		t.Fatalf("Login() error = %v, want duplicate identity refusal", err)
	}
	slotPath, _ := accountVault.SlotPath("claude", "duplicate")
	if _, err := os.Stat(slotPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("slot remains after a duplicate refusal: %v", err)
	}
}

func TestDefaultClaudeLoginRejectsAnUnusablePortOverride(t *testing.T) {
	t.Setenv(claudeLoginPortOverride, "http")

	_, err := defaultClaudeLogin(io.Discard)
	if err == nil || !strings.Contains(err.Error(), claudeLoginPortOverride) || !strings.Contains(err.Error(), "between 1 and 65535") {
		t.Fatalf("defaultClaudeLogin() error = %v, want port override guidance", err)
	}
}

func TestBrowserCommandHonorsBROWSERBeforeThePlatformOpener(t *testing.T) {
	t.Setenv("BROWSER", "my-browser --new-tab")

	got := browserCommand("https://example.test/authorize").Args
	want := []string{"my-browser", "--new-tab", "https://example.test/authorize"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("browserCommand().Args = %v, want %v", got, want)
	}
}
