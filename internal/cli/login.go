package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/janiorvalle/hop/internal/provider/claude"
	"github.com/janiorvalle/hop/internal/provider/codex"
	"github.com/janiorvalle/hop/internal/state"
	"github.com/janiorvalle/hop/internal/vault"
)

const claudeLiveLoginApproval = "HOP_CLAUDE_LIVE_LOGIN"
const slotReservationFilename = ".login-reservation"
const claudeStagingFilename = ".claude-login-transaction.json"
const claudeLoginLockFilename = ".claude-login.lock"
const codexLoginLockFilename = ".codex-login.lock"
const claudeRestoreTimeout = 20 * time.Second

type loginCommand struct {
	Name   string
	Args   []string
	Env    map[string]string
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
}

type loginRunner interface {
	Run(context.Context, loginCommand) error
}

type claudeLiveStore interface {
	Read(context.Context) (claude.Credentials, error)
	Write(context.Context, claude.Credentials) error
	Clear(context.Context) error
}

type loginManager struct {
	vault         vault.Vault
	runner        loginRunner
	claudeLive    claudeLiveStore
	stdout        io.Writer
	stderr        io.Writer
	getenv        func(string) string
	codexEmail    func(context.Context, codex.Credentials) (string, error)
	claudeEmail   func(context.Context) (string, error)
	restoreWait   time.Duration
	sandboxClaude bool
}

type systemLoginRunner struct{}
type systemClaudeLiveStore struct{}

type slotReservation struct {
	path  string
	owner string
	keep  bool
}

type slotReservationRecord struct {
	ProcessID int       `json:"pid"`
	CreatedAt time.Time `json:"created_at"`
	Owner     string    `json:"owner"`
}

type claudeStagingRecord struct {
	ActiveAccount string    `json:"active_account"`
	ProcessID     int       `json:"pid"`
	CreatedAt     time.Time `json:"created_at"`
}

func loginAccount(ctx context.Context, providerName, accountName string, stdin io.Reader, stdout, stderr io.Writer) error {
	accountVault, err := defaultVault()
	if err != nil {
		return err
	}
	claudeLive, claudeEmail := defaultClaudeSwitchStore()
	manager := loginManager{
		vault:      accountVault,
		runner:     systemLoginRunner{},
		claudeLive: claudeLive,
		stdout:     stdout,
		stderr:     stderr,
		getenv:     os.Getenv,
		codexEmail: func(ctx context.Context, credentials codex.Credentials) (string, error) {
			usage, err := codex.New(codex.Config{}).FetchUsage(ctx, credentials)
			return usage.Email, err
		},
		claudeEmail:   claudeEmail,
		sandboxClaude: strings.TrimSpace(os.Getenv(claudeCredentialsFileOverride)) != "",
	}
	return manager.Login(ctx, providerName, accountName, stdin)
}

func (manager loginManager) Login(ctx context.Context, providerName, accountName string, stdin io.Reader) error {
	switch providerName {
	case "codex":
		reservation, err := manager.reserveNewSlot(providerName, accountName)
		if err != nil {
			return err
		}
		defer reservation.Cleanup()
		return manager.loginCodex(ctx, accountName, reservation, stdin)
	case "claude":
		return manager.loginClaude(ctx, accountName, stdin)
	default:
		return fmt.Errorf("unknown provider %q; use claude or codex", providerName)
	}
}

