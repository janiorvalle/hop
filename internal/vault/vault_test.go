package vault

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestEnsureSlotCreatesPrivateCredentialLayout(t *testing.T) {
	t.Parallel()

	root := filepath.Join(t.TempDir(), ".hop")
	vault, err := New(root)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	slot, err := vault.EnsureSlot("claude", "work")
	if err != nil {
		t.Fatalf("EnsureSlot() error = %v", err)
	}
	if want := filepath.Join(root, "claude", "work"); slot != want {
		t.Fatalf("EnsureSlot() = %q, want %q", slot, want)
	}
	credentials, err := vault.CredentialsPath("claude", "work")
	if err != nil {
		t.Fatalf("CredentialsPath() error = %v", err)
	}
	if want := filepath.Join(slot, CredentialsFilename); credentials != want {
		t.Fatalf("CredentialsPath() = %q, want %q", credentials, want)
	}

	if runtime.GOOS != "windows" {
		info, statErr := os.Stat(slot)
		if statErr != nil {
			t.Fatalf("Stat() error = %v", statErr)
		}
		if got := info.Mode().Perm(); got != 0o700 {
			t.Errorf("slot permissions = %o, want 700", got)
		}
	}
}

func TestSlotPathRejectsTraversalAndUnknownProviders(t *testing.T) {
	t.Parallel()

	vault, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	testCases := []struct {
		provider string
		account  string
	}{
		{provider: "other", account: "work"},
		{provider: "codex", account: "../work"},
		{provider: "codex", account: ".."},
		{provider: "codex", account: ""},
	}
	for _, testCase := range testCases {
		_, slotErr := vault.SlotPath(testCase.provider, testCase.account)
		if !errors.Is(slotErr, ErrInvalidSlot) {
			t.Errorf("SlotPath(%q, %q) error = %v, want ErrInvalidSlot", testCase.provider, testCase.account, slotErr)
		}
	}
}
