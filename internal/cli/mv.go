package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/janiorvalle/hop/internal/state"
	"github.com/janiorvalle/hop/internal/vault"
)

type accountRenamer struct {
	vault  vault.Vault
	stdout io.Writer
}

func renameAccount(providerName, currentName, newName string, stdout io.Writer) error {
	return withRecoveredVault(stdout, func(accountVault vault.Vault) error {
		return (accountRenamer{vault: accountVault, stdout: stdout}).renameLocked(providerName, currentName, newName)
	})
}

func (renamer accountRenamer) Rename(providerName, currentName, newName string) error {
	manager := switchManager{vault: renamer.vault}
	releaseProviders, err := manager.lockProviders(context.Background())
	if err != nil {
		return err
	}
	defer releaseProviders()
	releaseState, err := acquireStateLock(context.Background(), renamer.vault.Root())
	if err != nil {
		return err
	}
	defer releaseState()
	return renamer.renameLocked(providerName, currentName, newName)
}

func (renamer accountRenamer) renameLocked(providerName, currentName, newName string) error {
	newSlot, err := renamer.vault.SlotPath(providerName, newName)
	if err != nil {
		return err
	}
	if currentName == newName {
		return fmt.Errorf("%s account %q already has that name; pick a different name or run 'hop ls' to review accounts", providerName, currentName)
	}
	currentSlot, err := settledSlotPath(renamer.vault, providerName, currentName)
	if err != nil {
		return err
	}
	if _, err := os.Stat(newSlot); err == nil {
		return fmt.Errorf("%s account %q already exists; pick another name, or run 'hop rm %s %s' first if it should go", providerName, newName, providerName, newName)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect %s account %q before renaming onto it; check its permissions and retry: %w", providerName, newName, err)
	}
	releaseRefresh, err := acquireRefreshLock(context.Background(), currentSlot)
	if err != nil {
		return fmt.Errorf("wait to rename %s account %q until its token refresh finishes: %w", providerName, currentName, err)
	}
	defer releaseRefresh()
	activeState, err := state.Load(renamer.vault.Root())
	if err != nil {
		return err
	}
	activeAccount, isActive := activeState.Active(providerName)
	isActive = isActive && activeAccount == currentName
	if isActive {
		activeState.SetActive(providerName, newName)
		if err := activeState.Save(renamer.vault.Root()); err != nil {
			return fmt.Errorf("record %s account %q as active under its new name; the slot was not changed, fix the hop directory permissions and retry: %w", providerName, currentName, err)
		}
	}
	if err := os.Rename(currentSlot, newSlot); err != nil {
		failure := fmt.Errorf("rename %s account %q to %q; the slot was not changed, check its permissions and retry: %w", providerName, currentName, newName, err)
		if isActive {
			activeState.SetActive(providerName, currentName)
			if restoreErr := activeState.Save(renamer.vault.Root()); restoreErr != nil {
				failure = errors.Join(failure, fmt.Errorf("restore %s account %q as active after the rename failed; repair %s before retrying: %w", providerName, currentName, filepath.Join(renamer.vault.Root(), state.Filename), restoreErr))
			}
		}
		return failure
	}
	// The refresh lock traveled with the slot; release it at its new path so the
	// deferred release of the old path stays a no-op. A leftover would block the
	// slot's token refresh until the stale-lock timeout.
	_ = os.Remove(filepath.Join(newSlot, refreshLockFilename))
	message := fmt.Sprintf("Renamed %s account %q to %q.", providerName, currentName, newName)
	if isActive {
		message += " It stays active."
	}
	_, err = fmt.Fprintln(renamer.stdout, message)
	return err
}