func (manager loginManager) loginCodex(ctx context.Context, accountName string, reservation *slotReservation, stdin io.Reader) error {
	temporaryHome, err := os.MkdirTemp("", "hop-codex-login-*")
	if err != nil {
		return fmt.Errorf("create isolated Codex login directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(temporaryHome) }()
	if err := os.Chmod(temporaryHome, 0o700); err != nil {
		return fmt.Errorf("secure isolated Codex login directory %s: %w", temporaryHome, err)
	}
	if err := manager.runner.Run(ctx, loginCommand{
		Name:   "codex",
		Args:   []string{"login"},
		Env:    map[string]string{"CODEX_HOME": temporaryHome},
		Stdin:  stdin,
		Stdout: manager.stdout,
		Stderr: manager.stderr,
	}); err != nil {
		return fmt.Errorf("codex login did not finish; complete the browser sign-in and retry 'hop login codex %s': %w", accountName, err)
	}
	credentials, err := (codex.FileStore{Path: filepath.Join(temporaryHome, "auth.json")}).Read()
	if err != nil {
		return fmt.Errorf("codex login finished without usable isolated credentials; retry 'hop login codex %s': %w", accountName, err)
	}
	email, emailErr := manager.codexEmail(ctx, credentials)
	releaseCodexCommit, err := acquireLoginLock(ctx, manager.vault.Root(), codexLoginLockFilename, "Codex enrollment commit")
	if err != nil {
		return err
	}
	defer releaseCodexCommit()
	if duplicateAccount, err := manager.duplicateCodexAccount(accountName, credentials); err != nil {
		return err
	} else if duplicateAccount != "" {
		return fmt.Errorf("codex identity is already enrolled as account %q; use that account or remove it before assigning a new name", duplicateAccount)
	}
	if err := manager.installCodexSlot(accountName, reservation.path, credentials, email); err != nil {
		return err
	}
	if err := reservation.Commit(); err != nil {
		return err
	}
	if emailErr != nil {
		_, _ = fmt.Fprintf(manager.stderr, "hop: enrolled codex account %q, but its email could not be read; the supplied account name was kept\n", accountName)
	}
	if email != "" {
		_, err = fmt.Fprintf(manager.stdout, "Enrolled codex account %q (%s).\n", accountName, email)
	} else {
		_, err = fmt.Fprintf(manager.stdout, "Enrolled codex account %q.\n", accountName)
	}
	return err
}

func (manager loginManager) loginClaude(ctx context.Context, accountName string, stdin io.Reader) (returnErr error) {
	releaseClaudeLogin, err := acquireClaudeLoginLock(ctx, manager.vault.Root())
	if err != nil {
		return err
	}
	defer releaseClaudeLogin()
	recoveredAccount, err := manager.recoverInterruptedClaudeLogin(ctx)
	if err != nil {
		return err
	}
	if recoveredAccount != "" {
		_, _ = fmt.Fprintf(manager.stderr, "hop: restored Claude account %q after an interrupted enrollment\n", recoveredAccount)
	}
	activeState, err := state.Load(manager.vault.Root())
	if err != nil {
		return err
	}
	activeAccount, hasActiveAccount := activeState.Active("claude")
	if !hasActiveAccount {
		reservation, err := manager.reserveNewSlot("claude", accountName)
		if err != nil {
			return err
		}
		defer reservation.Cleanup()
		credentials, err := manager.claudeLive.Read(ctx)
		if err != nil {
			return fmt.Errorf("no active Claude account is recorded and no live login could be read; run 'claude auth login', then retry: %w", err)
		}
		email, emailErr := manager.claudeEmail(ctx)
		if emailErr != nil {
			return fmt.Errorf("read the current Claude account email before enrollment; run 'claude auth status --json' to fix the login, then retry: %w", emailErr)
		}
		if duplicateAccount, err := manager.duplicateClaudeAccount(accountName, email, credentials); err != nil {
			return err
		} else if duplicateAccount != "" {
			return fmt.Errorf("claude identity is already enrolled as account %q; use that account or remove it before assigning a new name", duplicateAccount)
		}
		if err := manager.installClaudeSlot(accountName, reservation.path, email, credentials); err != nil {
			return err
		}
		releaseState, err := acquireStateLock(ctx, manager.vault.Root())
		if err != nil {
			return err
		}
		latestState, err := state.Load(manager.vault.Root())
		if err == nil {
			if latestActive, found := latestState.Active("claude"); found {
				err = fmt.Errorf("claude account %q became active while %q was being enrolled; retry with a different account name after the other login finishes", latestActive, accountName)
			} else {
				latestState.SetActive("claude", accountName)
				err = latestState.Save(manager.vault.Root())
			}
		}
		releaseState()
		if err != nil {
			return fmt.Errorf("save %q as the active Claude account; the incomplete slot was removed, fix the hop directory permissions and retry: %w", accountName, err)
		}
		if err := reservation.Commit(); err != nil {
			return err
		}
		_, err = fmt.Fprintf(manager.stdout, "Enrolled the current live Claude login as account %q%s.\n", accountName, emailSuffix(email))
		return err
	}
	if accountName == activeAccount {
		return manager.confirmActiveClaudeSlot(ctx, activeAccount)
	}
	if manager.sandboxClaude {
		return fmt.Errorf("cannot add another Claude account while %s is set because Claude's browser login would use the real Keychain; unset the override for a user-approved live login, or test loginManager with an injected fake runner", claudeCredentialsFileOverride)
	}
	if manager.getenv(claudeLiveLoginApproval) != "approved" {
		return fmt.Errorf("claude enrollment temporarily replaces the live Keychain login and needs a quiet window; stop Claude agents, then rerun with %s=approved hop login claude %s", claudeLiveLoginApproval, accountName)
	}
	originalCredentials, err := manager.claudeLive.Read(ctx)
	if err != nil {
		return fmt.Errorf("read the active Claude login before staging; unlock Keychain and retry: %w", err)
	}
	activeEmail, activeEmailErr := manager.claudeEmail(ctx)
	if activeEmailErr != nil {
		return fmt.Errorf("confirm the live Claude account before staging; run 'claude auth status --json' to fix the login, then retry: %w", activeEmailErr)
	}
	if err := manager.confirmRecordedClaudeIdentity(activeAccount, activeEmail, originalCredentials); err != nil {
		return err
	}
	reservation, err := manager.reserveNewSlot("claude", accountName)
	if err != nil {
		return err
	}
	defer reservation.Cleanup()
	if err := manager.saveClaudeSlot(activeAccount, activeEmail, originalCredentials); err != nil {
		return fmt.Errorf("copy back active Claude account %q before login; the live login was not changed: %w", activeAccount, err)
	}
	if err := manager.beginClaudeStaging(activeAccount); err != nil {
		return err
	}
	restored := false
	defer func() {
		if restored {
			return
		}
		if restoreErr := manager.finishClaudeStaging(ctx, originalCredentials); restoreErr != nil {
			returnErr = errors.Join(returnErr, fmt.Errorf("restore active Claude account %q after enrollment failed; stop using Claude and restore its slot before continuing: %w", activeAccount, restoreErr))
		}
	}()
	// The live login is cleared locally rather than with 'claude auth logout',
	// which revokes the grant server-side and would kill the copy just stashed
	// into the active account's slot.
	if err := manager.claudeLive.Clear(ctx); err != nil {
		return fmt.Errorf("clear the live Claude login so its browser sign-in opens for account %q; the previous login will be restored, then retry during the quiet window: %w", accountName, err)
	}
	if err := manager.runner.Run(ctx, loginCommand{Name: "claude", Args: []string{"auth", "login"}, Stdin: stdin, Stdout: manager.stdout, Stderr: manager.stderr}); err != nil {
		return fmt.Errorf("claude login did not finish; the previous login will be restored, then retry 'hop login claude %s': %w", accountName, err)
	}
	newCredentials, err := manager.claudeLive.Read(ctx)
	if err != nil {
		return fmt.Errorf("claude login finished without readable credentials; the previous login will be restored, then retry: %w", err)
	}
	if newCredentials.RefreshToken == originalCredentials.RefreshToken {
		return fmt.Errorf("claude login returned the already-active account %q; the previous login will be restored, retry and choose the account for slot %q", activeAccount, accountName)
	}
	newEmail, newEmailErr := manager.claudeEmail(ctx)
	if newEmailErr != nil {
		return fmt.Errorf("read the newly logged-in Claude account email before enrollment; the previous login will be restored, run 'claude auth status --json' to fix the login, then retry: %w", newEmailErr)
	}
	if newEmail != "" && activeEmail != "" && strings.EqualFold(newEmail, activeEmail) {
		return fmt.Errorf("claude login returned the already-active identity %s; the previous login will be restored, retry and choose the account for slot %q", activeEmail, accountName)
	}
	if duplicateAccount, err := manager.duplicateClaudeAccount(accountName, newEmail, newCredentials); err != nil {
		return err
	} else if duplicateAccount != "" {
		return fmt.Errorf("claude identity is already enrolled as account %q; the previous login will be restored, use that account or remove it before assigning a new name", duplicateAccount)
	}
	if err := manager.installClaudeSlot(accountName, reservation.path, newEmail, newCredentials); err != nil {
		return err
	}
	if err := reservation.Commit(); err != nil {
		return err
	}
	if err := manager.finishClaudeStaging(ctx, originalCredentials); err != nil {
		return fmt.Errorf("enrolled Claude account %q but could not restore active account %q; stop using Claude and restore its slot before continuing: %w", accountName, activeAccount, err)
	}
	restored = true
	_, err = fmt.Fprintf(manager.stdout, "Enrolled Claude account %q%s and restored active account %q.\n", accountName, emailSuffix(newEmail), activeAccount)
	return err
}

func (manager loginManager) duplicateCodexAccount(newAccount string, credentials codex.Credentials) (string, error) {
	providerPath := filepath.Join(manager.vault.Root(), "codex")
	entries, err := os.ReadDir(providerPath)
	if err != nil {
		return "", fmt.Errorf("check existing Codex accounts for this identity; inspect %s permissions and retry: %w", providerPath, err)
	}
	for _, entry := range entries {
		if !entry.IsDir() || entry.Name() == newAccount {
			continue
		}
		path, _ := manager.vault.CredentialsPath("codex", entry.Name())
		existing, err := (codex.FileStore{Path: path}).Read()
		if err != nil {
			if reserved, _, _ := inspectSlotReservation(filepath.Dir(path)); reserved {
				continue
			}
			return "", fmt.Errorf("check whether Codex account %q has this identity; repair or remove its slot before retrying: %w", entry.Name(), err)
		}
		if existing.AccountID == credentials.AccountID {
			return entry.Name(), nil
		}
	}
	return "", nil
}

func (manager loginManager) duplicateClaudeAccount(newAccount, email string, credentials claude.Credentials) (string, error) {
	providerPath := filepath.Join(manager.vault.Root(), "claude")
	entries, err := os.ReadDir(providerPath)
	if err != nil {
		return "", fmt.Errorf("check existing Claude accounts for this identity; inspect %s permissions and retry: %w", providerPath, err)
	}
	for _, entry := range entries {
		if !entry.IsDir() || entry.Name() == newAccount {
			continue
		}
		slotPath := filepath.Join(providerPath, entry.Name())
		metadataContents, metadataErr := os.ReadFile(filepath.Join(slotPath, slotMetadataFilename))
		var metadata slotMetadata
		if metadataErr == nil {
			metadataErr = json.Unmarshal(metadataContents, &metadata)
		}
		if metadataErr != nil {
			return "", fmt.Errorf("check whether Claude account %q has this identity; repair or remove its metadata before retrying: %w", entry.Name(), metadataErr)
		}
		if email != "" && metadata.Email != "" && strings.EqualFold(email, metadata.Email) {
			return entry.Name(), nil
		}
		path, _ := manager.vault.CredentialsPath("claude", entry.Name())
		existing, err := (claude.FileStore{Path: path}).Read()
		if err != nil {
			return "", fmt.Errorf("check whether Claude account %q has this identity; repair or remove its slot before retrying: %w", entry.Name(), err)
		}
		if existing.RefreshToken == credentials.RefreshToken && existing.AccessToken == credentials.AccessToken {
			return entry.Name(), nil
		}
	}
	return "", nil
}

func (manager loginManager) restoreClaudeLogin(ctx context.Context, credentials claude.Credentials) error {
	wait := manager.restoreWait
	if wait <= 0 {
		wait = claudeRestoreTimeout
	}
	restoreContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), wait)
	defer cancel()
	return manager.claudeLive.Write(restoreContext, credentials)
}

