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
	accountVault, err := defaultVault()
	if err != nil {
		return err
	}
	manager, err := defaultSwitchManager(stdout)
	if err != nil {
		return err
	}
	releaseProviders, err := manager.lockProviders(context.Background())
	if err != nil {
		return err
	}
	defer releaseProviders()
	releaseState, err := acquireStateLock(context.Background(), accountVault.Root())
	if err != nil {
		return err
	}
	defer releaseState()
	recovered, err := manager.recoverInterruptedSwitch(context.Background())
	if err != nil {
		return err
	}
	if recovered {
		_, _ = fmt.Fprintln(stdout, "Recovered an interrupted account switch before continuing.")
	}
	return (accountRenamer{vault: accountVault, stdout: stdout}).renameLocked(providerName, currentName, newName)
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
	currentSlot, err := renamer.vault.SlotPath(providerName, currentName)
	if err != nil {
		return err
	}
	newSlot, err := renamer.vault.SlotPath(providerName, newName)
	if err != nil {
		return err
	}
	if currentName == newName {
		return fmt.Errorf("%s account %q already has that name; pick a different name or run 'hop ls' to review accounts", providerName, currentName)
	}
	if _, err := os.Stat(currentSlot); errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%s account %q does not exist; run 'hop ls' to see enrolled accounts", providerName, currentName)
	} else if err != nil {
		return fmt.Errorf("inspect %s account %q before renaming; check its permissions and retry: %w", providerName, currentName, err)
	}
	if _, err := os.Stat(newSlot); err == nil {
		return fmt.Errorf("%s account %q already exists; pick another name, or run 'hop rm %s %s' first if it should go", providerName, newName, providerName, newName)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect %s account %q before renaming onto it; check its permissions and retry: %w", providerName, newName, err)
	}
	if _, err := os.Stat(filepath.Join(currentSlot, slotReservationFilename)); err == nil {
		_, active, inspectErr := inspectSlotReservation(currentSlot)
		if inspectErr != nil {
			return fmt.Errorf("inspect the enrollment owner for %s account %q; fix or remove %s and retry: %w", providerName, currentName, filepath.Join(currentSlot, slotReservationFilename), inspectErr)
		}
		if active {
			return fmt.Errorf("%s account %q is being enrolled; wait for login to finish, then retry the rename", providerName, currentName)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect the enrollment state for %s account %q; check its permissions and retry: %w", providerName, currentName, err)
	}
	if providerName == "claude" {
		transaction, found, err := readClaudeStagingRecord(renamer.vault.Root())
		if err != nil {
			return err
		}
		if found && transaction.ActiveAccount == currentName {
			return fmt.Errorf("claude account %q is needed to restore an enrollment in progress; wait for process %d, or rerun 'hop login claude %s' to recover it before renaming", currentName, transaction.ProcessID, currentName)
		}
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
