package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/janiorvalle/hop/internal/provider/claude"
	"github.com/janiorvalle/hop/internal/provider/codex"
	"github.com/janiorvalle/hop/internal/state"
	"github.com/janiorvalle/hop/internal/vault"
)

const (
	claudeCredentialsFileOverride = "HOP_CLAUDE_CREDENTIALS_FILE"
	claudeAccountEmailOverride    = "HOP_CLAUDE_ACCOUNT_EMAIL"
	codexAuthFileOverride         = "HOP_CODEX_AUTH_FILE"
	switchTransactionFilename     = ".switch-transaction.json"
)

type codexLiveStore interface {
	Read() (codex.Credentials, error)
	Write(codex.Credentials) error
}

type activeStateStore interface {
	Load() (state.State, error)
	Save(state.State) error
}

type fileActiveStateStore struct{ root string }

type switchManager struct {
	vault       vault.Vault
	state       activeStateStore
	claudeLive  claudeLiveStore
	codexLive   codexLiveStore
	claudeEmail func(context.Context) (string, error)
	// claudeTarget and codexTarget describe where live credentials are written
	// ("system" or a file path); recovery refuses a transaction recorded
	// against different targets so a sandbox switch can never restore into the
	// real Keychain or ~/.codex/auth.json.
	claudeTarget string
	codexTarget  string
	stdout       io.Writer
}

type switchStep struct {
	provider       string
	previous       string
	target         string
	hadActiveState bool
	copyBack       func() error
	install        func(context.Context) error
	rollback       func(context.Context) error
}

type switchTransaction struct {
	Steps      []switchTransactionStep `json:"steps"`
	ClaudeLive string                  `json:"claude_live,omitempty"`
	CodexLive  string                  `json:"codex_live,omitempty"`
	Committed  bool                    `json:"committed,omitempty"`
}

type switchTransactionStep struct {
	Provider       string `json:"provider"`
	Previous       string `json:"previous"`
	Target         string `json:"target"`
	HadActiveState bool   `json:"had_active_state"`
}

func switchAccount(ctx context.Context, providerName, accountName string, stdout io.Writer) error {
	manager, err := defaultSwitchManager(stdout)
	if err != nil {
		return err
	}
	return manager.Switch(ctx, providerName, accountName)
}

func recoverDefaultSwitch(ctx context.Context, stdout io.Writer) error {
	manager, err := defaultSwitchManager(stdout)
	if err != nil {
		return err
	}
	releaseProviders, err := manager.lockProviders(ctx)
	if err != nil {
		return err
	}
	defer releaseProviders()
	releaseState, err := acquireStateLock(ctx, manager.vault.Root())
	if err != nil {
		return err
	}
	defer releaseState()
	recovered, err := manager.recoverInterruptedSwitch(ctx)
	if recovered {
		_, _ = io.WriteString(stdout, "Recovered an interrupted account switch before continuing.\n")
	}
	return err
}

func showAccountsSafely(ctx context.Context, stdout, stderr io.Writer, asJSON bool) error {
	manager, err := defaultSwitchManager(stdout)
	if err != nil {
		return err
	}
	releaseProviders, err := manager.lockProviders(ctx)
	if err != nil {
		return err
	}
	defer releaseProviders()
	releaseState, err := acquireStateLock(ctx, manager.vault.Root())
	if err != nil {
		return err
	}
	defer releaseState()
	recovered, err := manager.recoverInterruptedSwitch(ctx)
	if err != nil {
		return err
	}
	if recovered {
		_, _ = io.WriteString(stderr, "hop: recovered an interrupted account switch before continuing\n")
	}
	return showAccounts(ctx, stdout, asJSON)
}

func defaultSwitchManager(stdout io.Writer) (switchManager, error) {
	accountVault, err := defaultVault()
	if err != nil {
		return switchManager{}, err
	}
	claudeLive, claudeEmail := defaultClaudeSwitchStore()
	claudeTarget, err := defaultClaudeTarget()
	if err != nil {
		return switchManager{}, err
	}
	codexLive, codexTarget, err := defaultCodexSwitchStore()
	if err != nil {
		return switchManager{}, err
	}
	return switchManager{
		vault:        accountVault,
		state:        fileActiveStateStore{root: accountVault.Root()},
		claudeLive:   claudeLive,
		codexLive:    codexLive,
		claudeEmail:  claudeEmail,
		claudeTarget: claudeTarget,
		codexTarget:  codexTarget,
		stdout:       stdout,
	}, nil
}

