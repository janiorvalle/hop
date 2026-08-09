package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
	clears      int
	writeErr    error
	clearErr    error
	blockWrite  bool
	// onClear observes the world at the moment the live login is cleared, so a
	// test can prove what was already stashed before the credentials went away.
	onClear func()
}

func (store *fakeClaudeLiveStore) Read(context.Context) (claude.Credentials, error) {
	store.reads++
	return store.credentials, nil
}

func (store *fakeClaudeLiveStore) Clear(context.Context) error {
	store.clears++
	if store.onClear != nil {
		store.onClear()
	}
	if store.clearErr != nil {
		return store.clearErr
	}
	store.credentials = claude.Credentials{}
	return nil
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
		getenv: func(string) string { return "" },
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
		getenv: func(string) string { return "" },
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
		getenv: func(string) string { return "" },
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

func TestLoginClaudeEnrollsSecondAccountWithoutStatusEmailAfterTokenConfirmation(t *testing.T) {
	t.Parallel()

	accountVault := newTestVault(t)
	seedActiveClaudeAccount(t, accountVault, "work")
	original := claude.Credentials{AccessToken: "old", RefreshToken: "old-refresh"}
	workPath, _ := accountVault.CredentialsPath("claude", "work")
	if err := (claude.FileStore{Path: workPath}).Write(original); err != nil {
		t.Fatalf("write confirmed active credentials: %v", err)
	}
	if err := writeManagedSlotMetadata(filepath.Dir(workPath), claude.Profile{}); err != nil {
		t.Fatalf("write email-less metadata: %v", err)
	}
	live := &fakeClaudeLiveStore{credentials: original}
	manager := loginManager{
		vault:      accountVault,
		claudeLive: live,
		runner: loginRunnerFunc(func(_ context.Context, command loginCommand) error {
			if reflect.DeepEqual(command.Args, []string{"auth", "login"}) {
				live.credentials = claude.Credentials{AccessToken: "new", RefreshToken: "new-refresh"}
			}
			return nil
		}),
		stdout: io.Discard,
		stderr: io.Discard,
		getenv: func(string) string { return "approved" },
		claudeEmail: func(context.Context) (string, error) {
			return "", nil
		},
	}

	if err := manager.Login(context.Background(), "claude", "personal", strings.NewReader("")); err != nil {
		t.Fatalf("Login() error = %v", err)
	}
	newPath, _ := accountVault.CredentialsPath("claude", "personal")
	credentials, err := (claude.FileStore{Path: newPath}).Read()
	if err != nil || credentials.RefreshToken != "new-refresh" {
		t.Fatalf("email-less new slot preserved = %t, error = %v", credentials.RefreshToken == "new-refresh", err)
	}
}

func TestLoginClaudeRequiresExplicitQuietWindowBeforeLiveMutation(t *testing.T) {
	t.Parallel()

	accountVault := newTestVault(t)
	seedActiveClaudeAccount(t, accountVault, "work")
	live := &fakeClaudeLiveStore{credentials: claude.Credentials{AccessToken: "old", RefreshToken: "old-refresh"}}
	manager := loginManager{
		vault:      accountVault,
		claudeLive: live,
		runner:     loginRunnerFunc(func(context.Context, loginCommand) error { t.Fatal("runner called without approval"); return nil }),
		stdout:     io.Discard,
		stderr:     io.Discard,
		getenv:     func(string) string { return "" },
	}

	err := manager.Login(context.Background(), "claude", "personal", strings.NewReader(""))
	if err == nil || !strings.Contains(err.Error(), "stop Claude agents") || !strings.Contains(err.Error(), "HOP_CLAUDE_LIVE_LOGIN=approved") {
		t.Fatalf("Login() error = %v, want quiet-window instructions", err)
	}
	if live.reads != 0 || len(live.writes) != 0 {
		t.Fatalf("live reads = %d, writes = %d; want 0, 0 before approval", live.reads, len(live.writes))
	}
}

func TestLoginClaudeStagesNewAccountAndRestoresActiveLogin(t *testing.T) {
	t.Parallel()

	accountVault := newTestVault(t)
	seedActiveClaudeAccount(t, accountVault, "work")
	original := claude.Credentials{AccessToken: "old", RefreshToken: "old-refresh"}
	enrolled := claude.Credentials{AccessToken: "new", RefreshToken: "new-refresh", Scopes: []string{"user:profile"}}
	live := &fakeClaudeLiveStore{credentials: original}
	var commands []string
	emailCalls := 0
	var stdout bytes.Buffer
	manager := loginManager{
		vault:      accountVault,
		claudeLive: live,
		runner: loginRunnerFunc(func(_ context.Context, command loginCommand) error {
			commands = append(commands, command.Name+" "+strings.Join(command.Args, " "))
			if reflect.DeepEqual(command.Args, []string{"auth", "login"}) {
				live.credentials = enrolled
			}
			return nil
		}),
		stdout: &stdout,
		stderr: io.Discard,
		getenv: func(name string) string {
			if name == claudeLiveLoginApproval {
				return "approved"
			}
			return ""
		},
		claudeEmail: func(context.Context) (string, error) {
			emailCalls++
			if emailCalls == 1 {
				return "work@example.com", nil
			}
			return "personal@example.com", nil
		},
		claudeProfile: func(_ context.Context, credentials claude.Credentials) (claude.Profile, error) {
			if credentials.AccessToken != "new" {
				t.Fatalf("profile access token = %q, want newly installed token", credentials.AccessToken)
			}
			return claude.Profile{AccountUUID: "personal-uuid", Email: "personal@example.com"}, nil
		},
	}

	if err := manager.Login(context.Background(), "claude", "personal", strings.NewReader("")); err != nil {
		t.Fatalf("Login() error = %v", err)
	}
	wantCommands := []string{"claude auth login"}
	if !reflect.DeepEqual(commands, wantCommands) {
		t.Fatalf("commands = %v, want %v", commands, wantCommands)
	}
	if live.clears != 1 {
		t.Fatalf("live clears = %d, want 1 before the browser sign-in", live.clears)
	}
	if live.credentials.RefreshToken != original.RefreshToken || len(live.writes) != 1 {
		t.Fatalf("live account restored = %t, writes = %d; want true, 1", live.credentials.RefreshToken == original.RefreshToken, len(live.writes))
	}
	newPath, _ := accountVault.CredentialsPath("claude", "personal")
	got, err := (claude.FileStore{Path: newPath}).Read()
	if err != nil || got.RefreshToken != enrolled.RefreshToken {
		t.Fatalf("new slot token preserved = %t, error = %v", got.RefreshToken == enrolled.RefreshToken, err)
	}
	if metadata := readSlotMetadata(t, filepath.Dir(newPath)); metadata.Email != "personal@example.com" || metadata.AccountUUID != "personal-uuid" {
		t.Fatalf("new slot identity = %#v, want personal profile", metadata)
	}
	if !strings.Contains(stdout.String(), "restored active account \"work\"") {
		t.Fatalf("stdout = %q, want restoration receipt", stdout.String())
	}
}

func TestLoginClaudeStagingPreservesReadOnlyActiveSlotPolicy(t *testing.T) {
	t.Parallel()

	accountVault := newTestVault(t)
	activeState := state.New()
	activeState.SetActive("claude", "seeded")
	if err := activeState.Save(accountVault.Root()); err != nil {
		t.Fatalf("state.Save() error = %v", err)
	}
	original := claude.Credentials{AccessToken: "seeded", RefreshToken: "seeded-refresh"}
	credentialsPath, _ := accountVault.CredentialsPath("claude", "seeded")
	if err := (claude.FileStore{Path: credentialsPath}).Write(original); err != nil {
		t.Fatalf("seed Claude slot: %v", err)
	}
	metadataPath := filepath.Join(filepath.Dir(credentialsPath), slotMetadataFilename)
	if err := os.WriteFile(metadataPath, []byte(`{"refresh_policy":"read-only","email":"seeded@example.com"}`), 0o600); err != nil {
		t.Fatalf("seed slot metadata: %v", err)
	}
	live := &fakeClaudeLiveStore{credentials: original}
	emailReads := 0
	manager := loginManager{
		vault:      accountVault,
		claudeLive: live,
		runner: loginRunnerFunc(func(_ context.Context, command loginCommand) error {
			if reflect.DeepEqual(command.Args, []string{"auth", "login"}) {
				live.credentials = claude.Credentials{AccessToken: "new", RefreshToken: "new-refresh"}
			}
			return nil
		}),
		stdout: io.Discard,
		stderr: io.Discard,
		getenv: func(string) string { return "approved" },
		claudeEmail: func(context.Context) (string, error) {
			emailReads++
			if emailReads == 1 {
				return "seeded@example.com", nil
			}
			return "personal@example.com", nil
		},
	}

	if err := manager.Login(context.Background(), "claude", "personal", strings.NewReader("")); err != nil {
		t.Fatalf("Login() error = %v", err)
	}
	if metadata := readSlotMetadata(t, filepath.Dir(credentialsPath)); metadata.RefreshPolicy != "read-only" {
		t.Fatalf("active slot refresh policy = %q, want read-only", metadata.RefreshPolicy)
	}
}

// Right after hop's own switch the Claude CLI still reports the email of the
// account hop switched away from, while the live Keychain already holds the
// active slot's tokens. Enrollment must key on those tokens, and must not
// stamp the stale email onto the healthy slot it copies back.
func TestLoginClaudeStagesNewAccountWhenStatusEmailIsStale(t *testing.T) {
	t.Parallel()

	accountVault := newTestVault(t)
	seedActiveClaudeAccount(t, accountVault, "work")
	original := claude.Credentials{AccessToken: "seed", RefreshToken: "seed-refresh"}
	live := &fakeClaudeLiveStore{credentials: original}
	emailReads := 0
	manager := loginManager{
		vault:      accountVault,
		claudeLive: live,
		runner: loginRunnerFunc(func(_ context.Context, command loginCommand) error {
			if reflect.DeepEqual(command.Args, []string{"auth", "login"}) {
				live.credentials = claude.Credentials{AccessToken: "new", RefreshToken: "new-refresh"}
			}
			return nil
		}),
		stdout: io.Discard,
		stderr: io.Discard,
		getenv: func(string) string { return "approved" },
		claudeEmail: func(context.Context) (string, error) {
			emailReads++
			if emailReads == 1 {
				return "stale@example.com", nil
			}
			return "personal@example.com", nil
		},
	}

	if err := manager.Login(context.Background(), "claude", "personal", strings.NewReader("")); err != nil {
		t.Fatalf("Login() error = %v, want enrollment to trust the recorded credentials", err)
	}
	workPath, _ := accountVault.CredentialsPath("claude", "work")
	if metadata := readSlotMetadata(t, filepath.Dir(workPath)); metadata.Email != "work@example.com" {
		t.Fatalf("active slot email = %q, want the recorded work@example.com kept", metadata.Email)
	}
	newPath, _ := accountVault.CredentialsPath("claude", "personal")
	credentials, err := (claude.FileStore{Path: newPath}).Read()
	if err != nil || credentials.RefreshToken != "new-refresh" {
		t.Fatalf("new slot refresh token = %q, error = %v; want new-refresh", credentials.RefreshToken, err)
	}
	if live.credentials.RefreshToken != original.RefreshToken {
		t.Fatalf("restored refresh token = %q, want %q", live.credentials.RefreshToken, original.RefreshToken)
	}
}

func TestLoginClaudeStagesNewAccountAfterTheActiveTokensRotate(t *testing.T) {
	t.Parallel()

	accountVault := newTestVault(t)
	seedActiveClaudeAccount(t, accountVault, "work")
	workPath, _ := accountVault.CredentialsPath("claude", "work")
	if err := writeManagedSlotMetadata(filepath.Dir(workPath), claude.Profile{AccountUUID: "work-uuid", Email: "work@example.com"}); err != nil {
		t.Fatalf("write active slot identity: %v", err)
	}
	rotated := claude.Credentials{
		AccessToken:  "rotated",
		RefreshToken: "rotated-refresh",
		Scopes:       []string{"user:profile"},
	}
	live := &fakeClaudeLiveStore{credentials: rotated}
	emailReads := 0
	profileReads := 0
	manager := loginManager{
		vault:      accountVault,
		claudeLive: live,
		runner: loginRunnerFunc(func(_ context.Context, command loginCommand) error {
			if reflect.DeepEqual(command.Args, []string{"auth", "login"}) {
				live.credentials = claude.Credentials{AccessToken: "personal", RefreshToken: "personal-refresh"}
			}
			return nil
		}),
		stdout: io.Discard,
		stderr: io.Discard,
		getenv: func(string) string { return "approved" },
		claudeEmail: func(context.Context) (string, error) {
			emailReads++
			if emailReads == 1 {
				return "stale@example.com", nil
			}
			return "personal@example.com", nil
		},
		claudeProfile: func(context.Context, claude.Credentials) (claude.Profile, error) {
			profileReads++
			return claude.Profile{AccountUUID: "work-uuid", Email: "work@example.com"}, nil
		},
	}

	if err := manager.Login(context.Background(), "claude", "personal", strings.NewReader("")); err != nil {
		t.Fatalf("Login() error = %v, want the fresh profile to confirm the rotated active login", err)
	}
	if profileReads != 1 {
		t.Fatalf("profile reads = %d, want 1 for the rotated active login", profileReads)
	}
	workCredentials, err := (claude.FileStore{Path: workPath}).Read()
	if err != nil || workCredentials.RefreshToken != "rotated-refresh" {
		t.Fatalf("active slot refresh token = %q, error = %v; want rotated-refresh", workCredentials.RefreshToken, err)
	}
	personalPath, _ := accountVault.CredentialsPath("claude", "personal")
	if _, err := (claude.FileStore{Path: personalPath}).Read(); err != nil {
		t.Fatalf("new personal slot was not enrolled: %v", err)
	}
}

func TestLoginClaudeLeavesAnEmailLessActiveSlotUnlabeledWhenStatusEmailIsStale(t *testing.T) {
	t.Parallel()

	accountVault := newTestVault(t)
	seedActiveClaudeAccount(t, accountVault, "work")
	original := claude.Credentials{AccessToken: "seed", RefreshToken: "seed-refresh"}
	workPath, _ := accountVault.CredentialsPath("claude", "work")
	if err := writeManagedSlotMetadata(filepath.Dir(workPath), claude.Profile{}); err != nil {
		t.Fatalf("write email-less metadata: %v", err)
	}
	live := &fakeClaudeLiveStore{credentials: original}
	emailReads := 0
	manager := loginManager{
		vault:      accountVault,
		claudeLive: live,
		runner: loginRunnerFunc(func(_ context.Context, command loginCommand) error {
			if reflect.DeepEqual(command.Args, []string{"auth", "login"}) {
				live.credentials = claude.Credentials{AccessToken: "new", RefreshToken: "new-refresh"}
			}
			return nil
		}),
		stdout: io.Discard,
		stderr: io.Discard,
		getenv: func(string) string { return "approved" },
		claudeEmail: func(context.Context) (string, error) {
			emailReads++
			if emailReads == 1 {
				return "stale@example.com", nil
			}
			return "personal@example.com", nil
		},
	}

	if err := manager.Login(context.Background(), "claude", "personal", strings.NewReader("")); err != nil {
		t.Fatalf("Login() error = %v", err)
	}
	if metadata := readSlotMetadata(t, filepath.Dir(workPath)); metadata.Email != "" {
		t.Fatalf("active slot email = %q, want the slot left unlabeled rather than named by the status cache", metadata.Email)
	}
}

// An unlabeled active slot still has one name for its identity: the status
// email read before the browser sign-in. Signing back into that identity must
// be refused rather than enrolled under a second account name.
func TestLoginClaudeRejectsReturningToAnEmailLessActiveIdentity(t *testing.T) {
	t.Parallel()

	accountVault := newTestVault(t)
	seedActiveClaudeAccount(t, accountVault, "work")
	original := claude.Credentials{AccessToken: "seed", RefreshToken: "seed-refresh"}
	workPath, _ := accountVault.CredentialsPath("claude", "work")
	if err := writeManagedSlotMetadata(filepath.Dir(workPath), claude.Profile{}); err != nil {
		t.Fatalf("write email-less metadata: %v", err)
	}
	live := &fakeClaudeLiveStore{credentials: original}
	manager := loginManager{
		vault:      accountVault,
		claudeLive: live,
		runner: loginRunnerFunc(func(_ context.Context, command loginCommand) error {
			if reflect.DeepEqual(command.Args, []string{"auth", "login"}) {
				live.credentials = claude.Credentials{AccessToken: "fresh", RefreshToken: "fresh-refresh"}
			}
			return nil
		}),
		stdout:      io.Discard,
		stderr:      io.Discard,
		getenv:      func(string) string { return "approved" },
		claudeEmail: func(context.Context) (string, error) { return "work@example.com", nil },
	}

	err := manager.Login(context.Background(), "claude", "duplicate", strings.NewReader(""))
	if err == nil || !strings.Contains(err.Error(), "already-active identity") {
		t.Fatalf("Login() error = %v, want duplicate-identity guidance", err)
	}
	duplicateSlot, _ := accountVault.SlotPath("claude", "duplicate")
	if _, statErr := os.Stat(duplicateSlot); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("duplicate identity slot exists: %v", statErr)
	}
	if live.credentials.RefreshToken != original.RefreshToken {
		t.Fatalf("active login restored = false, got refresh token %q", live.credentials.RefreshToken)
	}
}