func (manager loginManager) beginClaudeStaging(activeAccount string) error {
	path := filepath.Join(manager.vault.Root(), claudeStagingFilename)
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if errors.Is(err, os.ErrExist) {
		return fmt.Errorf("another Claude enrollment transaction exists at %s; rerun hop to recover it before starting a new login", path)
	}
	if err != nil {
		return fmt.Errorf("record how to restore active Claude account %q before its live login is cleared; the live login was not changed, check %s permissions and retry: %w", activeAccount, manager.vault.Root(), err)
	}
	contents, encodeErr := json.Marshal(claudeStagingRecord{ActiveAccount: activeAccount, ProcessID: os.Getpid(), CreatedAt: time.Now().UTC()})
	if encodeErr == nil {
		contents = append(contents, '\n')
		_, encodeErr = file.Write(contents)
	}
	if encodeErr == nil {
		encodeErr = file.Sync()
	}
	closeErr := file.Close()
	if encodeErr == nil {
		encodeErr = closeErr
	}
	if encodeErr != nil {
		_ = os.Remove(path)
		return fmt.Errorf("record how to restore active Claude account %q before its live login is cleared; the live login was not changed, retry: %w", activeAccount, encodeErr)
	}
	return nil
}

func (manager loginManager) finishClaudeStaging(ctx context.Context, credentials claude.Credentials) error {
	if err := manager.restoreClaudeLogin(ctx, credentials); err != nil {
		return err
	}
	path := filepath.Join(manager.vault.Root(), claudeStagingFilename)
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("clear completed Claude enrollment transaction %s; the active login was restored, remove this file before the next login: %w", path, err)
	}
	return nil
}