func defaultClaudeTarget() (string, error) {
	if path := strings.TrimSpace(os.Getenv(claudeCredentialsFileOverride)); path != "" {
		absolute, err := filepath.Abs(path)
		if err != nil {
			return "", fmt.Errorf("resolve %s=%s to an absolute path; fix the override and retry: %w", claudeCredentialsFileOverride, path, err)
		}
		return absolute, nil
	}
	return claude.LiveCredentialsTarget()
}

func defaultClaudeSwitchStore() (claudeLiveStore, func(context.Context) (string, error)) {
	if path := strings.TrimSpace(os.Getenv(claudeCredentialsFileOverride)); path != "" {
		return claudeFileLiveStore{store: claude.LiveFile{Path: path}}, func(context.Context) (string, error) {
			email := strings.TrimSpace(os.Getenv(claudeAccountEmailOverride))
			if email == "" {
				return "", fmt.Errorf("%s is required with %s so hop can verify which account owns the sandbox credentials", claudeAccountEmailOverride, claudeCredentialsFileOverride)
			}
			return email, nil
		}
	}
	return systemClaudeLiveStore{}, claudeAccountEmail
}

func defaultCodexSwitchStore() (codexLiveStore, string, error) {
	path := strings.TrimSpace(os.Getenv(codexAuthFileOverride))
	if path == "" {
		if codexHome := strings.TrimSpace(os.Getenv("CODEX_HOME")); codexHome != "" {
			path = filepath.Join(codexHome, "auth.json")
		} else {
			home, err := os.UserHomeDir()
			if err != nil {
				return nil, "", fmt.Errorf("find the home directory for live Codex credentials: %w", err)
			}
			path = filepath.Join(home, ".codex", "auth.json")
		}
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, "", fmt.Errorf("resolve the live Codex credential path %s; fix %s and retry: %w", path, codexAuthFileOverride, err)
	}
	return codex.LiveFile{Path: path}, absolute, nil
}

func (manager switchManager) Switch(ctx context.Context, providerName, accountName string) error {
	releaseProviders, err := manager.lockProviders(ctx)
	if err != nil {
		return err
	}
	defer releaseProviders()

	releaseState, err := acquireStateLock(ctx, manager.vault.Root())
	if err != nil {
		return err
	}
	defer releaseState()

	recovered, err := manager.recoverInterruptedSwitch(ctx)
	if err != nil {
		return err
	}
	if recovered {
		_, _ = io.WriteString(manager.stdout, "Recovered an interrupted account switch before continuing.\n")
	}
	providers, err := manager.providersFor(providerName, accountName)
	if err != nil {
		return err
	}
	activeState, err := manager.state.Load()
	if err != nil {
		return err
	}
	steps, releases, err := manager.prepareSteps(ctx, providers, accountName, activeState)
	if err != nil {
		return err
	}
	defer releaseAll(releases)

	for _, step := range steps {
		if err := step.copyBack(); err != nil {
			return fmt.Errorf("save the current live %s credentials before switching; the live login was not changed, repair the active slot and retry: %w", step.provider, err)
		}
	}
	transaction := transactionFor(steps)
	if err := manager.writeSwitchTransaction(transaction); err != nil {
		return fmt.Errorf("record the switch before changing live credentials; the live login was not changed, check %s permissions and retry: %w", manager.vault.Root(), err)
	}

	installed := make([]switchStep, 0, len(steps))
	for _, step := range steps {
		if err := step.install(ctx); err != nil {
			rollbackErr := rollbackSteps(ctx, append(installed, step))
			if rollbackErr == nil {
				rollbackErr = manager.removeSwitchTransaction()
			}
			return switchFailure(step.provider, accountName, err, rollbackErr)
		}
		installed = append(installed, step)
	}

	for _, step := range steps {
		activeState.SetActive(step.provider, accountName)
	}
	if err := manager.state.Save(activeState); err != nil {
		rollbackErr := rollbackSteps(ctx, installed)
		previousState := previousState(activeState, transaction)
		rollbackErr = errors.Join(rollbackErr, manager.state.Save(previousState))
		if rollbackErr == nil {
			rollbackErr = manager.removeSwitchTransaction()
		}
		return switchFailure("active-account state", accountName, err, rollbackErr)
	}
	transaction.Committed = true
	if err := manager.writeSwitchTransaction(transaction); err != nil {
		rollbackErr := rollbackSteps(ctx, installed)
		rollbackErr = errors.Join(rollbackErr, manager.state.Save(previousState(activeState, transaction)))
		if rollbackErr == nil {
			rollbackErr = manager.removeSwitchTransaction()
		}
		return switchFailure("switch transaction", accountName, err, rollbackErr)
	}
	if err := manager.removeSwitchTransaction(); err != nil {
		return fmt.Errorf("finish the switch to account %q; credentials and state were updated, but %s could not be removed, retry hop to recover safely: %w", accountName, switchTransactionFilename, err)
	}

	for _, step := range steps {
		if _, err := fmt.Fprintf(manager.stdout, "Switched %s to account %q.\n", step.provider, accountName); err != nil {
			return err
		}
	}
	_, err = io.WriteString(manager.stdout, "Warning: stop running Claude and Codex sessions before hopping; a session already in progress may fail its next token refresh.\n")
	return err
}