// Slots are default-deny: an account seeded by hand stays read-only until
// 'hop login' takes custody of it. Matching tokens must not let staging adopt
// one, or the stale cached email would be stamped onto a slot hop never
// enrolled.
func TestLoginClaudeRefusesToAdoptHandSeededActiveSlotOnMatchingTokens(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name     string
		metadata string
	}{
		{name: "no metadata"},
		{name: "unmanaged metadata", metadata: `{"refresh_policy":"read-only","email":"seeded@example.com"}`},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			accountVault := newTestVault(t)
			activeState := state.New()
			activeState.SetActive("claude", "seeded")
			if err := activeState.Save(accountVault.Root()); err != nil {
				t.Fatalf("state.Save() error = %v", err)
			}
			seeded := claude.Credentials{AccessToken: "seeded", RefreshToken: "seeded-refresh"}
			credentialsPath, _ := accountVault.CredentialsPath("claude", "seeded")
			if err := (claude.FileStore{Path: credentialsPath}).Write(seeded); err != nil {
				t.Fatalf("seed Claude slot: %v", err)
			}
			metadataPath := filepath.Join(filepath.Dir(credentialsPath), slotMetadataFilename)
			if testCase.metadata != "" {
				if err := os.WriteFile(metadataPath, []byte(testCase.metadata), 0o600); err != nil {
					t.Fatalf("seed slot metadata: %v", err)
				}
			}
			manager := loginManager{
				vault:      accountVault,
				claudeLive: &fakeClaudeLiveStore{credentials: seeded},
				runner: loginRunnerFunc(func(context.Context, loginCommand) error {
					t.Fatal("runner called for a hand-seeded active slot")
					return nil
				}),
				stdout:      io.Discard,
				stderr:      io.Discard,
				getenv:      func(string) string { return "approved" },
				claudeEmail: func(context.Context) (string, error) { return "stale@example.com", nil },
			}

			err := manager.Login(context.Background(), "claude", "personal", strings.NewReader(""))
			if err == nil || !strings.Contains(err.Error(), "adopt the current live login") {
				t.Fatalf("Login() error = %v, want the explicit-adoption instruction", err)
			}
			contents, readErr := os.ReadFile(metadataPath)
			if testCase.metadata == "" {
				if !errors.Is(readErr, os.ErrNotExist) {
					t.Fatalf("hand-seeded slot metadata = %q, %v; want the slot left unmanaged", contents, readErr)
				}
				return
			}
			if readErr != nil || string(contents) != testCase.metadata {
				t.Fatalf("hand-seeded slot metadata = %q, %v; want it left as seeded", contents, readErr)
			}
		})
	}
}

