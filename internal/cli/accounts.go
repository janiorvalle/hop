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
	refreshTokenWarnLead      = 7 * 24 * time.Hour
	refreshTokenCriticalLead  = 2 * 24 * time.Hour
	refreshLockWait           = 20 * time.Second
	refreshLockStale          = 2 * time.Minute
	refreshLockPoll           = 25 * time.Millisecond
	refreshTransactionTimeout = 20 * time.Second
	stateLockFilename         = ".state.lock"
	refreshLockFilename       = ".refresh.lock"
)

// errRotatedTokensLost marks a rotation the provider completed but hop could not
// persist: the token on disk is already revoked, so the row must say so now.
var errRotatedTokensLost = errors.New("rotated tokens were not preserved")

type tokenLifetimes interface {
	NeedsRefresh(now time.Time, skew time.Duration) bool
	RefreshTokenExpiry() time.Time
}

type rotationOutcome string

const (
	rotationUnmanaged rotationOutcome = "unmanaged"
	rotationFresh     rotationOutcome = "fresh"
	rotationRotated   rotationOutcome = "rotated"
)

// rotation is what one idle slot reports after the refresh decision the glance
// makes in Prepare has run for it.
type rotation struct {
	Outcome            rotationOutcome
	RefreshTokenExpiry time.Time
}

type slotMetadata struct {
	RefreshPolicy string `json:"refresh_policy,omitempty"`
	Email         string `json:"email,omitempty"`
	AccountUUID   string `json:"account_uuid,omitempty"`
	Disabled      bool   `json:"disabled,omitempty"`
}