func (manager loginManager) recoverInterruptedClaudeLogin(ctx context.Context) (string, error) {
	record, found, err := readClaudeStagingRecord(manager.vault.Root())
	if !found && err == nil {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if processIsRunning(record.ProcessID) && record.ProcessID != os.Getpid() {
		return "", fmt.Errorf("claude enrollment is already running in process %d; wait for it to finish before retrying", record.ProcessID)
	}
	credentialsPath, err := manager.vault.CredentialsPath("claude", record.ActiveAccount)
	if err != nil {
		return "", err
	}
	credentials, err := (claude.FileStore{Path: credentialsPath}).Read()
	if err != nil {
		return "", fmt.Errorf("recover interrupted Claude enrollment from active account %q; repair its slot at %s before retrying: %w", record.ActiveAccount, credentialsPath, err)
	}
	if err := manager.finishClaudeStaging(ctx, credentials); err != nil {
		return "", fmt.Errorf("recover interrupted Claude enrollment by restoring account %q; stop using Claude and retry hop: %w", record.ActiveAccount, err)
	}
	return record.ActiveAccount, nil
}

func readClaudeStagingRecord(root string) (claudeStagingRecord, bool, error) {
	path := filepath.Join(root, claudeStagingFilename)
	contents, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return claudeStagingRecord{}, false, nil
	}
	if err != nil {
		return claudeStagingRecord{}, false, fmt.Errorf("read interrupted Claude enrollment transaction %s; check its permissions and retry: %w", path, err)
	}
	var record claudeStagingRecord
	if err := json.Unmarshal(contents, &record); err != nil || record.ActiveAccount == "" || record.ProcessID <= 0 {
		return claudeStagingRecord{}, true, fmt.Errorf("read interrupted Claude enrollment transaction %s; expected active_account and pid, restore the active slot manually before removing this file: %w", path, errors.Join(err, errors.New("invalid Claude staging record")))
	}
	return record, true, nil
}

