package livefile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestClearIfMatchesPreservesReplacementWrittenDuringComparison(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json")
	if err := os.WriteFile(path, []byte("installed"), 0o600); err != nil {
		t.Fatal(err)
	}

	err := ClearIfMatches(path, "test credentials", func(quarantine string) (bool, error) {
		contents, err := os.ReadFile(quarantine)
		if err != nil {
			return false, err
		}
		if err := os.WriteFile(path, []byte("replacement"), 0o600); err != nil {
			return false, err
		}
		return string(contents) == "installed", nil
	})
	if err == nil || !strings.Contains(err.Error(), "left that new login untouched") {
		t.Fatalf("ClearIfMatches() error = %v, want concurrent-replacement notice", err)
	}
	contents, readErr := os.ReadFile(path)
	if readErr != nil || string(contents) != "replacement" {
		t.Fatalf("live file = %q, %v; want replacement preserved", contents, readErr)
	}
}

func TestClearIfMatchesRestoresUnexpectedCredential(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json")
	if err := os.WriteFile(path, []byte("unexpected"), 0o600); err != nil {
		t.Fatal(err)
	}

	err := ClearIfMatches(path, "test credentials", func(string) (bool, error) {
		return false, nil
	})
	if err == nil || !strings.Contains(err.Error(), "preserved the unexpected login") {
		t.Fatalf("ClearIfMatches() error = %v, want unexpected-login refusal", err)
	}
	contents, readErr := os.ReadFile(path)
	if readErr != nil || string(contents) != "unexpected" {
		t.Fatalf("live file = %q, %v; want unexpected credential restored", contents, readErr)
	}
}
