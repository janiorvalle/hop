package cli

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/janiorvalle/hop/internal/provider"
	"github.com/janiorvalle/hop/internal/provider/codex"
	"github.com/janiorvalle/hop/internal/render"
	"github.com/janiorvalle/hop/internal/state"
	"github.com/janiorvalle/hop/internal/vault"
)

const (
	codexResetApproval   = "HOP_CODEX_RESET"
	pendingResetFilename = ".pending-reset.json"
	resetRequestTimeout  = 20 * time.Second
)

// pendingReset is the redeem id hop saved before asking OpenAI to spend a
// credit. It lives in the slot until OpenAI confirms the request, so a retry
// sends the same id instead of spending a second credit.
type pendingReset struct {
	RedeemRequestID string `json:"redeem_request_id"`
	AccountID       string `json:"account_id"`
}

type codexResetter struct {
	vault      vault.Vault
	adapter    codex.Adapter
	live       codexLiveStore
	stdout     io.Writer
	stderr     io.Writer
	getenv     func(string) string
	stdinIsTTY func(io.Reader) bool
	now        func() time.Time
}

func resetCodexAccount(ctx context.Context, accountName string, stdin io.Reader, stdout, stderr io.Writer) error {
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
		_, _ = fmt.Fprintln(stdout, "Recovered an interrupted account switch before continuing.")
	}
	resetter := codexResetter{
		vault:      manager.vault,
		adapter:    codex.New(codex.Config{}),
		live:       manager.codexLive,
		stdout:     stdout,
		stderr:     stderr,
		getenv:     os.Getenv,
		stdinIsTTY: readerIsTerminal,
		now:        time.Now,
	}
	return resetter.resetLocked(ctx, accountName, stdin)
}

func (resetter codexResetter) Reset(ctx context.Context, accountName string, stdin io.Reader) error {
	manager := switchManager{vault: resetter.vault}
	releaseProviders, err := manager.lockProviders(ctx)
	if err != nil {
		return err
	}
	defer releaseProviders()
	releaseState, err := acquireStateLock(ctx, resetter.vault.Root())
	if err != nil {
		return err
	}
	defer releaseState()
	return resetter.resetLocked(ctx, accountName, stdin)
}

func (resetter codexResetter) resetLocked(ctx context.Context, accountName string, stdin io.Reader) error {
	credentialsPath, err := resetter.vault.CredentialsPath("codex", accountName)
	if err != nil {
		return err
	}
	slotPath := filepath.Dir(credentialsPath)
	if _, err := os.Stat(slotPath); errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("codex account %q does not exist; run 'hop ls' to see enrolled accounts", accountName)
	} else if err != nil {
		return fmt.Errorf("inspect codex account %q before resetting; check its permissions and retry: %w", accountName, err)
	}
	if _, err := os.Stat(filepath.Join(slotPath, slotReservationFilename)); err == nil {
		_, active, inspectErr := inspectSlotReservation(slotPath)
		if inspectErr != nil {
			return fmt.Errorf("inspect the enrollment owner for codex account %q; fix or remove %s and retry: %w", accountName, filepath.Join(slotPath, slotReservationFilename), inspectErr)
		}
		if active {
			return fmt.Errorf("codex account %q is being enrolled; wait for login to finish, then retry the reset", accountName)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect the enrollment state for codex account %q; check its permissions and retry: %w", accountName, err)
	}
	releaseRefresh, err := acquireRefreshLock(ctx, slotPath)
	if err != nil {
		return fmt.Errorf("wait to reset codex account %q until its token refresh finishes: %w", accountName, err)
	}
	defer releaseRefresh()

	activeState, err := state.Load(resetter.vault.Root())
	if err != nil {
		return err
	}
	activeAccount, isActive := activeState.Active("codex")
	isActive = isActive && activeAccount == accountName
	var credentials codex.Credentials
	if isActive {
		credentials, err = resetter.liveCredentials(accountName, codex.FileStore{Path: credentialsPath})
	} else {
		credentials, err = resetter.slotCredentials(ctx, accountName, codex.FileStore{Path: credentialsPath})
	}
	if err != nil {
		return fmt.Errorf("read the credentials for codex account %q before resetting; no credit was spent: %w", accountName, err)
	}
	pendingPath := filepath.Join(slotPath, pendingResetFilename)
	pending, retrying, err := readPendingReset(pendingPath)
	if err != nil {
		return err
	}
	if retrying && pending.AccountID != credentials.AccountID {
		return fmt.Errorf("[CODEX_RESET_STALE_REQUEST] The reset request recorded at %s belongs to a different Codex identity than codex account %q now holds, so nothing was sent to OpenAI. Delete that file, then rerun 'hop reset codex %s'", pendingPath, accountName, accountName)
	}
	if retrying {
		err = resetter.confirm(ctx, stdin, fmt.Sprintf("Retrying the reset request an earlier run recorded for codex account %q. It spends at most 1 manual reset.", accountName), accountName)
	} else {
		pending, err = resetter.startReset(ctx, stdin, accountName, credentials, pendingPath)
	}
	if err != nil {
		return err
	}

	requestCtx, cancel := context.WithTimeout(ctx, resetRequestTimeout)
	defer cancel()
	if err := resetter.adapter.ConsumeResetCredit(requestCtx, credentials, pending.RedeemRequestID); err != nil {
		return fmt.Errorf("[CODEX_RESET_FAILED] The reset for codex account %q did not go through: %s. Rerun 'hop reset codex %s' to retry with the same request id, which cannot spend a second credit. If 'hop ls' already shows the reset, delete %s instead", accountName, strings.ReplaceAll(err.Error(), "<account>", accountName), accountName, pendingPath)
	}
	if err := os.Remove(pendingPath); err != nil {
		return fmt.Errorf("[CODEX_RESET_RECORD_STUCK] The reset for codex account %q went through, but hop could not clear its request record: %w. Delete %s before the next reset", accountName, err, pendingPath)
	}
	resetter.reportSpentCredit(ctx, accountName, credentials, isActive)
	return nil
}