type account struct {
	Provider provider.Name
	Name     string
	Active   bool
	Disabled bool
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
	claudeDependencies := defaultClaudeLiveDependencies()
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
		claudeLive:    claudeDependencies.store,
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
	credentialsPath, err := catalog.vault.CredentialsPath(string(providerName), name)
	if err != nil {
		return account{Provider: providerName, Name: name, Active: isActive, Fetcher: failingFetcher{err: err}}
	}
	metadata, err := loadSlotMetadata(filepath.Dir(credentialsPath))
	if err != nil {
		return account{Provider: providerName, Name: name, Active: isActive, Fetcher: failingFetcher{err: err}}
	}
	if metadata.Disabled {
		return account{Provider: providerName, Name: name, Active: isActive, Disabled: true}
	}
	if isActive {
		// Live credentials belong to the provider CLI. A glance may read them,
		// but only that CLI may rotate and persist its live refresh token.
		return account{Provider: providerName, Name: name, Active: true, Fetcher: catalog.liveFetcher(providerName)}
	}
	return account{
		Provider: providerName,
		Name:     name,
		Fetcher:  catalog.slotFetcher(providerName, credentialsPath, metadata.RefreshPolicy == managedRefreshPolicy),
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

func loadSlotMetadata(slotPath string) (slotMetadata, error) {
	metadataPath := filepath.Join(slotPath, slotMetadataFilename)
	contents, err := os.ReadFile(metadataPath)
	if errors.Is(err, os.ErrNotExist) {
		// Manually seeded slots are read-only. The login flow opts a slot into
		// rotation only after hop has taken custody of its refresh token.
		return slotMetadata{}, nil
	}
	if err != nil {
		return slotMetadata{}, fmt.Errorf("read slot metadata from %s; fix its permissions or run 'hop login' again: %w", metadataPath, err)
	}
	var metadata slotMetadata
	if err := json.Unmarshal(contents, &metadata); err != nil {
		return slotMetadata{}, fmt.Errorf("read slot metadata from %s; expected {\"refresh_policy\":\"managed\"}, fix the file or run 'hop login' again: %w", metadataPath, err)
	}
	return metadata, nil
}

// writeManagedSlotMetadata carries a parked slot's disabled flag through a
// login rewrite, so only 'hop enable' brings an account back. A slot.json that
// cannot be read is replaced, because login is its repair path.
func writeManagedSlotMetadata(slotPath string, identity claude.Profile) error {
	metadata, err := loadSlotMetadata(slotPath)
	if err != nil {
		metadata = slotMetadata{}
	}
	metadata.RefreshPolicy = managedRefreshPolicy
	metadata.Email = identity.Email
	metadata.AccountUUID = identity.AccountUUID
	return writeSlotMetadata(slotPath, metadata)
}

func writeSlotMetadata(slotPath string, metadata slotMetadata) error {
	if err := os.MkdirAll(slotPath, 0o700); err != nil {
		return fmt.Errorf("create account slot %s: %w", slotPath, err)
	}
	if err := os.Chmod(slotPath, 0o700); err != nil {
		return fmt.Errorf("secure account slot %s: %w", slotPath, err)
	}
	contents, err := json.MarshalIndent(metadata, "", "  ")
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
	if err != nil || !slotNeedsRotation(credentials, fetcher.now()) {
		return err
	}
	_, err = refreshClaudeSlot(ctx, fetcher.adapter, fetcher.store, fetcher.now())
	return rotationErrorFor(credentials, fetcher.now(), err)
}

func (fetcher codexSlotFetcher) Prepare(ctx context.Context) error {
	if !fetcher.refreshAllowed {
		return nil
	}
	credentials, err := fetcher.store.Read()
	if err != nil || !slotNeedsRotation(credentials, fetcher.now()) {
		return err
	}
	_, err = refreshCodexSlot(ctx, fetcher.adapter, fetcher.store, fetcher.now())
	return rotationErrorFor(credentials, fetcher.now(), err)
}

func (fetcher claudeSlotFetcher) Rotate(ctx context.Context) (rotation, error) {
	if !fetcher.refreshAllowed {
		return rotation{Outcome: rotationUnmanaged}, nil
	}
	credentials, err := fetcher.store.Read()
	if err != nil {
		return rotation{}, err
	}
	if !slotNeedsRotation(credentials, fetcher.now()) {
		return rotation{Outcome: rotationFresh, RefreshTokenExpiry: credentials.RefreshTokenExpiry()}, nil
	}
	rotated, err := refreshClaudeSlot(ctx, fetcher.adapter, fetcher.store, fetcher.now())
	if err != nil {
		return rotation{}, err
	}
	return rotation{Outcome: rotationRotated, RefreshTokenExpiry: rotated.RefreshTokenExpiry()}, nil
}

func (fetcher codexSlotFetcher) Rotate(ctx context.Context) (rotation, error) {
	if !fetcher.refreshAllowed {
		return rotation{Outcome: rotationUnmanaged}, nil
	}
	credentials, err := fetcher.store.Read()
	if err != nil {
		return rotation{}, err
	}
	if !slotNeedsRotation(credentials, fetcher.now()) {
		return rotation{Outcome: rotationFresh, RefreshTokenExpiry: credentials.RefreshTokenExpiry()}, nil
	}
	rotated, err := refreshCodexSlot(ctx, fetcher.adapter, fetcher.store, fetcher.now())
	if err != nil {
		return rotation{}, err
	}
	return rotation{Outcome: rotationRotated, RefreshTokenExpiry: rotated.RefreshTokenExpiry()}, nil
}

func (fetcher failingFetcher) Rotate(context.Context) (rotation, error) {
	return rotation{}, fetcher.err
}

// A rotation that only ran early, while the access token still works and the
// slot is unchanged, must not cost the row its usage: the row's expiry warning
// names the fix and the next glance retries the rotation.
func rotationErrorFor(credentials tokenLifetimes, now time.Time, rotationErr error) error {
	if rotationErr == nil || credentials.NeedsRefresh(now, refreshSkew) || errors.Is(rotationErr, errRotatedTokensLost) {
		return rotationErr
	}
	return nil
}

func slotNeedsRotation(credentials tokenLifetimes, now time.Time) bool {
	return credentials.NeedsRefresh(now, refreshSkew) || expiresWithin(credentials.RefreshTokenExpiry(), now, refreshTokenWarnLead)
}

func expiresWithin(expiry, now time.Time, lead time.Duration) bool {
	return !expiry.IsZero() && !expiry.After(now.Add(lead))
}

func refreshTokenSeverity(expiry, now time.Time) string {
	switch {
	case expiresWithin(expiry, now, refreshTokenCriticalLead):
		return "critical"
	case expiresWithin(expiry, now, refreshTokenWarnLead):
		return "warning"
	default:
		return "normal"
	}
}

func refreshClaudeSlot(ctx context.Context, adapter claude.Adapter, store claude.Store, now time.Time) (claude.Credentials, error) {
	if fileStore, ok := store.(claude.FileStore); ok {
		release, err := acquireRefreshLock(ctx, filepath.Dir(fileStore.Path))
		if err != nil {
			return claude.Credentials{}, err
		}
		defer release()
		credentials, err := fileStore.Read()
		if err != nil || !slotNeedsRotation(credentials, now) {
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
		return claude.Credentials{}, fmt.Errorf("save rotated Claude tokens after two write attempts; run 'hop login claude <account>' before retrying because the slot could not retain the recovery copy: %v: %w: %w", recoveryErr, err, errRotatedTokensLost)
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
		if err != nil || !slotNeedsRotation(credentials, now) {
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
		return codex.Credentials{}, fmt.Errorf("save rotated Codex tokens after two write attempts; run 'hop login codex <account>' before retrying because the slot could not retain the recovery copy: %v: %w: %w", recoveryErr, err, errRotatedTokensLost)
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
		return claude.Credentials{}, fmt.Errorf("preserve rotated Claude tokens before replacing the slot; run 'hop login claude <account>' before retrying because the private recovery journal failed: %w: %w", refreshErr, errRotatedTokensLost)
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
		return codex.Credentials{}, fmt.Errorf("preserve rotated Codex tokens before replacing the slot; run 'hop login codex <account>' before retrying because the private recovery journal failed: %w: %w", refreshErr, errRotatedTokensLost)
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
	lockPath := filepath.Join(slotPath, refreshLockFilename)
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
