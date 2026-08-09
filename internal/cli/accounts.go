package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/janiorvalle/hop/internal/provider"
	"github.com/janiorvalle/hop/internal/provider/claude"
	"github.com/janiorvalle/hop/internal/provider/codex"
	"github.com/janiorvalle/hop/internal/state"
	"github.com/janiorvalle/hop/internal/vault"
)

const (
	slotMetadataFilename      = "slot.json"
	managedRefreshPolicy      = "managed"
	refreshSkew               = 5 * time.Minute
	refreshLockWait           = 20 * time.Second
	refreshLockStale          = 2 * time.Minute
	refreshLockPoll           = 25 * time.Millisecond
	refreshTransactionTimeout = 20 * time.Second
	stateLockFilename         = ".state.lock"
)

type slotMetadata struct {
	RefreshPolicy string `json:"refresh_policy"`
	Email         string `json:"email,omitempty"`
}

type account struct {
	Provider provider.Name
	Name     string
	Active   bool
	Fetcher  provider.Fetcher
}

type catalog interface {
	Accounts() ([]account, error)
}

type vaultCatalog struct {
	vault         vault.Vault
	state         state.State
	claudeAdapter claude.Adapter
	codexAdapter  codex.Adapter
	claudeLive    claudeLiveStore
	codexLive     codexLiveStore
	now           func() time.Time
}

type claudeSlotFetcher struct {
	adapter        claude.Adapter
	store          claude.Store
	refreshAllowed bool
	now            func() time.Time
}

type codexSlotFetcher struct {
	adapter        codex.Adapter
	store          codex.Store
	refreshAllowed bool
	now            func() time.Time
}

type claudeLiveFetcher struct {
	adapter claude.Adapter
	store   claudeLiveStore
}
type codexLiveFetcher struct {
	adapter codex.Adapter
	store   codexLiveStore
}
type failingFetcher struct{ err error }

func defaultCatalog() (catalog, error) {
	accountVault, err := defaultVault()
	if err != nil {
		return nil, err
	}
	activeState, err := state.Load(accountVault.Root())
	if err != nil {
		return nil, err
	}
	claudeLive, _ := defaultClaudeSwitchStore()
	codexLive, _, err := defaultCodexSwitchStore()
	if err != nil {
		return nil, err
	}
	return vaultCatalog{
		vault:         accountVault,
		state:         activeState,
		claudeAdapter: claude.New(claude.Config{}),
		codexAdapter:  codex.New(codex.Config{}),
		now:           time.Now,
		claudeLive:    claudeLive,
		codexLive:     codexLive,
	}, nil
}

func defaultVault() (vault.Vault, error) {
	if root := os.Getenv("HOP_HOME"); root != "" {
		return vault.New(root)
	}
	return vault.Default()
}

func (catalog vaultCatalog) Accounts() ([]account, error) {
	accounts := make([]account, 0)
	for _, providerName := range []provider.Name{provider.Claude, provider.Codex} {
		providerDirectory := filepath.Join(catalog.vault.Root(), string(providerName))
		entries, err := os.ReadDir(providerDirectory)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("list %s accounts in %s; check the directory permissions and retry: %w", providerName, providerDirectory, err)
		}
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			accounts = append(accounts, catalog.account(providerName, entry.Name()))
		}
	}
	sort.Slice(accounts, func(left, right int) bool {
		if accounts[left].Provider != accounts[right].Provider {
			return accounts[left].Provider < accounts[right].Provider
		}
		return accounts[left].Name < accounts[right].Name
	})
	return accounts, nil
}

func (catalog vaultCatalog) account(providerName provider.Name, name string) account {
	activeName, isActive := catalog.state.Active(string(providerName))
	isActive = isActive && activeName == name
	if isActive {
		// Live credentials belong to the provider CLI. A glance may read them,
		// but only that CLI may rotate and persist its live refresh token.
		return account{Provider: providerName, Name: name, Active: true, Fetcher: catalog.liveFetcher(providerName)}
	}

	credentialsPath, err := catalog.vault.CredentialsPath(string(providerName), name)
	if err != nil {
		return account{Provider: providerName, Name: name, Fetcher: failingFetcher{err: err}}
	}
	refreshAllowed, err := slotAllowsRefresh(filepath.Dir(credentialsPath))
	if err != nil {
		return account{Provider: providerName, Name: name, Fetcher: failingFetcher{err: err}}
	}
	return account{
		Provider: providerName,
		Name:     name,
		Fetcher:  catalog.slotFetcher(providerName, credentialsPath, refreshAllowed),
	}
}

