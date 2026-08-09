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
)

func TestRemoveAccountDeletesInactiveSlot(t *testing.T) {
	t.Parallel()

	accountVault := newTestVault(t)
	slotPath, err := accountVault.EnsureSlot("codex", "old")
	if err != nil {
		t.Fatalf("EnsureSlot() error = %v", err)
	}
	var stdout bytes.Buffer
	remover := accountRemover{vault: accountVault, stdout: &stdout}

	if err := remover.Remove("codex", "old"); err != nil {
		t.Fatalf("Remove() error = %v", err)
	}
	if _, err := os.Stat(slotPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("removed slot still exists: %v", err)
	}
	if got := stdout.String(); !strings.Contains(got, `Removed codex account "old"`) {
		t.Fatalf("stdout = %q, want removal receipt", got)
	}
}

func TestRemoveAccountDeletesActiveSlotAndClearsState(t *testing.T) {
	t.Parallel()

	accountVault := newTestVault(t)
	slotPath, err := accountVault.EnsureSlot("claude", "work")
	if err != nil {
		t.Fatalf("EnsureSlot() error = %v", err)
	}
	activeState := state.New()
	activeState.SetActive("claude", "work")
	if err := activeState.Save(accountVault.Root()); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	remover := accountRemover{vault: accountVault, stdout: io.Discard}

	if err := remover.Remove("claude", "work"); err != nil {
		t.Fatalf("Remove() error = %v", err)
	}
	if _, err := os.Stat(slotPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("active slot still exists: %v", err)
	}
	loaded, err := state.Load(accountVault.Root())
	if err != nil {
		t.Fatalf("state.Load() error = %v", err)
	}
	if active, found := loaded.Active("claude"); found {
		t.Fatalf("active Claude account = %q, true; want absent", active)
	}
}

func TestRemoveAccountExplainsMissingSlot(t *testing.T) {
	t.Parallel()

	remover := accountRemover{vault: newTestVault(t), stdout: &bytes.Buffer{}}
	err := remover.Remove("codex", "missing")
	if err == nil || !strings.Contains(err.Error(), "run 'hop ls'") {
		t.Fatalf("Remove() error = %v, want account-list next step", err)
	}
}

func TestRemoveAccountRefusesSlotBeingEnrolled(t *testing.T) {
	t.Parallel()

	accountVault := newTestVault(t)
	manager := loginManager{vault: accountVault}
	reservation, err := manager.reserveNewSlot("codex", "work")
	if err != nil {
		t.Fatalf("reserveNewSlot() error = %v", err)
	}
	defer reservation.Cleanup()

	remover := accountRemover{vault: accountVault, stdout: io.Discard}
	err = remover.Remove("codex", "work")
	if err == nil || !strings.Contains(err.Error(), "being enrolled") {
		t.Fatalf("Remove() error = %v, want enrollment-in-progress guidance", err)
	}
}

func TestRemoveAccountPreservesClaudeStagingRecoverySlot(t *testing.T) {
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
	remover := accountRemover{vault: accountVault, stdout: io.Discard}

	err = remover.Remove("claude", "work")
	if err == nil || !strings.Contains(err.Error(), "needed to restore") {
		t.Fatalf("Remove() error = %v, want staging recovery guard", err)
	}
	workPath, _ := accountVault.SlotPath("claude", "work")
	if _, err := os.Stat(workPath); err != nil {
		t.Fatalf("recovery slot was removed: %v", err)
	}
}

func TestRemoveAccountRechecksStagingAfterWaitingForClaudeLoginLock(t *testing.T) {
	t.Parallel()

	accountVault := newTestVault(t)
	seedActiveClaudeAccount(t, accountVault, "work")
	releaseLogin, err := acquireClaudeLoginLock(context.Background(), accountVault.Root())
	if err != nil {
		t.Fatalf("acquireClaudeLoginLock() error = %v", err)
	}
	remover := accountRemover{vault: accountVault, stdout: io.Discard}
	result := make(chan error, 1)
	go func() { result <- remover.Remove("claude", "work") }()
	select {
	case err := <-result:
		t.Fatalf("Remove() returned while Claude login lock held: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	record, err := json.Marshal(claudeStagingRecord{ActiveAccount: "work", ProcessID: os.Getpid(), CreatedAt: time.Now().UTC()})
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(accountVault.Root(), claudeStagingFilename), record, 0o600); err != nil {
		t.Fatalf("WriteFile(transaction) error = %v", err)
	}
	releaseLogin()
	err = <-result
	if err == nil || !strings.Contains(err.Error(), "needed to restore") {
		t.Fatalf("Remove() error = %v, want post-lock staging guard", err)
	}
	workPath, _ := accountVault.SlotPath("claude", "work")
	if _, err := os.Stat(workPath); err != nil {
		t.Fatalf("staging recovery slot was removed: %v", err)
	}
}

func TestRemoveAccountWaitsForManagedRefreshBeforeDeletingSlot(t *testing.T) {
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
	remover := accountRemover{vault: accountVault, stdout: &bytes.Buffer{}}
	result := make(chan error, 1)
	go func() { result <- remover.Remove("codex", "old") }()

	select {
	case err := <-result:
		t.Fatalf("Remove() returned before refresh released: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	releaseRefresh()
	if err := <-result; err != nil {
		t.Fatalf("Remove() error = %v", err)
	}
	if _, err := os.Stat(slotPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("slot exists after coordinated removal: %v", err)
	}
}

func TestConcurrentActiveRemovalsDoNotResurrectState(t *testing.T) {
	t.Parallel()

	accountVault := newTestVault(t)
	for _, entry := range []struct{ provider, account string }{{"claude", "work"}, {"codex", "personal"}} {
		if _, err := accountVault.EnsureSlot(entry.provider, entry.account); err != nil {
			t.Fatalf("EnsureSlot(%s) error = %v", entry.provider, err)
		}
	}
	activeState := state.New()
	activeState.SetActive("claude", "work")
	activeState.SetActive("codex", "personal")
	if err := activeState.Save(accountVault.Root()); err != nil {
		t.Fatalf("state.Save() error = %v", err)
	}
	remover := accountRemover{vault: accountVault, stdout: io.Discard}
	start := make(chan struct{})
	results := make(chan error, 2)
	for _, entry := range []struct{ provider, account string }{{"claude", "work"}, {"codex", "personal"}} {
		entry := entry
		go func() {
			<-start
			results <- remover.Remove(entry.provider, entry.account)
		}()
	}
	close(start)
	for range 2 {
		if err := <-results; err != nil {
			t.Fatalf("concurrent Remove() error = %v", err)
		}
	}
	loaded, err := state.Load(accountVault.Root())
	if err != nil {
		t.Fatalf("state.Load() error = %v", err)
	}
	if len(loaded.ActiveAccounts) != 0 {
		t.Fatalf("active accounts after concurrent removal = %v, want empty", loaded.ActiveAccounts)
	}
}
