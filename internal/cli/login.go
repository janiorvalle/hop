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
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/janiorvalle/hop/internal/provider/claude"
	"github.com/janiorvalle/hop/internal/provider/codex"
	"github.com/janiorvalle/hop/internal/state"
	"github.com/janiorvalle/hop/internal/vault"
	"golang.org/x/term"
)

const slotReservationFilename = ".login-reservation"
const claudeLoginLockFilename = ".claude-login.lock"
const codexLoginLockFilename = ".codex-login.lock"
const claudeLoginTimeout = 10 * time.Minute

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
}

type loginManager struct {
	vault         vault.Vault
	runner        loginRunner
	claudeLive    claudeLiveStore
	stdout        io.Writer
	stderr        io.Writer
	codexEmail    func(context.Context, codex.Credentials) (string, error)
	claudeEmail   func(context.Context) (string, error)
	claudeProfile func(context.Context, claude.Credentials) (claude.Profile, error)
	claudeLogin   func(context.Context) (claude.Enrollment, error)
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

func loginAccount(ctx context.Context, providerName, accountName string, stdin io.Reader, stdout, stderr io.Writer) error {
	accountVault, err := defaultVault()
	if err != nil {
		return err
	}
	claudeDependencies := defaultClaudeLiveDependencies()
	claudeLogin, err := defaultClaudeLogin(stderr)
	if err != nil {
		return err
	}
	manager := loginManager{
		vault:      accountVault,
		runner:     systemLoginRunner{},
		claudeLive: claudeDependencies.store,
		stdout:     stdout,
		stderr:     stderr,
		codexEmail: func(ctx context.Context, credentials codex.Credentials) (string, error) {
			usage, err := codex.New(codex.Config{}).FetchUsage(ctx, credentials)
			return usage.Email, err
		},
		claudeEmail:   claudeDependencies.email,
		claudeProfile: claudeDependencies.profile,
		claudeLogin:   claudeLogin,
	}
	return manager.Login(ctx, providerName, accountName, stdin)
}

func defaultClaudeLogin(stderr io.Writer) (func(context.Context) (claude.Enrollment, error), error) {
	port := claude.DefaultLoginPort
	if override := strings.TrimSpace(os.Getenv(claudeLoginPortOverride)); override != "" {
		parsed, err := strconv.Atoi(override)
		if err != nil || parsed < 1 || parsed > 65535 {
			return nil, fmt.Errorf("%s=%q is not a port between 1 and 65535; fix the override and retry", claudeLoginPortOverride, override)
		}
		port = parsed
	}
	login := claude.Login{Port: port, OpenBrowser: func(authorizeURL string) error {
		_, _ = fmt.Fprintf(stderr, "Opening your browser to sign in to Claude. If nothing opens, paste this URL into a browser:\n%s\n", authorizeURL)
		opener := browserCommand(authorizeURL)
		if err := opener.Start(); err != nil {
			_, _ = fmt.Fprintf(stderr, "hop: could not open a browser (%v); paste the URL above yourself\n", err)
		} else {
			go func() { _ = opener.Wait() }()
		}
		_, _ = fmt.Fprintln(stderr, "Waiting for the sign-in to finish. Press Ctrl-C to cancel.")
		return nil
	}}
	adapter := defaultClaudeAdapter()
	return func(ctx context.Context) (claude.Enrollment, error) {
		loginContext, cancel := context.WithTimeout(ctx, claudeLoginTimeout)
		defer cancel()
		return adapter.Login(loginContext, login)
	}, nil
}

// browserCommand honors BROWSER the way xdg-open, gh, and git do, then falls
// back to the platform opener.
func browserCommand(authorizeURL string) *exec.Cmd {
	if browser := strings.Fields(os.Getenv("BROWSER")); len(browser) > 0 {
		return exec.Command(browser[0], append(browser[1:], authorizeURL)...)
	}
	switch runtime.GOOS {
	case "darwin":
		return exec.Command("open", authorizeURL)
	case "windows":
		return exec.Command("rundll32", "url.dll,FileProtocolHandler", authorizeURL)
	default:
		return exec.Command("xdg-open", authorizeURL)
	}
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
		return manager.loginClaude(ctx, accountName)
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

func (manager loginManager) loginClaude(ctx context.Context, accountName string) error {
	releaseClaudeLogin, err := acquireClaudeLoginLock(ctx, manager.vault.Root())
	if err != nil {
		return err
	}
	defer releaseClaudeLogin()
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
		identity, err := manager.resolveClaudeIdentity(ctx, credentials, email)
		if err != nil {
			return fmt.Errorf("claude enrollment stopped before account %q was saved: %w", accountName, err)
		}
		if duplicateAccount, err := manager.duplicateClaudeAccount(accountName, identity, credentials); err != nil {
			return err
		} else if duplicateAccount != "" {
			return fmt.Errorf("claude identity is already enrolled as account %q; use that account or remove it before assigning a new name", duplicateAccount)
		}
		if err := manager.installClaudeSlot(accountName, reservation.path, identity, credentials); err != nil {
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
		_, err = fmt.Fprintf(manager.stdout, "Enrolled the current live Claude login as account %q%s.\n", accountName, emailSuffix(identity.Email))
		return err
	}
	if accountName == activeAccount {
		return manager.confirmActiveClaudeSlot(ctx, activeAccount)
	}
	return manager.loginClaudeInBrowser(ctx, accountName)
}

// loginClaudeInBrowser enrolls another Claude account through hop's own
// browser sign-in, so the live login and every running session stay untouched.
func (manager loginManager) loginClaudeInBrowser(ctx context.Context, accountName string) error {
	reservation, err := manager.reserveNewSlot("claude", accountName)
	if err != nil {
		return err
	}
	defer reservation.Cleanup()
	enrollment, err := manager.claudeLogin(ctx)
	if errors.Is(err, claude.ErrCallbackPort) {
		return fmt.Errorf("claude enrollment for account %q stopped before anything was saved; set %s to a free port and retry: %w", accountName, claudeLoginPortOverride, err)
	}
	if err != nil {
		return fmt.Errorf("claude enrollment for account %q stopped before anything was saved: %w", accountName, err)
	}
	if duplicateAccount, err := manager.duplicateClaudeAccount(accountName, enrollment.Profile, enrollment.Credentials); err != nil {
		return err
	} else if duplicateAccount != "" {
		return fmt.Errorf("claude identity is already enrolled as account %q; use that account or remove it before assigning a new name", duplicateAccount)
	}
	if err := manager.installClaudeSlot(accountName, reservation.path, enrollment.Profile, enrollment.Credentials); err != nil {
		return err
	}
	if err := reservation.Commit(); err != nil {
		return err
	}
	_, err = fmt.Fprintf(manager.stdout, "Enrolled Claude account %q%s.\n", accountName, emailSuffix(enrollment.Profile.Email))
	return err
}

// awaitConfirmation reads a y/N answer unless ctx ends first, so a signal at
// the prompt unwinds the command and releases its locks.
func awaitConfirmation(ctx context.Context, stdin io.Reader) (bool, error) {
	type confirmationResult struct {
		approved bool
		err      error
	}
	result := make(chan confirmationResult, 1)
	go func() {
		approved, err := readConfirmation(stdin)
		result <- confirmationResult{approved: approved, err: err}
	}()
	select {
	case <-ctx.Done():
		return false, ctx.Err()
	case confirmation := <-result:
		return confirmation.approved, confirmation.err
	}
}

func readConfirmation(reader io.Reader) (bool, error) {
	isYes := true
	hasAnswer := false
	var nextByte [1]byte
	for {
		if _, err := io.ReadFull(reader, nextByte[:]); err != nil {
			return false, err
		}
		switch nextByte[0] {
		case '\n':
			return isYes && hasAnswer, nil
		case 'y', 'Y':
			if hasAnswer {
				isYes = false
			}
			hasAnswer = true
		case ' ', '\t', '\r':
		default:
			isYes = false
		}
	}
}

func readerIsTerminal(reader io.Reader) bool {
	inputFile, ok := reader.(*os.File)
	return ok && term.IsTerminal(int(inputFile.Fd()))
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

func (manager loginManager) duplicateClaudeAccount(newAccount string, identity claude.Profile, credentials claude.Credentials) (string, error) {
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
		if identity.AccountUUID != "" && metadata.AccountUUID == identity.AccountUUID {
			return entry.Name(), nil
		}
		if identity.Email != "" && metadata.Email != "" && strings.EqualFold(identity.Email, metadata.Email) {
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

func (manager loginManager) resolveClaudeIdentity(ctx context.Context, credentials claude.Credentials, fallbackEmail string) (claude.Profile, error) {
	identity := claude.Profile{Email: strings.TrimSpace(fallbackEmail)}
	profile, found, err := manager.freshClaudeProfile(ctx, credentials)
	if err != nil {
		return claude.Profile{}, err
	}
	if found {
		return profile, nil
	}
	return identity, nil
}

func (manager loginManager) freshClaudeProfile(ctx context.Context, credentials claude.Credentials) (claude.Profile, bool, error) {
	if manager.claudeProfile == nil || !credentials.HasScope("user:profile") {
		return claude.Profile{}, false, nil
	}
	profile, err := manager.claudeProfile(ctx, credentials)
	if err != nil {
		if ctx.Err() != nil {
			return claude.Profile{}, false, ctx.Err()
		}
		return claude.Profile{}, false, nil
	}
	return profile, true, nil
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
	// When the slot already holds exactly these credentials, nothing about the
	// account changed and its recorded email outranks `claude auth status`,
	// whose cached email still names the previous account right after a hop
	// switch. Stamping that stale email here would poison a healthy slot.
	identity, err := manager.resolveClaudeIdentity(ctx, credentials, email)
	if err != nil {
		return fmt.Errorf("claude confirmation stopped before account %q was saved: %w", accountName, err)
	}
	if recordedIdentity, recorded := manager.recordedClaudeSlotIdentity(accountName, credentials); recorded {
		if recordedIdentity.Email != "" && identity.AccountUUID == "" {
			identity.Email = recordedIdentity.Email
		}
		if identity.AccountUUID == "" {
			identity.AccountUUID = recordedIdentity.AccountUUID
		}
	}
	if err := manager.saveClaudeSlot(accountName, identity, credentials); err != nil {
		return fmt.Errorf("confirm the current live Claude login as account %q; the live login was not changed: %w", accountName, err)
	}
	_, err = fmt.Fprintf(manager.stdout, "Confirmed the current live Claude login as account %q%s.\n", accountName, emailSuffix(identity.Email))
	return err
}

// recordedClaudeSlotIdentity reports whether the slot hop recorded for
// accountName holds exactly the live credentials, and with it the account
// identity recorded there.
//
// Only a slot hop enrolled — one whose metadata records the managed refresh
// policy — can match. Slots are default-deny: one seeded by hand stays
// read-only until 'hop login' takes custody of it, so matching tokens alone
// must not let a caller adopt it and promote it to managed; the caller keeps
// its explicit-adoption error instead. Callers that explain a non-match read
// the slot themselves so the failure keeps its repair instructions.
func (manager loginManager) recordedClaudeSlotIdentity(accountName string, liveCredentials claude.Credentials) (claude.Profile, bool) {
	slotPath, err := manager.vault.SlotPath("claude", accountName)
	if err != nil {
		return claude.Profile{}, false
	}
	recorded, err := (claude.FileStore{Path: filepath.Join(slotPath, vault.CredentialsFilename)}).Read()
	if err != nil {
		return claude.Profile{}, false
	}
	if recorded.AccessToken != liveCredentials.AccessToken || recorded.RefreshToken != liveCredentials.RefreshToken {
		return claude.Profile{}, false
	}
	contents, err := os.ReadFile(filepath.Join(slotPath, slotMetadataFilename))
	if err != nil {
		return claude.Profile{}, false
	}
	var metadata slotMetadata
	if err := json.Unmarshal(contents, &metadata); err != nil || metadata.RefreshPolicy != managedRefreshPolicy {
		return claude.Profile{}, false
	}
	return claude.Profile{AccountUUID: strings.TrimSpace(metadata.AccountUUID), Email: strings.TrimSpace(metadata.Email)}, true
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

func (manager loginManager) installClaudeSlot(accountName, slotPath string, identity claude.Profile, credentials claude.Credentials) error {
	return manager.installSlot("claude", accountName, slotPath, slotMetadata{
		RefreshPolicy: managedRefreshPolicy,
		Email:         identity.Email,
		AccountUUID:   identity.AccountUUID,
	}, func(credentialsPath string) error {
		return (claude.FileStore{Path: credentialsPath}).Write(credentials)
	})
}

func (manager loginManager) saveClaudeSlot(accountName string, identity claude.Profile, credentials claude.Credentials) error {
	if err := manager.saveClaudeCredentials(accountName, credentials); err != nil {
		return err
	}
	credentialsPath, err := manager.vault.CredentialsPath("claude", accountName)
	if err != nil {
		return err
	}
	return writeManagedSlotMetadata(filepath.Dir(credentialsPath), identity)
}

func (manager loginManager) saveClaudeCredentials(accountName string, credentials claude.Credentials) error {
	credentialsPath, err := manager.vault.CredentialsPath("claude", accountName)
	if err != nil {
		return err
	}
	return (claude.FileStore{Path: credentialsPath}).Write(credentials)
}

func (manager loginManager) installCodexSlot(accountName, slotPath string, credentials codex.Credentials, email string) error {
	return manager.installSlot("codex", accountName, slotPath, slotMetadata{RefreshPolicy: managedRefreshPolicy, Email: email}, func(credentialsPath string) error {
		return (codex.FileStore{Path: credentialsPath}).Write(credentials)
	})
}

func (manager loginManager) installSlot(providerName, accountName, slotPath string, metadata slotMetadata, writeCredentials func(string) error) error {
	credentialsPath := filepath.Join(slotPath, vault.CredentialsFilename)
	if err := writeCredentials(credentialsPath); err != nil {
		return fmt.Errorf("save %s account %q credentials; the incomplete slot was removed, retry login: %w", providerName, accountName, err)
	}
	if err := writeSlotMetadata(slotPath, metadata); err != nil {
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

func (systemClaudeLiveStore) ClearIfMatches(ctx context.Context, expected claude.Credentials) error {
	return claude.ClearLiveCredentialsIfMatches(ctx, expected)
}