func (manager switchManager) providersFor(providerName, accountName string) ([]string, error) {
	if providerName != "" {
		exists, err := manager.slotExists(providerName, accountName)
		if err != nil {
			return nil, err
		}
		if !exists {
			return nil, fmt.Errorf("%s account %q is not enrolled; run 'hop login %s %s', then retry", providerName, accountName, providerName, accountName)
		}
		return []string{providerName}, nil
	}

	providers := make([]string, 0, 2)
	for _, candidate := range []string{"claude", "codex"} {
		exists, err := manager.slotExists(candidate, accountName)
		if err != nil {
			return nil, err
		}
		if exists {
			providers = append(providers, candidate)
		}
	}
	if len(providers) == 0 {
		return nil, fmt.Errorf("account %q is not enrolled for Claude or Codex; run 'hop login claude %s' or 'hop login codex %s', then retry", accountName, accountName, accountName)
	}
	return providers, nil
}

func (manager switchManager) slotExists(providerName, accountName string) (bool, error) {
	path, err := manager.vault.CredentialsPath(providerName, accountName)
	if err != nil {
		return false, err
	}
	_, err = os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("inspect %s account %q at %s; check its permissions and retry: %w", providerName, accountName, path, err)
	}
	return true, nil
}

func (manager switchManager) lockProviders(ctx context.Context) (func(), error) {
	providers := []string{"claude", "codex"}
	releases := make([]func(), 0, len(providers))
	for _, providerName := range providers {
		filename := codexLoginLockFilename
		if providerName == "claude" {
			filename = claudeLoginLockFilename
		}
		release, err := acquireLoginLock(ctx, manager.vault.Root(), filename, providerName+" account change")
		if err != nil {
			releaseAll(releases)
			return nil, err
		}
		releases = append(releases, release)
	}
	return func() { releaseAll(releases) }, nil
}

func (manager switchManager) prepareSteps(ctx context.Context, providers []string, target string, activeState state.State) ([]switchStep, []func(), error) {
	steps := make([]switchStep, 0, len(providers))
	releases := make([]func(), 0, len(providers)*2)
	for _, providerName := range providers {
		current, hadActiveState := activeState.Active(providerName)
		if providerName == "codex" && !hadActiveState {
			liveCredentials, err := manager.codexLive.Read()
			if err != nil {
				releaseAll(releases)
				return nil, nil, fmt.Errorf("read the current live Codex credentials before switching; run 'codex login', then retry: %w", err)
			}
			current, err = manager.findCodexSlotByAccountID(liveCredentials.AccountID)
			if err != nil {
				releaseAll(releases)
				return nil, nil, err
			}
		}
		for _, accountName := range uniqueAccounts(current, target) {
			slotPath, err := manager.vault.SlotPath(providerName, accountName)
			if err != nil {
				releaseAll(releases)
				return nil, nil, err
			}
			release, err := acquireRefreshLock(ctx, slotPath)
			if err != nil {
				releaseAll(releases)
				return nil, nil, err
			}
			releases = append(releases, release)
		}

		var step switchStep
		var err error
		if providerName == "claude" {
			step, err = manager.prepareClaudeStep(ctx, current, target, hadActiveState)
		} else {
			step, err = manager.prepareCodexStep(current, target, hadActiveState)
		}
		if err != nil {
			releaseAll(releases)
			return nil, nil, err
		}
		steps = append(steps, step)
	}
	return steps, releases, nil
}

