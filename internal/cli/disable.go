package cli

import (
	"context"
	"fmt"
	"io"

	"github.com/janiorvalle/hop/internal/state"
	"github.com/janiorvalle/hop/internal/vault"
)

type availability struct {
	disabled bool
	receipt  string
}

var (
	parked    = availability{disabled: true, receipt: "Disabled"}
	available = availability{disabled: false, receipt: "Enabled"}
)

type accountParker struct {
	vault  vault.Vault
	stdout io.Writer
}

func disableAccount(providerName, accountName string, stdout io.Writer) error {
	return withRecoveredVault(stdout, func(accountVault vault.Vault) error {
		return (accountParker{vault: accountVault, stdout: stdout}).setLocked(providerName, accountName, parked)
	})
}

func enableAccount(providerName, accountName string, stdout io.Writer) error {
	return withRecoveredVault(stdout, func(accountVault vault.Vault) error {
		return (accountParker{vault: accountVault, stdout: stdout}).setLocked(providerName, accountName, available)
	})
}

func (parker accountParker) Disable(providerName, accountName string) error {
	return parker.set(providerName, accountName, parked)
}

func (parker accountParker) Enable(providerName, accountName string) error {
	return parker.set(providerName, accountName, available)
}

func (parker accountParker) set(providerName, accountName string, wanted availability) error {
	manager := switchManager{vault: parker.vault}
	releaseProviders, err := manager.lockProviders(context.Background())
	if err != nil {
		return err
	}
	defer releaseProviders()
	releaseState, err := acquireStateLock(context.Background(), parker.vault.Root())
	if err != nil {
		return err
	}
	defer releaseState()
	return parker.setLocked(providerName, accountName, wanted)
}

func (parker accountParker) setLocked(providerName, accountName string, wanted availability) error {
	slotPath, err := settledSlotPath(parker.vault, providerName, accountName)
	if err != nil {
		return err
	}
	releaseRefresh, err := acquireRefreshLock(context.Background(), slotPath)
	if err != nil {
		return fmt.Errorf("wait to change %s account %q until its token refresh finishes: %w", providerName, accountName, err)
	}
	defer releaseRefresh()
	metadata, err := loadSlotMetadata(slotPath)
	if err != nil {
		return err
	}
	metadata.Disabled = wanted.disabled
	if err := writeSlotMetadata(slotPath, metadata); err != nil {
		return err
	}
	activeState, err := state.Load(parker.vault.Root())
	if err != nil {
		return err
	}
	message := fmt.Sprintf("%s %s account %q.", wanted.receipt, providerName, accountName)
	if activeAccount, found := activeState.Active(providerName); found && activeAccount == accountName {
		message += " The live provider login was not changed."
	}
	_, err = fmt.Fprintln(parker.stdout, message)
	return err
}
