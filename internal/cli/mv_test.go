package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/janiorvalle/hop/internal/state"
	"github.com/janiorvalle/hop/internal/vault"
)

func TestRenameAccountMovesInactiveSlotWithItsContents(t *testing.T) {
	t.Parallel()

	accountVault := newTestVault(t)
	slotPath, err := accountVault.EnsureSlot("codex", "old")
	if err != nil {
		t.Fatalf("EnsureSlot() error = %v", err)
	}
	if err := writeSlotMetadata(slotPath, slotMetadata{RefreshPolicy: managedRefreshPolicy, Email: "old@example.com"}); err != nil {
		t.Fatalf("writeSlotMetadata() error = %v", err)
	}
	activeState := state.New()
	activeState.SetActive("codex", "keep")
	if err := activeState.Save(accountVault.Root()); err != nil {
		t.Fatalf("state.Save() error = %v", err)
	}
	var stdout bytes.Buffer
	renamer := accountRenamer{vault: accountVault, stdout: &stdout}

	if err := renamer.Rename("codex", "old", "new"); err != nil {
		t.Fatalf("Rename() error = %v", err)
	}
	if _, err := os.Stat(slotPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("renamed slot still exists at the old path: %v", err)
	}
	newPath, _ := accountVault.SlotPath("codex", "new")
	if got := readSlotMetadata(t, newPath); got.Email != "old@example.com" {
		t.Fatalf("slot metadata email = %q, want the moved slot's contents", got.Email)
	}
	loaded, err := state.Load(accountVault.Root())
	if err != nil {
		t.Fatalf("state.Load() error = %v", err)
	}
	if active, _ := loaded.Active("codex"); active != "keep" {
		t.Fatalf("active codex account = %q, want the untouched %q", active, "keep")
	}
	if got := stdout.String(); !strings.Contains(got, `Renamed codex account "old" to "new".`) {
		t.Fatalf("stdout = %q, want rename receipt", got)
	}
	if got := stdout.String(); strings.Contains(got, "stays active") {
		t.Fatalf("stdout = %q, want no active-account note for an inactive slot", got)
	}
}

func TestRenameAccountKeepsActiveAccountActiveUnderItsNewName(t *testing.T) {
	t.Parallel()

	accountVault := newTestVault(t)
	seedActiveClaudeAccount(t, accountVault, "work1")
	var stdout bytes.Buffer
	renamer := accountRenamer{vault: accountVault, stdout: &stdout}

	if err := renamer.Rename("claude", "work1", "personal"); err != nil {
		t.Fatalf("Rename() error = %v", err)
	}
	loaded, err := state.Load(accountVault.Root())
	if err != nil {
		t.Fatalf("state.Load() error = %v", err)
	}
	if active, found := loaded.Active("claude"); !found || active != "personal" {
		t.Fatalf("active claude account = %q, %t; want %q under the new name", active, found, "personal")
	}
	oldPath, _ := accountVault.SlotPath("claude", "work1")
	if _, err := os.Stat(oldPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("renamed slot still exists at the old path: %v", err)
	}
	credentialsPath, _ := accountVault.CredentialsPath("claude", "personal")
	if _, err := os.Stat(credentialsPath); err != nil {
		t.Fatalf("credentials did not move with the slot: %v", err)
	}
	if got := stdout.String(); !strings.Contains(got, `Renamed claude account "work1" to "personal". It stays active.`) {
		t.Fatalf("stdout = %q, want rename receipt with the active note", got)
	}
}

func TestRenameAccountExplainsMissingSlot(t *testing.T) {
	t.Parallel()

	renamer := accountRenamer{vault: newTestVault(t), stdout: &bytes.Buffer{}}
	err := renamer.Rename("codex", "missing", "new")
	if err == nil || !strings.Contains(err.Error(), "run 'hop ls'") {
		t.Fatalf("Rename() error = %v, want account-list next step", err)
	}
}

func TestRenameAccountRefusesExistingTarget(t *testing.T) {
	t.Parallel()

	accountVault := newTestVault(t)
	for _, name := range []string{"old", "taken"} {
		if _, err := accountVault.EnsureSlot("codex", name); err != nil {
			t.Fatalf("EnsureSlot(%s) error = %v", name, err)
		}
	}
	renamer := accountRenamer{vault: accountVault, stdout: io.Discard}
	err := renamer.Rename("codex", "old", "taken")
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("Rename() error = %v, want existing-target refusal", err)
	}
	oldPath, _ := accountVault.SlotPath("codex", "old")
	if _, err := os.Stat(oldPath); err != nil {
		t.Fatalf("source slot was changed by a refused rename: %v", err)
	}
}