func (manager switchManager) prepareClaudeStep(ctx context.Context, current, target string, hadActiveState bool) (switchStep, error) {
	targetPath, err := manager.vault.CredentialsPath("claude", target)
	if err != nil {
		return switchStep{}, err
	}
	targetCredentials, err := (claude.FileStore{Path: targetPath}).Read()
	if err != nil {
		return switchStep{}, unusableSlotError("claude", target, err)
	}
	liveCredentials, err := manager.claudeLive.Read(ctx)
	if err != nil {
		return switchStep{}, fmt.Errorf("read the current live Claude credentials before switching; unlock Keychain or run 'claude auth login', then retry: %w", err)
	}
	if !hadActiveState {
		return switchStep{}, fmt.Errorf("no active Claude account is recorded, so hop cannot safely save the current live credentials; run 'hop login claude <account>' to adopt the current login, then retry")
	}
	if err := manager.confirmActiveClaudeIdentity(ctx, current, liveCredentials); err != nil {
		return switchStep{}, err
	}
	if current == target {
		targetCredentials = liveCredentials
	}
	copyBack := func() error { return nil }
	if current != "" {
		currentPath, err := manager.vault.CredentialsPath("claude", current)
		if err != nil {
			return switchStep{}, err
		}
		currentStore := claude.FileStore{Path: currentPath}
		copyBack = func() error { return currentStore.Write(liveCredentials) }
	}
	return switchStep{
		provider:       "claude",
		previous:       current,
		target:         target,
		hadActiveState: hadActiveState,
		copyBack:       copyBack,
		install:        func(ctx context.Context) error { return manager.claudeLive.Write(ctx, targetCredentials) },
		rollback:       func(ctx context.Context) error { return manager.claudeLive.Write(ctx, liveCredentials) },
	}, nil
}

func (manager switchManager) prepareCodexStep(current, target string, hadActiveState bool) (switchStep, error) {
	targetPath, err := manager.vault.CredentialsPath("codex", target)
	if err != nil {
		return switchStep{}, err
	}
	targetCredentials, err := (codex.FileStore{Path: targetPath}).Read()
	if err != nil {
		return switchStep{}, unusableSlotError("codex", target, err)
	}
	liveCredentials, err := manager.codexLive.Read()
	if err != nil {
		return switchStep{}, fmt.Errorf("read the current live Codex credentials before switching; run 'codex login', then retry: %w", err)
	}
	if current == target {
		targetCredentials = liveCredentials
	}
	currentPath, err := manager.vault.CredentialsPath("codex", current)
	if err != nil {
		return switchStep{}, err
	}
	currentCredentials, err := (codex.FileStore{Path: currentPath}).Read()
	if err != nil {
		return switchStep{}, fmt.Errorf("verify the recorded Codex account %q before copy-back; repair its slot or run 'hop login codex %s', then retry: %w", current, current, err)
	}
	if currentCredentials.AccountID != liveCredentials.AccountID {
		return switchStep{}, fmt.Errorf("live Codex belongs to a different identity than active account %q; run 'hop login codex <account>' to preserve the live login or restore account %q with 'hop codex %s', then retry", current, current, current)
	}
	copyBack := func() error { return nil }
	if current != "" {
		currentStore := codex.FileStore{Path: currentPath}
		copyBack = func() error { return currentStore.Write(liveCredentials) }
	}
	return switchStep{
		provider:       "codex",
		previous:       current,
		target:         target,
		hadActiveState: hadActiveState,
		copyBack:       copyBack,
		install:        func(context.Context) error { return manager.codexLive.Write(targetCredentials) },
		rollback:       func(context.Context) error { return manager.codexLive.Write(liveCredentials) },
	}, nil
}