// startReset checks the credits, asks, and records the request id before
// anything is sent, so a retry can find it.
func (resetter codexResetter) startReset(ctx context.Context, stdin io.Reader, accountName string, credentials codex.Credentials, pendingPath string) (pendingReset, error) {
	usage, err := resetter.fetchUsage(ctx, credentials)
	if err != nil {
		return pendingReset{}, fmt.Errorf("[CODEX_RESET_USAGE_UNAVAILABLE] Could not read the usage for codex account %q, so no credit was spent: %s", accountName, strings.ReplaceAll(err.Error(), "<account>", accountName))
	}
	if usage.ResetCredits == nil {
		return pendingReset{}, fmt.Errorf("[CODEX_RESET_CREDITS_UNKNOWN] OpenAI did not report how many manual resets codex account %q has, so no credit was spent. Retry 'hop reset codex %s'", accountName, accountName)
	}
	if usage.ResetCredits.Count == 0 {
		return pendingReset{}, noCreditsRefusal(accountName, usage, resetter.now())
	}
	if err := resetter.confirm(ctx, stdin, fmt.Sprintf("This spends 1 of %d manual resets on codex account %q.", usage.ResetCredits.Count, accountName), accountName); err != nil {
		return pendingReset{}, err
	}
	pending, err := newPendingReset(credentials.AccountID)
	if err != nil {
		return pendingReset{}, fmt.Errorf("generate a request id for the reset of codex account %q; no credit was spent: %w", accountName, err)
	}
	if err := writePendingReset(pendingPath, pending); err != nil {
		return pendingReset{}, fmt.Errorf("record the reset request for codex account %q before sending it; no credit was spent, check the slot permissions and retry: %w", accountName, err)
	}
	return pending, nil
}

// reportSpentCredit never fails the command: the credit is already spent, and a
// nonzero exit here would invite an automated retry that mints a new request
// id and spends a second one.
func (resetter codexResetter) reportSpentCredit(ctx context.Context, accountName string, credentials codex.Credentials, isActive bool) {
	refreshed, err := resetter.fetchUsage(ctx, credentials)
	if err != nil {
		_, _ = fmt.Fprintf(resetter.stdout, "Spent 1 manual reset on codex account %q.\n", accountName)
		_, _ = fmt.Fprintf(resetter.stderr, "hop: the refreshed usage for codex account %q could not be read: %s. Run 'hop ls' to see it.\n", accountName, strings.ReplaceAll(err.Error(), "<account>", accountName))
		return
	}
	_, _ = fmt.Fprintf(resetter.stdout, "Spent 1 manual reset on codex account %q%s.\n", accountName, creditsLeft(refreshed.ResetCredits))
	row := newAccountResult(account{Provider: provider.Codex, Name: accountName, Active: isActive})
	row.recordUsage(refreshed)
	_ = writeTable(resetter.stdout, glanceDocument{Schema: listSchema, Accounts: []accountResult{row}}, terminalOptions(resetter.stdout, resetter.now()))
}

// creditsLeft says what a spent reset left behind, since the glance no
// longer prints the count on the row.
func creditsLeft(credits *provider.ResetCredits) string {
	switch {
	case credits == nil:
		return ""
	case credits.Count == 1:
		return ", 1 reset left"
	default:
		return fmt.Sprintf(", %d resets left", credits.Count)
	}
}