// The first live enrollment ran 'claude auth logout', which revoked the grant
// on Anthropic's side and killed the copy hop had just stashed into the active
// account's slot. Staging must clear the live login locally instead, and only
// once that stash is on disk.
func TestLoginClaudeStagingClearsLiveLoginInsteadOfLoggingOut(t *testing.T) {
	t.Parallel()

	accountVault := newTestVault(t)
	seedActiveClaudeAccount(t, accountVault, "work")
	original := claude.Credentials{AccessToken: "old", RefreshToken: "old-refresh"}
	enrolled := claude.Credentials{AccessToken: "new", RefreshToken: "new-refresh"}
	live := &fakeClaudeLiveStore{credentials: original}
	var steps []string
	live.onClear = func() {
		steps = append(steps, "clear live login")
		stashPath, err := accountVault.CredentialsPath("claude", "work")
		if err != nil {
			t.Errorf("CredentialsPath() error = %v", err)
			return
		}
		stashed, err := (claude.FileStore{Path: stashPath}).Read()
		if err != nil || stashed.RefreshToken != original.RefreshToken {
			t.Errorf("stashed refresh token when the live login was cleared = %q, error = %v; want %q saved first", stashed.RefreshToken, err, original.RefreshToken)
		}
	}
	emailCalls := 0
	manager := loginManager{
		vault:      accountVault,
		claudeLive: live,
		runner: loginRunnerFunc(func(_ context.Context, command loginCommand) error {
			steps = append(steps, command.Name+" "+strings.Join(command.Args, " "))
			if reflect.DeepEqual(command.Args, []string{"auth", "login"}) {
				live.credentials = enrolled
			}
			return nil
		}),
		stdout: io.Discard,
		stderr: io.Discard,
		getenv: func(name string) string {
			if name == claudeLiveLoginApproval {
				return "approved"
			}
			return ""
		},
		claudeEmail: func(context.Context) (string, error) {
			emailCalls++
			if emailCalls == 1 {
				return "work@example.com", nil
			}
			return "personal@example.com", nil
		},
	}

	if err := manager.Login(context.Background(), "claude", "personal", strings.NewReader("")); err != nil {
		t.Fatalf("Login() error = %v", err)
	}
	for _, step := range steps {
		if strings.Contains(step, "logout") {
			t.Fatalf("staging ran %q; logging out revokes the stashed account on Anthropic's side", step)
		}
	}
	wantSteps := []string{"clear live login", "claude auth login"}
	if !reflect.DeepEqual(steps, wantSteps) {
		t.Fatalf("staging steps = %v, want %v", steps, wantSteps)
	}
	if live.credentials.RefreshToken != original.RefreshToken {
		t.Fatalf("restored refresh token = %q, want %q", live.credentials.RefreshToken, original.RefreshToken)
	}
}