func (manager loginManager) confirmActiveClaudeSlot(ctx context.Context, accountName string) error {
	credentials, err := manager.claudeLive.Read(ctx)
	if err != nil {
		return fmt.Errorf("read the current Claude login before confirming account %q; unlock Keychain and retry: %w", accountName, err)
	}
	email, err := manager.claudeEmail(ctx)
	if err != nil {
		return fmt.Errorf("read the current Claude account email before confirming account %q; run 'claude auth status --json' and retry: %w", accountName, err)
	}
	if err := manager.saveClaudeSlot(accountName, email, credentials); err != nil {
		return fmt.Errorf("confirm the current live Claude login as account %q; the live login was not changed: %w", accountName, err)
	}
	_, err = fmt.Fprintf(manager.stdout, "Confirmed the current live Claude login as account %q%s.\n", accountName, emailSuffix(email))
	return err
}

func (manager loginManager) confirmRecordedClaudeIdentity(accountName, liveEmail string, liveCredentials claude.Credentials) error {
	slotPath, err := manager.vault.SlotPath("claude", accountName)
	if err != nil {
		return err
	}
	contents, err := os.ReadFile(filepath.Join(slotPath, slotMetadataFilename))
	if err != nil {
		return fmt.Errorf("confirm which login belongs to active Claude account %q; run 'hop login claude %s' first to explicitly adopt the current live login: %w", accountName, accountName, err)
	}
	var metadata slotMetadata
	if err := json.Unmarshal(contents, &metadata); err != nil {
		return fmt.Errorf("confirm which login belongs to active Claude account %q; run 'hop login claude %s' first to repair its metadata: %w", accountName, accountName, err)
	}
	if metadata.Email != "" && liveEmail != "" {
		if strings.EqualFold(metadata.Email, liveEmail) {
			return nil
		}
		return fmt.Errorf("live Claude is signed in as %s, but hop state names %q as %s; restore the recorded account or run 'hop login claude %s' to explicitly adopt the current live login before adding another account", liveEmail, accountName, metadata.Email, accountName)
	}
	credentialsPath := filepath.Join(slotPath, vault.CredentialsFilename)
	recordedCredentials, err := (claude.FileStore{Path: credentialsPath}).Read()
	if err != nil {
		return fmt.Errorf("confirm credentials for active Claude account %q; run 'hop login claude %s' first to repair its slot: %w", accountName, accountName, err)
	}
	if recordedCredentials.RefreshToken == liveCredentials.RefreshToken && recordedCredentials.AccessToken == liveCredentials.AccessToken {
		return nil
	}
	return fmt.Errorf("claude auth status did not provide an email and the live credentials no longer match active account %q; run 'hop login claude %s' to explicitly confirm the current live login, then retry the new account", accountName, accountName)
}