func (resetter codexResetter) liveCredentials(accountName string, slot codex.FileStore) (codex.Credentials, error) {
	live, err := resetter.live.Read()
	if err != nil {
		return codex.Credentials{}, err
	}
	recorded, err := slot.Read()
	if err != nil {
		return codex.Credentials{}, err
	}
	if recorded.AccountID != live.AccountID {
		return codex.Credentials{}, fmt.Errorf("[CODEX_RESET_WRONG_LOGIN] the live Codex login belongs to a different identity than active account %q; run 'hop login codex <account>' to keep that login, or 'hop codex %s' to restore the account, then retry", accountName, accountName)
	}
	return live, nil
}

func (resetter codexResetter) slotCredentials(ctx context.Context, accountName string, store codex.FileStore) (codex.Credentials, error) {
	credentials, err := store.Read()
	if err != nil || !credentials.NeedsRefresh(resetter.now(), refreshSkew) {
		return credentials, err
	}
	metadata, err := loadSlotMetadata(filepath.Dir(store.Path))
	if err != nil {
		return codex.Credentials{}, err
	}
	if metadata.RefreshPolicy != managedRefreshPolicy {
		return codex.Credentials{}, fmt.Errorf("its access token has expired and hop does not manage this slot's refresh token; run 'hop login codex %s' and retry", accountName)
	}
	return refreshCodexFileSlot(ctx, resetter.adapter, store)
}

func (resetter codexResetter) fetchUsage(ctx context.Context, credentials codex.Credentials) (provider.Usage, error) {
	usageCtx, cancel := context.WithTimeout(ctx, usageTimeout)
	defer cancel()
	return resetter.adapter.FetchUsage(usageCtx, credentials)
}

func (resetter codexResetter) confirm(ctx context.Context, stdin io.Reader, question, accountName string) error {
	if resetter.getenv(codexResetApproval) == "approved" {
		return nil
	}
	if !resetter.stdinIsTTY(stdin) {
		return fmt.Errorf("[CODEX_RESET_NEEDS_CONFIRMATION] Spending a manual reset needs a yes from a terminal, so nothing was sent to OpenAI. Rerun 'hop reset codex %s' in a terminal, or approve it from automation with %s=approved", accountName, codexResetApproval)
	}
	if _, err := fmt.Fprint(resetter.stderr, question+" Continue? [y/N] "); err != nil {
		return fmt.Errorf("show the reset confirmation prompt; check the terminal and retry: %w", err)
	}
	approved, err := awaitConfirmation(ctx, stdin)
	if err != nil && errors.Is(err, ctx.Err()) {
		return fmt.Errorf("[CODEX_RESET_CANCELED] codex account %q was not reset; nothing was sent to OpenAI: %w", accountName, err)
	}
	if err == nil && approved {
		return nil
	}
	return fmt.Errorf("[CODEX_RESET_DECLINED] codex account %q was not reset; nothing was sent to OpenAI", accountName)
}

func noCreditsRefusal(accountName string, usage provider.Usage, now time.Time) error {
	message := fmt.Sprintf("[CODEX_RESET_NO_CREDITS] codex account %q has 0 manual resets to spend", accountName)
	for _, window := range usage.Windows {
		if window.Kind == provider.Weekly {
			message += fmt.Sprintf("; its weekly limit resets on its own in %s", render.Countdown(now, window.ResetsAt))
			break
		}
	}
	return fmt.Errorf("%s. Run 'hop ls' to pick an account with room", message)
}

func newPendingReset(accountID string) (pendingReset, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return pendingReset{}, err
	}
	value[6] = value[6]&0x0f | 0x40
	value[8] = value[8]&0x3f | 0x80
	id := fmt.Sprintf("%x-%x-%x-%x-%x", value[0:4], value[4:6], value[6:8], value[8:10], value[10:16])
	return pendingReset{RedeemRequestID: id, AccountID: accountID}, nil
}

func readPendingReset(path string) (pendingReset, bool, error) {
	contents, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return pendingReset{}, false, nil
	}
	if err != nil {
		return pendingReset{}, false, fmt.Errorf("read the recorded reset request at %s; fix its permissions and retry: %w", path, err)
	}
	var pending pendingReset
	if err := json.Unmarshal(contents, &pending); err != nil || pending.RedeemRequestID == "" || pending.AccountID == "" {
		return pendingReset{}, false, fmt.Errorf("read the recorded reset request at %s; expected {\"redeem_request_id\":\"<uuid>\",\"account_id\":\"<chatgpt account id>\"}, delete the file if 'hop ls' shows the reset already landed, then retry", path)
	}
	return pending, true, nil
}

func writePendingReset(path string, pending pendingReset) error {
	contents, err := json.Marshal(pending)
	if err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".pending-reset-*.json")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer func() { _ = os.Remove(temporaryPath) }()
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := temporary.Write(append(contents, '\n')); err != nil {
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
	return os.Rename(temporaryPath, path)
}