func (catalog vaultCatalog) liveFetcher(providerName provider.Name) provider.Fetcher {
	if providerName == provider.Claude {
		return claudeLiveFetcher{adapter: catalog.claudeAdapter, store: catalog.claudeLive}
	}
	return codexLiveFetcher{adapter: catalog.codexAdapter, store: catalog.codexLive}
}

func (catalog vaultCatalog) slotFetcher(providerName provider.Name, credentialsPath string, refreshAllowed bool) provider.Fetcher {
	if providerName == provider.Claude {
		return claudeSlotFetcher{
			adapter:        catalog.claudeAdapter,
			store:          claude.FileStore{Path: credentialsPath},
			refreshAllowed: refreshAllowed,
			now:            catalog.now,
		}
	}
	return codexSlotFetcher{
		adapter:        catalog.codexAdapter,
		store:          codex.FileStore{Path: credentialsPath},
		refreshAllowed: refreshAllowed,
		now:            catalog.now,
	}
}

func slotAllowsRefresh(slotPath string) (bool, error) {
	metadataPath := filepath.Join(slotPath, slotMetadataFilename)
	contents, err := os.ReadFile(metadataPath)
	if errors.Is(err, os.ErrNotExist) {
		// Manually seeded slots are read-only. The login flow opts a slot into
		// rotation only after hop has taken custody of its refresh token.
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read slot metadata from %s; fix its permissions or run 'hop login' again: %w", metadataPath, err)
	}
	var metadata slotMetadata
	if err := json.Unmarshal(contents, &metadata); err != nil {
		return false, fmt.Errorf("read slot metadata from %s; expected {\"refresh_policy\":\"managed\"}, fix the file or run 'hop login' again: %w", metadataPath, err)
	}
	if metadata.RefreshPolicy != managedRefreshPolicy {
		return false, nil
	}
	return true, nil
}

func writeManagedSlotMetadata(slotPath, email string) error {
	if err := os.MkdirAll(slotPath, 0o700); err != nil {
		return fmt.Errorf("create account slot %s: %w", slotPath, err)
	}
	if err := os.Chmod(slotPath, 0o700); err != nil {
		return fmt.Errorf("secure account slot %s: %w", slotPath, err)
	}
	contents, err := json.MarshalIndent(slotMetadata{RefreshPolicy: managedRefreshPolicy, Email: email}, "", "  ")
	if err != nil {
		return fmt.Errorf("encode slot metadata: %w", err)
	}
	contents = append(contents, '\n')
	temporary, err := os.CreateTemp(slotPath, ".slot-*.json")
	if err != nil {
		return fmt.Errorf("create temporary slot metadata in %s: %w", slotPath, err)
	}
	temporaryPath := temporary.Name()
	defer func() { _ = os.Remove(temporaryPath) }()
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("secure temporary slot metadata %s: %w", temporaryPath, err)
	}
	if _, err := temporary.Write(contents); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("write temporary slot metadata %s: %w", temporaryPath, err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("sync temporary slot metadata %s: %w", temporaryPath, err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close temporary slot metadata %s: %w", temporaryPath, err)
	}
	if err := os.Rename(temporaryPath, filepath.Join(slotPath, slotMetadataFilename)); err != nil {
		return fmt.Errorf("install slot metadata in %s: %w", slotPath, err)
	}
	return nil
}

func (fetcher claudeSlotFetcher) FetchUsage(ctx context.Context) (provider.Usage, error) {
	credentials, err := fetcher.store.Read()
	if err != nil {
		return provider.Usage{}, err
	}
	return fetcher.adapter.FetchUsage(ctx, credentials)
}

func (fetcher codexSlotFetcher) FetchUsage(ctx context.Context) (provider.Usage, error) {
	credentials, err := fetcher.store.Read()
	if err != nil {
		return provider.Usage{}, err
	}
	return fetcher.adapter.FetchUsage(ctx, credentials)
}

func (fetcher claudeSlotFetcher) Prepare(ctx context.Context) error {
	if !fetcher.refreshAllowed {
		return nil
	}
	credentials, err := fetcher.store.Read()
	if err != nil || !credentials.NeedsRefresh(fetcher.now(), refreshSkew) {
		return err
	}
	_, err = refreshClaudeSlot(ctx, fetcher.adapter, fetcher.store, fetcher.now())
	return err
}