func acquireClaudeLoginLock(ctx context.Context, root string) (func(), error) {
	return acquireLoginLock(ctx, root, claudeLoginLockFilename, "Claude enrollment or removal")
}

func acquireLoginLock(ctx context.Context, root, filename, operation string) (func(), error) {
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("create hop data directory %s before %s: %w", root, operation, err)
	}
	lockPath := filepath.Join(root, filename)
	deadline := time.NewTimer(refreshLockWait)
	defer deadline.Stop()
	ticker := time.NewTicker(refreshLockPoll)
	defer ticker.Stop()
	for {
		release, acquired, err := tryAcquireFileLock(lockPath)
		if err != nil {
			return nil, fmt.Errorf("lock %s at %s; check its permissions and retry: %w", operation, lockPath, err)
		}
		if acquired {
			return release, nil
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("wait for another %s to finish; retry the command: %w", operation, ctx.Err())
		case <-deadline.C:
			return nil, fmt.Errorf("wait for another %s to finish; it did not finish within %s, retry the command: login lock timeout", operation, refreshLockWait)
		case <-ticker.C:
		}
	}
}

func (manager loginManager) reserveNewSlot(providerName, accountName string) (*slotReservation, error) {
	slotPath, err := manager.vault.SlotPath(providerName, accountName)
	if err != nil {
		return nil, err
	}
	providerPath := filepath.Dir(slotPath)
	for _, directory := range []string{manager.vault.Root(), providerPath} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			return nil, fmt.Errorf("prepare %s account storage at %s: %w", providerName, directory, err)
		}
		if err := os.Chmod(directory, 0o700); err != nil {
			return nil, fmt.Errorf("secure %s account storage at %s: %w", providerName, directory, err)
		}
	}
	owner, err := newReservationOwner()
	if err != nil {
		return nil, fmt.Errorf("create an ownership token for %s account %q enrollment; retry login: %w", providerName, accountName, err)
	}
	if err := manager.createReservedSlot(slotPath, owner); errors.Is(err, os.ErrExist) {
		found, active, inspectErr := inspectSlotReservation(slotPath)
		if inspectErr != nil {
			return nil, fmt.Errorf("inspect the existing %s account %q enrollment; run 'hop rm %s %s' to remove the abandoned slot, then retry: %w", providerName, accountName, providerName, accountName, inspectErr)
		}
		if !found {
			return nil, fmt.Errorf("%s account %q already exists; run 'hop rm %s %s' before enrolling it again", providerName, accountName, providerName, accountName)
		}
		if active {
			return nil, fmt.Errorf("%s account %q is being enrolled by another hop process; wait for that login to finish, then retry", providerName, accountName)
		}
		if err := os.RemoveAll(slotPath); err != nil {
			return nil, fmt.Errorf("remove the abandoned %s account %q enrollment; check %s permissions and retry: %w", providerName, accountName, slotPath, err)
		}
		if err := manager.createReservedSlot(slotPath, owner); err != nil {
			return nil, fmt.Errorf("reserve %s account slot %s after removing its abandoned enrollment; retry login: %w", providerName, slotPath, err)
		}
	} else if err != nil {
		return nil, fmt.Errorf("reserve %s account slot %s; check its permissions and retry: %w", providerName, slotPath, err)
	}
	return &slotReservation{path: slotPath, owner: owner}, nil
}

