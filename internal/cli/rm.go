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

type accountRemover struct {
	vault  vault.Vault
	stdout io.Writer
}

func removeAccount(providerName, accountName string, stdout io.Writer) error {
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
	return (accountRemover{vault: accountVault, stdout: stdout}).removeLocked(providerName, accountName)
}

func (remover accountRemover) Remove(providerName, accountName string) error {
	manager := switchManager{vault: remover.vault}
	releaseProviders, err := manager.lockProviders(context.Background())
	if err != nil {
		return err
	}
	defer releaseProviders()
	releaseState, err := acquireStateLock(context.Background(), remover.vault.Root())
	if err != nil {
		return err
	}
	defer releaseState()
	return remover.removeLocked(providerName, accountName)
}

func (remover accountRemover) removeLocked(providerName, accountName string) error {
	slotPath, err := remover.vault.SlotPath(providerName, accountName)
	if err != nil {
		return err
	}
	if _, err := os.Stat(slotPath); errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%s account %q does not exist; run 'hop ls' to see enrolled accounts", providerName, accountName)
	} else if err != nil {
		return fmt.Errorf("inspect %s account %q before removal; check its permissions and retry: %w", providerName, accountName, err)
	}
	if _, err := os.Stat(filepath.Join(slotPath, slotReservationFilename)); err == nil {
		_, active, inspectErr := inspectSlotReservation(slotPath)
		if inspectErr != nil {
			return fmt.Errorf("inspect the enrollment owner for %s account %q; fix or remove %s and retry: %w", providerName, accountName, filepath.Join(slotPath, slotReservationFilename), inspectErr)
		}
		if active {
			return fmt.Errorf("%s account %q is being enrolled; wait for login to finish, then retry removal", providerName, accountName)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect the enrollment state for %s account %q; check its permissions and retry: %w", providerName, accountName, err)
	}
	if providerName == "claude" {
		transaction, found, err := readClaudeStagingRecord(remover.vault.Root())
		if err != nil {
			return err
		}
		if found && transaction.ActiveAccount == accountName {
			return fmt.Errorf("claude account %q is needed to restore an enrollment in progress; wait for process %d, or rerun 'hop login claude %s' to recover it before removal", accountName, transaction.ProcessID, accountName)
		}
	}
	releaseRefresh, err := acquireRefreshLock(context.Background(), slotPath)
	if err != nil {
		return fmt.Errorf("wait to remove %s account %q until its token refresh finishes: %w", providerName, accountName, err)
	}
	defer releaseRefresh()
	activeState, err := state.Load(remover.vault.Root())
	if err != nil {
		return err
	}
	activeAccount, isActive := activeState.Active(providerName)
	isActive = isActive && activeAccount == accountName
	if isActive {
		delete(activeState.ActiveAccounts, providerName)
		if err := activeState.Save(remover.vault.Root()); err != nil {
			return fmt.Errorf("clear %s account %q from active state before removal; the slot was not changed, fix the hop directory permissions and retry: %w", providerName, accountName, err)
		}
	}
	tombstonePath, err := renameSlotForRemoval(slotPath)
	if err != nil {
		if isActive {
			activeState.SetActive(providerName, accountName)
			if restoreErr := activeState.Save(remover.vault.Root()); restoreErr != nil {
				return errors.Join(
					fmt.Errorf("prepare %s account %q for removal; the slot was not changed: %w", providerName, accountName, err),
					fmt.Errorf("restore %s account %q as active after removal preparation failed; repair %s before retrying: %w", providerName, accountName, filepath.Join(remover.vault.Root(), state.Filename), restoreErr),
				)
			}
		}
		return fmt.Errorf("prepare %s account %q for removal; the slot was not changed, check its permissions and retry: %w", providerName, accountName, err)
	}
	if err := os.RemoveAll(tombstonePath); err != nil {
		failure := fmt.Errorf("remove %s account %q from %s: %w", providerName, accountName, tombstonePath, err)
		if isActive {
			activeState.SetActive(providerName, accountName)
			if restoreErr := activeState.Save(remover.vault.Root()); restoreErr != nil {
				failure = errors.Join(failure, fmt.Errorf("restore %s account %q as active after removal failed; repair %s before retrying: %w", providerName, accountName, filepath.Join(remover.vault.Root(), state.Filename), restoreErr))
			}
		}
		if restoreErr := os.Rename(tombstonePath, slotPath); restoreErr != nil {
			failure = errors.Join(failure, fmt.Errorf("restore the account slot after removal failed; move %s back to %s before retrying: %w", tombstonePath, slotPath, restoreErr))
		}
		return failure
	}
	message := fmt.Sprintf("Removed %s account %q.", providerName, accountName)
	if isActive {
		message += " The live provider login was not changed."
	}
	_, err = fmt.Fprintln(remover.stdout, message)
	return err
}

func renameSlotForRemoval(slotPath string) (string, error) {
	placeholder, err := os.CreateTemp(filepath.Dir(slotPath), ".remove-account-*")
	if err != nil {
		return "", err
	}
	tombstonePath := placeholder.Name()
	if err := placeholder.Close(); err != nil {
		_ = os.Remove(tombstonePath)
		return "", err
	}
	if err := os.Remove(tombstonePath); err != nil {
		return "", err
	}
	if err := os.Rename(slotPath, tombstonePath); err != nil {
		return "", err
	}
	return tombstonePath, nil
}