func (fetcher codexSlotFetcher) Prepare(ctx context.Context) error {
	if !fetcher.refreshAllowed {
		return nil
	}
	credentials, err := fetcher.store.Read()
	if err != nil || !credentials.NeedsRefresh(fetcher.now(), refreshSkew) {
		return err
	}
	_, err = refreshCodexSlot(ctx, fetcher.adapter, fetcher.store, fetcher.now())
	return err
}

func refreshClaudeSlot(ctx context.Context, adapter claude.Adapter, store claude.Store, now time.Time) (claude.Credentials, error) {
	if fileStore, ok := store.(claude.FileStore); ok {
		release, err := acquireRefreshLock(ctx, filepath.Dir(fileStore.Path))
		if err != nil {
			return claude.Credentials{}, err
		}
		defer release()
		credentials, err := fileStore.Read()
		if err != nil || !credentials.NeedsRefresh(now, refreshSkew) {
			return credentials, err
		}
		if err := ctx.Err(); err != nil {
			return claude.Credentials{}, fmt.Errorf("start Claude token refresh before the glance deadline; retry the command: %w", err)
		}
		return refreshClaudeFileSlot(ctx, adapter, fileStore)
	}
	credentials, err := adapter.Refresh(ctx, store)
	if err == nil || credentials.AccessToken == "" {
		return credentials, err
	}
	if recoveryErr := store.Write(credentials); recoveryErr != nil {
		return claude.Credentials{}, fmt.Errorf("save rotated Claude tokens after two write attempts; run 'hop login claude <account>' before retrying because the slot could not retain the recovery copy: %v: %w", recoveryErr, err)
	}
	return credentials, nil
}

func refreshCodexSlot(ctx context.Context, adapter codex.Adapter, store codex.Store, now time.Time) (codex.Credentials, error) {
	if fileStore, ok := store.(codex.FileStore); ok {
		release, err := acquireRefreshLock(ctx, filepath.Dir(fileStore.Path))
		if err != nil {
			return codex.Credentials{}, err
		}
		defer release()
		credentials, err := fileStore.Read()
		if err != nil || !credentials.NeedsRefresh(now, refreshSkew) {
			return credentials, err
		}
		if err := ctx.Err(); err != nil {
			return codex.Credentials{}, fmt.Errorf("start Codex token refresh before the glance deadline; retry the command: %w", err)
		}
		return refreshCodexFileSlot(ctx, adapter, fileStore)
	}
	credentials, err := adapter.Refresh(ctx, store)
	if err == nil || credentials.AccessToken == "" {
		return credentials, err
	}
	if recoveryErr := store.Write(credentials); recoveryErr != nil {
		return codex.Credentials{}, fmt.Errorf("save rotated Codex tokens after two write attempts; run 'hop login codex <account>' before retrying because the slot could not retain the recovery copy: %v: %w", recoveryErr, err)
	}
	return credentials, nil
}

func refreshClaudeFileSlot(ctx context.Context, adapter claude.Adapter, store claude.FileStore) (claude.Credentials, error) {
	journal, err := store.ReserveRecovery()
	if err != nil {
		return claude.Credentials{}, err
	}
	// After submission, rotation and persistence must outlive the read-only
	// glance deadline, but the transaction still has its own hard bound.
	transactionCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), refreshTransactionTimeout)
	defer cancel()
	journaledStore := &claudeJournaledStore{primary: store, journal: journal}
	credentials, refreshErr := adapter.Refresh(transactionCtx, journaledStore)
	if refreshErr == nil {
		if err := journal.Discard(); err != nil {
			return claude.Credentials{}, err
		}
		return credentials, nil
	}
	if credentials.AccessToken == "" {
		_ = journal.Discard()
		return claude.Credentials{}, refreshErr
	}
	if !journaledStore.saved {
		return claude.Credentials{}, fmt.Errorf("preserve rotated Claude tokens before replacing the slot; run 'hop login claude <account>' before retrying because the private recovery journal failed: %w", refreshErr)
	}
	// The journaled store synced the recovery copy before attempting primary.
	return credentials, nil
}