func (manager loginManager) createReservedSlot(slotPath, owner string) error {
	if err := os.Mkdir(slotPath, 0o700); err != nil {
		return err
	}
	markerPath := filepath.Join(slotPath, slotReservationFilename)
	marker, err := os.OpenFile(markerPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		_ = os.Remove(slotPath)
		return err
	}
	contents, err := json.Marshal(slotReservationRecord{ProcessID: os.Getpid(), CreatedAt: time.Now().UTC(), Owner: owner})
	if err == nil {
		contents = append(contents, '\n')
		_, err = marker.Write(contents)
	}
	if err == nil {
		err = marker.Sync()
	}
	closeErr := marker.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		_ = os.Remove(markerPath)
		_ = os.Remove(slotPath)
		return err
	}
	return nil
}

func newReservationOwner() (string, error) {
	value := make([]byte, 16)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return hex.EncodeToString(value), nil
}

func inspectSlotReservation(slotPath string) (bool, bool, error) {
	contents, err := os.ReadFile(filepath.Join(slotPath, slotReservationFilename))
	if errors.Is(err, os.ErrNotExist) {
		return false, false, nil
	}
	if err != nil {
		return false, false, err
	}
	var record slotReservationRecord
	if err := json.Unmarshal(contents, &record); err != nil {
		return true, false, fmt.Errorf("decode the reservation owner: %w", err)
	}
	if record.ProcessID <= 0 || record.CreatedAt.IsZero() {
		return true, false, errors.New("reservation owner is missing its process ID or creation time")
	}
	return true, processIsRunning(record.ProcessID), nil
}

func (manager loginManager) installClaudeSlot(accountName, slotPath, email string, credentials claude.Credentials) error {
	return manager.installSlot("claude", accountName, slotPath, email, func(credentialsPath string) error {
		return (claude.FileStore{Path: credentialsPath}).Write(credentials)
	})
}

func (manager loginManager) saveClaudeSlot(accountName, email string, credentials claude.Credentials) error {
	credentialsPath, err := manager.vault.CredentialsPath("claude", accountName)
	if err != nil {
		return err
	}
	if err := (claude.FileStore{Path: credentialsPath}).Write(credentials); err != nil {
		return err
	}
	return writeManagedSlotMetadata(filepath.Dir(credentialsPath), email)
}