func TestLoginClaudeRestoresActiveAccountWhenClearingTheLiveLoginFails(t *testing.T) {
	t.Parallel()

	accountVault := newTestVault(t)
	seedActiveClaudeAccount(t, accountVault, "work")
	original := claude.Credentials{AccessToken: "old", RefreshToken: "old-refresh"}
	live := &fakeClaudeLiveStore{credentials: original, clearErr: errors.New("keychain locked")}
	var commands []string
	manager := loginManager{
		vault:      accountVault,
		claudeLive: live,
		runner: loginRunnerFunc(func(_ context.Context, command loginCommand) error {
			commands = append(commands, command.Name+" "+strings.Join(command.Args, " "))
			return nil
		}),
		stdout: io.Discard,
		stderr: io.Discard,
		getenv: func(name string) string {
			if name == claudeLiveLoginApproval {
				return "approved"
			}
			return ""
		},
		claudeEmail: func(context.Context) (string, error) { return "work@example.com", nil },
	}

	err := manager.Login(context.Background(), "claude", "personal", strings.NewReader(""))
	if err == nil || !strings.Contains(err.Error(), "clear the live Claude login") {
		t.Fatalf("Login() error = %v, want the failure to name the live login it could not clear", err)
	}
	if len(commands) != 0 {
		t.Fatalf("commands = %v, want none once the live login could not be cleared", commands)
	}
	if len(live.writes) != 1 || live.writes[0].RefreshToken != original.RefreshToken {
		t.Fatalf("live writes = %+v, want the active account restored once", live.writes)
	}
}

