package state

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestLoadMissingStateReturnsWritableEmptyState(t *testing.T) {
	t.Parallel()

	loaded, err := Load(t.TempDir())
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if len(loaded.ActiveAccounts) != 0 {
		t.Fatalf("Load() active accounts = %v, want empty", loaded.ActiveAccounts)
	}
	loaded.SetActive("claude", "work")
	if account, found := loaded.Active("claude"); !found || account != "work" {
		t.Fatalf("Active(claude) = %q, %t; want work, true", account, found)
	}
}

func TestSaveAndLoadRoundTrip(t *testing.T) {
	t.Parallel()

	root := filepath.Join(t.TempDir(), ".hop")
	want := New()
	want.SetActive("claude", "work")
	want.SetActive("codex", "personal")

	if err := want.Save(root); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	got, err := Load(root)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	for provider, wantAccount := range want.ActiveAccounts {
		gotAccount, found := got.Active(provider)
		if !found || gotAccount != wantAccount {
			t.Errorf("Active(%q) = %q, %t; want %q, true", provider, gotAccount, found, wantAccount)
		}
	}

	if runtime.GOOS != "windows" {
		info, statErr := os.Stat(filepath.Join(root, Filename))
		if statErr != nil {
			t.Fatalf("Stat() error = %v", statErr)
		}
		if gotMode := info.Mode().Perm(); gotMode != 0o600 {
			t.Errorf("state permissions = %o, want 600", gotMode)
		}
	}
}

func TestLoadInvalidStateExplainsRecovery(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, Filename), []byte("{"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	_, err := Load(root)
	if err == nil {
		t.Fatal("Load() error = nil, want invalid JSON error")
	}
	if !strings.Contains(err.Error(), "fix or remove") {
		t.Fatalf("Load() error = %q, want recovery step", err)
	}
}

func TestSaveZeroValueUsesEmptyObjectInsteadOfNull(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	if err := (State{}).Save(root); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	contents, err := os.ReadFile(filepath.Join(root, Filename))
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if got := string(contents); !strings.Contains(got, `"active": {}`) {
		t.Fatalf("state.json = %q, want empty active object", got)
	}
}