func (manager switchManager) confirmActiveClaudeIdentity(ctx context.Context, current string, liveCredentials claude.Credentials) error {
	slotPath, err := manager.vault.SlotPath("claude", current)
	if err != nil {
		return err
	}
	contents, metadataErr := os.ReadFile(filepath.Join(slotPath, slotMetadataFilename))
	if metadataErr == nil {
		var metadata slotMetadata
		if err := json.Unmarshal(contents, &metadata); err != nil {
			return fmt.Errorf("verify the recorded Claude account %q before copy-back; repair %s or run 'hop login claude %s', then retry: %w", current, filepath.Join(slotPath, slotMetadataFilename), current, err)
		}
		if strings.TrimSpace(metadata.Email) != "" {
			liveEmail, err := manager.claudeEmail(ctx)
			if err != nil {
				return fmt.Errorf("verify the current live Claude identity before copy-back; run 'claude auth status --json' and retry: %w", err)
			}
			if strings.EqualFold(strings.TrimSpace(metadata.Email), strings.TrimSpace(liveEmail)) {
				return nil
			}
			return fmt.Errorf("live Claude is signed in as %s, but active account %q is recorded as %s; run 'hop login claude <account>' to preserve the live login or restore account %q with 'hop claude %s', then retry", liveEmail, current, metadata.Email, current, current)
		}
	}

	credentialsPath, err := manager.vault.CredentialsPath("claude", current)
	if err != nil {
		return err
	}
	recorded, err := (claude.FileStore{Path: credentialsPath}).Read()
	if err != nil {
		return fmt.Errorf("verify the recorded Claude account %q before copy-back; repair its slot or run 'hop login claude %s', then retry: %w", current, current, err)
	}
	if recorded.AccessToken == liveCredentials.AccessToken && recorded.RefreshToken == liveCredentials.RefreshToken {
		return nil
	}
	if metadataErr != nil && !errors.Is(metadataErr, os.ErrNotExist) {
		return fmt.Errorf("verify the recorded Claude account %q before copy-back; check %s permissions and retry: %w", current, filepath.Join(slotPath, slotMetadataFilename), metadataErr)
	}
	return fmt.Errorf("active Claude account %q has no recorded email and its live credentials no longer match; run 'hop login claude %s' to explicitly adopt the current login, then retry", current, current)
}

func (manager switchManager) findCodexSlotByAccountID(accountID string) (string, error) {
	providerPath := filepath.Join(manager.vault.Root(), "codex")
	entries, err := os.ReadDir(providerPath)
	if err != nil {
		return "", fmt.Errorf("no active Codex account is recorded and enrolled slots could not be inspected; run 'hop login codex <account>' to preserve the current login, then retry: %w", err)
	}
	matches := make([]string, 0, 1)
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		path, err := manager.vault.CredentialsPath("codex", entry.Name())
		if err != nil {
			continue
		}
		credentials, err := (codex.FileStore{Path: path}).Read()
		if err == nil && credentials.AccountID == accountID {
			matches = append(matches, entry.Name())
		}
	}
	if len(matches) == 1 {
		return matches[0], nil
	}
	if len(matches) > 1 {
		return "", fmt.Errorf("the live Codex login matches more than one enrolled slot; remove the duplicate slots with 'hop rm codex <account>', then retry")
	}
	return "", fmt.Errorf("no active Codex account is recorded and the live login does not match an enrolled slot; run 'hop login codex <account>' to preserve it, then retry")
}