func TestLoginClaudeRefusesToOverwriteSlotWhenLiveIdentityChanged(t *testing.T) {
	t.Parallel()

	accountVault := newTestVault(t)
	seedActiveClaudeAccount(t, accountVault, "work")
	live := &fakeClaudeLiveStore{credentials: claude.Credentials{AccessToken: "other", RefreshToken: "other-refresh"}}
	manager := loginManager{
		vault:      accountVault,
		claudeLive: live,
		runner: loginRunnerFunc(func(context.Context, loginCommand) error {
			t.Fatal("runner called after identity mismatch")
			return nil
		}),
		stdout: io.Discard,
		stderr: io.Discard,
		getenv: func(string) string { return "approved" },
		claudeEmail: func(context.Context) (string, error) {
			return "other@example.com", nil
		},
	}

	err := manager.Login(context.Background(), "claude", "personal", strings.NewReader(""))
	if err == nil || !strings.Contains(err.Error(), "restore the recorded account") {
		t.Fatalf("Login() error = %v, want identity reconciliation step", err)
	}
	workPath, _ := accountVault.CredentialsPath("claude", "work")
	workCredentials, readErr := (claude.FileStore{Path: workPath}).Read()
	if readErr != nil || workCredentials.RefreshToken != "seed-refresh" {
		t.Fatalf("recorded work slot unchanged = %t, error = %v", workCredentials.RefreshToken == "seed-refresh", readErr)
	}
	if len(live.writes) != 0 {
		t.Fatalf("live writes = %d, want 0", len(live.writes))
	}
}