func refreshCodexFileSlot(ctx context.Context, adapter codex.Adapter, store codex.FileStore) (codex.Credentials, error) {
	journal, err := store.ReserveRecovery()
	if err != nil {
		return codex.Credentials{}, err
	}
	// After submission, rotation and persistence must outlive the read-only
	// glance deadline, but the transaction still has its own hard bound.
	transactionCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), refreshTransactionTimeout)
	defer cancel()
	journaledStore := &codexJournaledStore{primary: store, journal: journal}
	credentials, refreshErr := adapter.Refresh(transactionCtx, journaledStore)
	if refreshErr == nil {
		if err := journal.Discard(); err != nil {
			return codex.Credentials{}, err
		}
		return credentials, nil
	}
	if credentials.AccessToken == "" {
		_ = journal.Discard()
		return codex.Credentials{}, refreshErr
	}
	if !journaledStore.saved {
		return codex.Credentials{}, fmt.Errorf("preserve rotated Codex tokens before replacing the slot; run 'hop login codex <account>' before retrying because the private recovery journal failed: %w", refreshErr)
	}
	// The journaled store synced the recovery copy before attempting primary.
	return credentials, nil
}

type claudeJournaledStore struct {
	primary claude.FileStore
	journal *claude.RecoveryJournal
	saved   bool
}

func (store *claudeJournaledStore) Read() (claude.Credentials, error) {
	return store.primary.Read()
}

func (store *claudeJournaledStore) Write(credentials claude.Credentials) error {
	if err := store.journal.Save(credentials); err != nil {
		return fmt.Errorf("save rotated Claude tokens to the recovery journal before replacing the slot: %w", err)
	}
	store.saved = true
	return store.primary.Write(credentials)
}

type codexJournaledStore struct {
	primary codex.FileStore
	journal *codex.RecoveryJournal
	saved   bool
}

func (store *codexJournaledStore) Read() (codex.Credentials, error) {
	return store.primary.Read()
}

func (store *codexJournaledStore) Write(credentials codex.Credentials) error {
	if err := store.journal.Save(credentials); err != nil {
		return fmt.Errorf("save rotated Codex tokens to the recovery journal before replacing the slot: %w", err)
	}
	store.saved = true
	return store.primary.Write(credentials)
}

func acquireRefreshLock(ctx context.Context, slotPath string) (func(), error) {
	lockPath := filepath.Join(slotPath, ".refresh.lock")
	deadline := time.NewTimer(refreshLockWait)
	defer deadline.Stop()
	ticker := time.NewTicker(refreshLockPoll)
	defer ticker.Stop()
	for {
		err := os.Mkdir(lockPath, 0o700)
		if err == nil {
			return func() { _ = os.Remove(lockPath) }, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("lock account slot %s before refreshing; check its permissions and retry: %w", slotPath, err)
		}
		if info, statErr := os.Stat(lockPath); statErr == nil && time.Since(info.ModTime()) > refreshLockStale {
			_ = os.Remove(lockPath)
			continue
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("wait for another hop process to finish refreshing %s; retry the command: %w", slotPath, ctx.Err())
		case <-deadline.C:
			return nil, fmt.Errorf("wait for another hop process to finish refreshing %s; it did not finish within %s, retry the command: refresh lock timeout", slotPath, refreshLockWait)
		case <-ticker.C:
		}
	}
}

func acquireStateLock(ctx context.Context, root string) (func(), error) {
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("create hop data directory %s before updating active-account state: %w", root, err)
	}
	lockPath := filepath.Join(root, stateLockFilename)
	deadline := time.NewTimer(refreshLockWait)
	defer deadline.Stop()
	ticker := time.NewTicker(refreshLockPoll)
	defer ticker.Stop()
	for {
		release, acquired, err := tryAcquireFileLock(lockPath)
		if err != nil {
			return nil, fmt.Errorf("lock active-account state at %s; check its permissions and retry: %w", lockPath, err)
		}
		if acquired {
			return release, nil
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("wait for another hop process to finish updating active-account state; retry the command: %w", ctx.Err())
		case <-deadline.C:
			return nil, fmt.Errorf("wait for another hop process to finish updating active-account state; it did not finish within %s, retry the command: state lock timeout", refreshLockWait)
		case <-ticker.C:
		}
	}
}

func (fetcher claudeLiveFetcher) FetchUsage(ctx context.Context) (provider.Usage, error) {
	credentials, err := fetcher.store.Read(ctx)
	if err != nil {
		return provider.Usage{}, err
	}
	return fetcher.adapter.FetchUsage(ctx, credentials)
}

func (fetcher codexLiveFetcher) FetchUsage(ctx context.Context) (provider.Usage, error) {
	credentials, err := fetcher.store.Read()
	if err != nil {
		return provider.Usage{}, err
	}
	return fetcher.adapter.FetchUsage(ctx, credentials)
}

func (fetcher failingFetcher) FetchUsage(context.Context) (provider.Usage, error) {
	return provider.Usage{}, fetcher.err
}