func (manager loginManager) installCodexSlot(accountName, slotPath string, credentials codex.Credentials, email string) error {
	return manager.installSlot("codex", accountName, slotPath, email, func(credentialsPath string) error {
		return (codex.FileStore{Path: credentialsPath}).Write(credentials)
	})
}

func (manager loginManager) installSlot(providerName, accountName, slotPath, email string, writeCredentials func(string) error) error {
	credentialsPath := filepath.Join(slotPath, vault.CredentialsFilename)
	if err := writeCredentials(credentialsPath); err != nil {
		return fmt.Errorf("save %s account %q credentials; the incomplete slot was removed, retry login: %w", providerName, accountName, err)
	}
	if err := writeManagedSlotMetadata(slotPath, email); err != nil {
		return fmt.Errorf("save %s account %q metadata; the incomplete slot was removed, retry login: %w", providerName, accountName, err)
	}
	return nil
}

func (reservation *slotReservation) Commit() error {
	reservation.keep = true
	markerPath := filepath.Join(reservation.path, slotReservationFilename)
	if err := os.Remove(markerPath); err != nil {
		return fmt.Errorf("finish account enrollment at %s; credentials are installed, remove %s before retrying: %w", reservation.path, markerPath, err)
	}
	return nil
}

func (reservation *slotReservation) Cleanup() {
	if reservation == nil || reservation.keep {
		return
	}
	contents, err := os.ReadFile(filepath.Join(reservation.path, slotReservationFilename))
	if err != nil {
		return
	}
	var record slotReservationRecord
	if err := json.Unmarshal(contents, &record); err != nil || record.Owner != reservation.owner {
		return
	}
	_ = os.RemoveAll(reservation.path)
}

func (systemLoginRunner) Run(ctx context.Context, request loginCommand) error {
	command := exec.CommandContext(ctx, request.Name, request.Args...)
	command.Env = environmentWithOverrides(os.Environ(), request.Env)
	command.Stdin = request.Stdin
	command.Stdout = request.Stdout
	command.Stderr = request.Stderr
	if err := command.Run(); err != nil {
		return fmt.Errorf("run %s %s: %w", request.Name, strings.Join(request.Args, " "), err)
	}
	return nil
}

func environmentWithOverrides(environment []string, overrides map[string]string) []string {
	result := make([]string, 0, len(environment)+len(overrides))
	for _, entry := range environment {
		key, _, found := strings.Cut(entry, "=")
		if _, overridden := overrides[key]; found && overridden {
			continue
		}
		result = append(result, entry)
	}
	for key, value := range overrides {
		result = append(result, key+"="+value)
	}
	return result
}

func claudeAccountEmail(ctx context.Context) (string, error) {
	command := exec.CommandContext(ctx, "claude", "auth", "status", "--json")
	output, err := command.Output()
	if err != nil {
		return "", fmt.Errorf("read Claude login status: %w", err)
	}
	return parseClaudeAccountEmail(output)
}

func parseClaudeAccountEmail(output []byte) (string, error) {
	var status struct {
		LoggedIn bool   `json:"loggedIn"`
		Email    string `json:"email"`
	}
	if err := json.Unmarshal(output, &status); err != nil {
		return "", fmt.Errorf("read Claude login email from auth status: %w", err)
	}
	if !status.LoggedIn {
		return "", errors.New("claude auth status reports no login; run 'claude auth login' and retry")
	}
	return strings.TrimSpace(status.Email), nil
}

func emailSuffix(email string) string {
	if email == "" {
		return ""
	}
	return " (" + email + ")"
}

func (systemClaudeLiveStore) Read(ctx context.Context) (claude.Credentials, error) {
	return claude.ReadLiveCredentials(ctx)
}

func (systemClaudeLiveStore) Write(ctx context.Context, credentials claude.Credentials) error {
	return claude.WriteLiveCredentials(ctx, credentials)
}

func (systemClaudeLiveStore) Clear(ctx context.Context) error {
	return claude.ClearLiveCredentials(ctx)
}

func (systemClaudeLiveStore) ClearIfMatches(ctx context.Context, expected claude.Credentials) error {
	return claude.ClearLiveCredentialsIfMatches(ctx, expected)
}