func TestLoginClaudeRejectsFreshTokensForAlreadyActiveIdentity(t *testing.T) {
	t.Parallel()

	accountVault := newTestVault(t)
	seedActiveClaudeAccount(t, accountVault, "work")
	original := claude.Credentials{AccessToken: "old", RefreshToken: "old-refresh"}
	live := &fakeClaudeLiveStore{credentials: original}
	emailCalls := 0
	manager := loginManager{
		vault:      accountVault,
		claudeLive: live,
		runner: loginRunnerFunc(func(_ context.Context, command loginCommand) error {
			if reflect.DeepEqual(command.Args, []string{"auth", "login"}) {
				live.credentials = claude.Credentials{AccessToken: "fresh", RefreshToken: "fresh-refresh"}
			}
			return nil
		}),
		stdout: io.Discard,
		stderr: io.Discard,
		getenv: func(string) string { return "approved" },
		claudeEmail: func(context.Context) (string, error) {
			emailCalls++
			return "work@example.com", nil
		},
	}

	err := manager.Login(context.Background(), "claude", "duplicate", strings.NewReader(""))
	if err == nil || !strings.Contains(err.Error(), "already-active identity") {
		t.Fatalf("Login() error = %v, want duplicate-identity guidance", err)
	}
	if emailCalls != 2 {
		t.Fatalf("email reads = %d, want active and newly logged-in identities", emailCalls)
	}
	if live.credentials.RefreshToken != original.RefreshToken {
		t.Fatalf("active login restored = false, got refresh token %q", live.credentials.RefreshToken)
	}
	duplicateSlot, _ := accountVault.SlotPath("claude", "duplicate")
	if _, err := os.Stat(duplicateSlot); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("duplicate identity slot exists: %v", err)
	}
}