func TestRenameAccountRefusesUnchangedName(t *testing.T) {
	t.Parallel()

	accountVault := newTestVault(t)
	if _, err := accountVault.EnsureSlot("codex", "work"); err != nil {
		t.Fatalf("EnsureSlot() error = %v", err)
	}
	renamer := accountRenamer{vault: accountVault, stdout: io.Discard}
	err := renamer.Rename("codex", "work", "work")
	if err == nil || !strings.Contains(err.Error(), "already has that name") {
		t.Fatalf("Rename() error = %v, want unchanged-name refusal", err)
	}
}

func TestRenameAccountRejectsInvalidNewName(t *testing.T) {
	t.Parallel()

	renamer := accountRenamer{vault: newTestVault(t), stdout: io.Discard}
	err := renamer.Rename("codex", "old", "bad name")
	if err == nil || !errors.Is(err, vault.ErrInvalidSlot) {
		t.Fatalf("Rename() error = %v, want invalid-name refusal", err)
	}
}

func TestRenameAccountRefusesSlotBeingEnrolled(t *testing.T) {
	t.Parallel()

	accountVault := newTestVault(t)
	manager := loginManager{vault: accountVault}
	reservation, err := manager.reserveNewSlot("codex", "work")
	if err != nil {
		t.Fatalf("reserveNewSlot() error = %v", err)
	}
	defer reservation.Cleanup()

	renamer := accountRenamer{vault: accountVault, stdout: io.Discard}
	err = renamer.Rename("codex", "work", "play")
	if err == nil || !strings.Contains(err.Error(), "being enrolled") {
		t.Fatalf("Rename() error = %v, want enrollment-in-progress guidance", err)
	}
}

func TestRenameAccountPreservesClaudeStagingRecoverySlot(t *testing.T) {
	t.Parallel()

	accountVault := newTestVault(t)
	seedActiveClaudeAccount(t, accountVault, "work")
	record, err := json.Marshal(claudeStagingRecord{ActiveAccount: "work", ProcessID: os.Getpid(), CreatedAt: time.Now().UTC()})
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(accountVault.Root(), claudeStagingFilename), record, 0o600); err != nil {
		t.Fatalf("WriteFile(transaction) error = %v", err)
	}
	renamer := accountRenamer{vault: accountVault, stdout: io.Discard}

	err = renamer.Rename("claude", "work", "play")
	if err == nil || !strings.Contains(err.Error(), "needed to restore") {
		t.Fatalf("Rename() error = %v, want staging recovery guard", err)
	}
	workPath, _ := accountVault.SlotPath("claude", "work")
	if _, err := os.Stat(workPath); err != nil {
		t.Fatalf("recovery slot was renamed away: %v", err)
	}
}

func TestRenameAccountWaitsForManagedRefreshAndLeavesNoLockBehind(t *testing.T) {
	t.Parallel()

	accountVault := newTestVault(t)
	slotPath, err := accountVault.EnsureSlot("codex", "old")
	if err != nil {
		t.Fatalf("EnsureSlot() error = %v", err)
	}
	releaseRefresh, err := acquireRefreshLock(context.Background(), slotPath)
	if err != nil {
		t.Fatalf("acquireRefreshLock() error = %v", err)
	}
	renamer := accountRenamer{vault: accountVault, stdout: &bytes.Buffer{}}
	result := make(chan error, 1)
	go func() { result <- renamer.Rename("codex", "old", "new") }()

	select {
	case err := <-result:
		t.Fatalf("Rename() returned before refresh released: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	releaseRefresh()
	if err := <-result; err != nil {
		t.Fatalf("Rename() error = %v", err)
	}
	if _, err := os.Stat(slotPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("slot exists at the old path after coordinated rename: %v", err)
	}
	newPath, _ := accountVault.SlotPath("codex", "new")
	if _, err := os.Stat(newPath); err != nil {
		t.Fatalf("renamed slot missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(newPath, refreshLockFilename)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("refresh lock followed the slot and was not released: %v", err)
	}
}