func (manager switchManager) recoverInterruptedSwitch(ctx context.Context) (bool, error) {
	transaction, found, err := manager.readSwitchTransaction()
	if err != nil || !found {
		return false, err
	}
	activeState, err := manager.state.Load()
	if err != nil {
		return false, err
	}
	if transaction.Committed {
		if !transactionMatchesState(transaction, activeState) {
			return false, fmt.Errorf("finish the committed account switch recorded in %s; active-account state no longer matches its targets, restore the live credentials manually before removing the transaction file", manager.transactionPath())
		}
		if err := manager.removeSwitchTransaction(); err != nil {
			return false, fmt.Errorf("finish the previously committed account switch; remove %s and retry: %w", manager.transactionPath(), err)
		}
		return false, nil
	}

	if err := manager.confirmTransactionLiveTargets(transaction); err != nil {
		return false, err
	}

	for _, step := range transaction.Steps {
		credentialsPath, err := manager.vault.CredentialsPath(step.Provider, step.Previous)
		if err != nil {
			return false, fmt.Errorf("recover the interrupted %s switch; repair the previous account slot %q before retrying: %w", step.Provider, step.Previous, err)
		}
		switch step.Provider {
		case "claude":
			credentials, err := (claude.FileStore{Path: credentialsPath}).Read()
			if err == nil {
				err = manager.claudeLive.Write(ctx, credentials)
			}
			if err != nil {
				return false, fmt.Errorf("recover the interrupted Claude switch by restoring account %q; stop using Claude, repair its slot, and retry hop: %w", step.Previous, err)
			}
		case "codex":
			credentials, err := (codex.FileStore{Path: credentialsPath}).Read()
			if err == nil {
				err = manager.codexLive.Write(credentials)
			}
			if err != nil {
				return false, fmt.Errorf("recover the interrupted Codex switch by restoring account %q; stop using Codex, repair its slot, and retry hop: %w", step.Previous, err)
			}
		default:
			return false, fmt.Errorf("read interrupted switch transaction %s; provider %q is invalid, restore live credentials manually before removing the transaction file", manager.transactionPath(), step.Provider)
		}
	}

	restoredState := previousState(activeState, transaction)
	if err := manager.state.Save(restoredState); err != nil {
		return false, fmt.Errorf("recover the interrupted account switch after restoring live credentials; retry hop to restore active-account state: %w", err)
	}
	if err := manager.removeSwitchTransaction(); err != nil {
		return false, fmt.Errorf("finish recovery of the interrupted account switch; remove %s and retry: %w", manager.transactionPath(), err)
	}
	return true, nil
}

// confirmTransactionLiveTargets refuses to restore a transaction that was
// recorded against different live-credential targets than the current run —
// an interrupted sandbox switch must never be "recovered" into the real
// Keychain or ~/.codex/auth.json.
func (manager switchManager) confirmTransactionLiveTargets(transaction switchTransaction) error {
	for _, step := range transaction.Steps {
		recorded, current := transaction.CodexLive, manager.codexTarget
		if step.Provider == "claude" {
			recorded, current = transaction.ClaudeLive, manager.claudeTarget
		}
		if recorded != current {
			return fmt.Errorf("the interrupted %s switch in %s was recorded against live target %q, but hop is now targeting %q; rerun hop with the same HOP_CLAUDE_CREDENTIALS_FILE/HOP_CODEX_AUTH_FILE settings to recover it, or restore the live credentials manually before removing the transaction file", step.Provider, manager.transactionPath(), recorded, current)
		}
	}
	return nil
}

func transactionFor(steps []switchStep) switchTransaction {
	transaction := switchTransaction{Steps: make([]switchTransactionStep, 0, len(steps))}
	for _, step := range steps {
		transaction.Steps = append(transaction.Steps, switchTransactionStep{
			Provider:       step.provider,
			Previous:       step.previous,
			Target:         step.target,
			HadActiveState: step.hadActiveState,
		})
	}
	return transaction
}

func previousState(current state.State, transaction switchTransaction) state.State {
	restored := state.New()
	for providerName, accountName := range current.ActiveAccounts {
		restored.SetActive(providerName, accountName)
	}
	for _, step := range transaction.Steps {
		if step.HadActiveState {
			restored.SetActive(step.Provider, step.Previous)
		} else {
			delete(restored.ActiveAccounts, step.Provider)
		}
	}
	return restored
}

func transactionMatchesState(transaction switchTransaction, activeState state.State) bool {
	for _, step := range transaction.Steps {
		active, found := activeState.Active(step.Provider)
		if !found || active != step.Target {
			return false
		}
	}
	return true
}

func (manager switchManager) readSwitchTransaction() (switchTransaction, bool, error) {
	contents, err := os.ReadFile(manager.transactionPath())
	if errors.Is(err, os.ErrNotExist) {
		return switchTransaction{}, false, nil
	}
	if err != nil {
		return switchTransaction{}, false, fmt.Errorf("read interrupted switch transaction %s; check its permissions and retry: %w", manager.transactionPath(), err)
	}
	var transaction switchTransaction
	if err := json.Unmarshal(contents, &transaction); err != nil {
		return switchTransaction{}, true, fmt.Errorf("read interrupted switch transaction %s; restore live credentials manually before removing the invalid file: %w", manager.transactionPath(), err)
	}
	if err := validateSwitchTransaction(transaction); err != nil {
		return switchTransaction{}, true, fmt.Errorf("read interrupted switch transaction %s; restore live credentials manually before removing the invalid file: %w", manager.transactionPath(), err)
	}
	return transaction, true, nil
}