func TestLoginClaudeRejectsIdentityAlreadyEnrolledUnderAnotherName(t *testing.T) {
	t.Parallel()

	accountVault := newTestVault(t)
	seedActiveClaudeAccount(t, accountVault, "work")
	existingPath, _ := accountVault.CredentialsPath("claude", "existing")
	if err := (claude.FileStore{Path: existingPath}).Write(claude.Credentials{AccessToken: "existing", RefreshToken: "existing-refresh"}); err != nil {
		t.Fatalf("write existing slot: %v", err)
	}
	if err := writeManagedSlotMetadata(filepath.Dir(existingPath), claude.Profile{AccountUUID: "personal-uuid", Email: "old-personal@example.com"}); err != nil {
		t.Fatalf("write existing metadata: %v", err)
	}
	original := claude.Credentials{AccessToken: "old", RefreshToken: "old-refresh"}
	live := &fakeClaudeLiveStore{credentials: original}
	emailCalls := 0
	manager := loginManager{
		vault:      accountVault,
		claudeLive: live,
		runner: loginRunnerFunc(func(_ context.Context, command loginCommand) error {
			if reflect.DeepEqual(command.Args, []string{"auth", "login"}) {
				live.credentials = claude.Credentials{AccessToken: "fresh", RefreshToken: "fresh-refresh", Scopes: []string{"user:profile"}}
			}
			return nil
		}),
		stdout: io.Discard,
		stderr: io.Discard,
		getenv: func(string) string { return "approved" },
		claudeEmail: func(context.Context) (string, error) {
			emailCalls++
			if emailCalls == 1 {
				return "work@example.com", nil
			}
			return "new-personal@example.com", nil
		},
		claudeProfile: func(context.Context, claude.Credentials) (claude.Profile, error) {
			return claude.Profile{AccountUUID: "personal-uuid", Email: "new-personal@example.com"}, nil
		},
	}

	err := manager.Login(context.Background(), "claude", "duplicate", strings.NewReader(""))
	if err == nil || !strings.Contains(err.Error(), `already enrolled as account "existing"`) {
		t.Fatalf("Login() error = %v, want duplicate identity guidance", err)
	}
	if live.credentials.RefreshToken != original.RefreshToken {
		t.Fatalf("active login restored = false, got refresh token %q", live.credentials.RefreshToken)
	}
	duplicatePath, _ := accountVault.SlotPath("claude", "duplicate")
	if _, err := os.Stat(duplicatePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("duplicate slot exists: %v", err)
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
		getenv: func(string) string { return "" },
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
		getenv: func(string) string { return "" },
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

func TestLoginClaudeRestoresActiveLoginWhenNewLoginFails(t *testing.T) {
	t.Parallel()

	accountVault := newTestVault(t)
	seedActiveClaudeAccount(t, accountVault, "work")
	original := claude.Credentials{AccessToken: "old", RefreshToken: "old-refresh"}
	live := &fakeClaudeLiveStore{credentials: original}
	manager := loginManager{
		vault:      accountVault,
		claudeLive: live,
		runner: loginRunnerFunc(func(_ context.Context, command loginCommand) error {
			if reflect.DeepEqual(command.Args, []string{"auth", "login"}) {
				live.credentials = claude.Credentials{}
				return errors.New("browser closed")
			}
			return nil
		}),
		stdout: io.Discard,
		stderr: io.Discard,
		getenv: func(string) string { return "approved" },
		claudeEmail: func(context.Context) (string, error) {
			return "work@example.com", nil
		},
	}

	err := manager.Login(context.Background(), "claude", "personal", strings.NewReader(""))
	if err == nil || !strings.Contains(err.Error(), "previous login will be restored") {
		t.Fatalf("Login() error = %v, want restoration guidance", err)
	}
	if live.credentials.RefreshToken != original.RefreshToken || len(live.writes) != 1 {
		t.Fatalf("live account restored after failure = %t, writes = %d; want true, 1", live.credentials.RefreshToken == original.RefreshToken, len(live.writes))
	}
	newSlot, _ := accountVault.SlotPath("claude", "personal")
	if _, err := os.Stat(newSlot); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed Claude login slot exists: %v", err)
	}
}

func TestLoginClaudeBoundsRestorationAfterFailedLogin(t *testing.T) {
	t.Parallel()

	accountVault := newTestVault(t)
	seedActiveClaudeAccount(t, accountVault, "work")
	live := &fakeClaudeLiveStore{
		credentials: claude.Credentials{AccessToken: "old", RefreshToken: "old-refresh"},
		blockWrite:  true,
	}
	manager := loginManager{
		vault:      accountVault,
		claudeLive: live,
		runner: loginRunnerFunc(func(_ context.Context, command loginCommand) error {
			if reflect.DeepEqual(command.Args, []string{"auth", "login"}) {
				return errors.New("browser closed")
			}
			return nil
		}),
		stdout:      io.Discard,
		stderr:      io.Discard,
		getenv:      func(string) string { return "approved" },
		restoreWait: 10 * time.Millisecond,
		claudeEmail: func(context.Context) (string, error) {
			return "work@example.com", nil
		},
	}

	err := manager.Login(context.Background(), "claude", "personal", strings.NewReader(""))
	if err == nil || !strings.Contains(err.Error(), "restore active Claude account") || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Login() error = %v, want bounded restoration failure", err)
	}
}

func TestLoginClaudeKeepsNewSlotWhenActiveRestorationFails(t *testing.T) {
	t.Parallel()

	accountVault := newTestVault(t)
	seedActiveClaudeAccount(t, accountVault, "work")
	original := claude.Credentials{AccessToken: "old", RefreshToken: "old-refresh"}
	live := &fakeClaudeLiveStore{credentials: original, writeErr: errors.New("keychain locked")}
	emailCalls := 0
	manager := loginManager{
		vault:      accountVault,
		claudeLive: live,
		runner: loginRunnerFunc(func(_ context.Context, command loginCommand) error {
			if reflect.DeepEqual(command.Args, []string{"auth", "login"}) {
				live.credentials = claude.Credentials{AccessToken: "new", RefreshToken: "new-refresh"}
			}
			return nil
		}),
		stdout: io.Discard,
		stderr: io.Discard,
		getenv: func(string) string { return "approved" },
		claudeEmail: func(context.Context) (string, error) {
			emailCalls++
			if emailCalls == 1 {
				return "work@example.com", nil
			}
			return "personal@example.com", nil
		},
	}

	err := manager.Login(context.Background(), "claude", "personal", strings.NewReader(""))
	if err == nil || !strings.Contains(err.Error(), "could not restore active account") {
		t.Fatalf("Login() error = %v, want restoration failure", err)
	}
	newPath, _ := accountVault.CredentialsPath("claude", "personal")
	credentials, readErr := (claude.FileStore{Path: newPath}).Read()
	if readErr != nil || credentials.RefreshToken != "new-refresh" {
		t.Fatalf("new slot preserved = %t, error = %v", credentials.RefreshToken == "new-refresh", readErr)
	}
}

func TestLoginClaudeRecoversInterruptedStagingBeforeContinuing(t *testing.T) {
	t.Parallel()

	accountVault := newTestVault(t)
	seedActiveClaudeAccount(t, accountVault, "work")
	workPath, _ := accountVault.CredentialsPath("claude", "work")
	wantCredentials := claude.Credentials{AccessToken: "saved", RefreshToken: "saved-refresh"}
	if err := (claude.FileStore{Path: workPath}).Write(wantCredentials); err != nil {
		t.Fatalf("write recovery slot: %v", err)
	}
	record, err := json.Marshal(claudeStagingRecord{ActiveAccount: "work", ProcessID: 999999, CreatedAt: time.Now().UTC()})
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	transactionPath := filepath.Join(accountVault.Root(), claudeStagingFilename)
	if err := os.WriteFile(transactionPath, record, 0o600); err != nil {
		t.Fatalf("WriteFile(transaction) error = %v", err)
	}
	live := &fakeClaudeLiveStore{}
	var stderr bytes.Buffer
	manager := loginManager{
		vault:      accountVault,
		claudeLive: live,
		runner:     loginRunnerFunc(func(context.Context, loginCommand) error { return nil }),
		stdout:     io.Discard,
		stderr:     &stderr,
		getenv:     func(string) string { return "" },
		claudeEmail: func(context.Context) (string, error) {
			return "work@example.com", nil
		},
	}

	if err := manager.Login(context.Background(), "claude", "work", strings.NewReader("")); err != nil {
		t.Fatalf("Login() error = %v", err)
	}
	if live.credentials.RefreshToken != wantCredentials.RefreshToken {
		t.Fatalf("live refresh token restored = false, got %q", live.credentials.RefreshToken)
	}
	if _, err := os.Stat(transactionPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("completed transaction marker remains: %v", err)
	}
	if !strings.Contains(stderr.String(), "restored Claude account \"work\"") {
		t.Fatalf("stderr = %q, want interrupted-enrollment recovery receipt", stderr.String())
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