func validateSwitchTransaction(transaction switchTransaction) error {
	if len(transaction.Steps) == 0 || len(transaction.Steps) > 2 {
		return errors.New("expected one or two provider steps")
	}
	providers := make(map[string]bool)
	for _, step := range transaction.Steps {
		if step.Provider != "claude" && step.Provider != "codex" {
			return fmt.Errorf("provider %q is not supported", step.Provider)
		}
		if providers[step.Provider] {
			return fmt.Errorf("provider %q appears more than once", step.Provider)
		}
		if step.Previous == "" || step.Target == "" {
			return fmt.Errorf("provider %q omits its previous or target account", step.Provider)
		}
		providers[step.Provider] = true
	}
	return nil
}

func (manager switchManager) writeSwitchTransaction(transaction switchTransaction) error {
	if transaction.ClaudeLive == "" {
		transaction.ClaudeLive = manager.claudeTarget
	}
	if transaction.CodexLive == "" {
		transaction.CodexLive = manager.codexTarget
	}
	if err := validateSwitchTransaction(transaction); err != nil {
		return err
	}
	contents, err := json.MarshalIndent(transaction, "", "  ")
	if err != nil {
		return fmt.Errorf("encode switch transaction: %w", err)
	}
	contents = append(contents, '\n')
	if err := os.MkdirAll(manager.vault.Root(), 0o700); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(manager.vault.Root(), ".switch-transaction-*.json")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer func() { _ = os.Remove(temporaryPath) }()
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := temporary.Write(contents); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, manager.transactionPath()); err != nil {
		return err
	}
	return nil
}

func (manager switchManager) removeSwitchTransaction() error {
	if err := os.Remove(manager.transactionPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func (manager switchManager) transactionPath() string {
	return filepath.Join(manager.vault.Root(), switchTransactionFilename)
}

func unusableSlotError(providerName, accountName string, err error) error {
	return fmt.Errorf("%s account %q has unusable credentials; run 'hop rm %s %s', then 'hop login %s %s' before retrying: %w", providerName, accountName, providerName, accountName, providerName, accountName, err)
}

func switchFailure(subject, accountName string, switchErr, rollbackErr error) error {
	if rollbackErr != nil {
		return fmt.Errorf("switch %s to account %q: %v; restoring the previous live credentials also failed: %w; stop using that provider and repair its live login before retrying", subject, accountName, switchErr, rollbackErr)
	}
	return fmt.Errorf("switch %s to account %q; the previous live credentials were restored, fix the reported problem and retry: %w", subject, accountName, switchErr)
}

func rollbackSteps(ctx context.Context, steps []switchStep) error {
	var rollbackErr error
	for index := len(steps) - 1; index >= 0; index-- {
		if err := steps[index].rollback(ctx); err != nil {
			rollbackErr = errors.Join(rollbackErr, fmt.Errorf("restore %s: %w", steps[index].provider, err))
		}
	}
	return rollbackErr
}

func uniqueAccounts(accounts ...string) []string {
	unique := make([]string, 0, len(accounts))
	seen := make(map[string]bool)
	for _, accountName := range accounts {
		if accountName == "" || seen[accountName] {
			continue
		}
		seen[accountName] = true
		unique = append(unique, accountName)
	}
	return unique
}

func releaseAll(releases []func()) {
	for index := len(releases) - 1; index >= 0; index-- {
		releases[index]()
	}
}

func (store fileActiveStateStore) Load() (state.State, error) {
	return state.Load(store.root)
}

func (store fileActiveStateStore) Save(activeState state.State) error {
	return activeState.Save(store.root)
}

type claudeFileLiveStore struct{ store claude.LiveFile }

func (store claudeFileLiveStore) Read(context.Context) (claude.Credentials, error) {
	return store.store.Read()
}

func (store claudeFileLiveStore) Write(_ context.Context, credentials claude.Credentials) error {
	return store.store.Write(credentials)
}
