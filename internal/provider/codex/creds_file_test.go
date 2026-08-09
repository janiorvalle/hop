package codex

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestFileStoreRoundTripPreservesAuthShape(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "codex", "work", "auth.json")
	store := FileStore{Path: path}
	want := Credentials{AuthMode: "chatgpt", IDToken: "id", AccessToken: "access", RefreshToken: "refresh", AccountID: "account", LastRefresh: "2026-08-08T05:00:00Z"}
	if err := store.Write(want); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	got, err := store.Read()
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if got != want {
		t.Errorf("Read() = %+v, want %+v", got, want)
	}
	if runtime.GOOS != "windows" {
		info, statErr := os.Stat(path)
		if statErr != nil {
			t.Fatalf("Stat() error = %v", statErr)
		}
		if gotMode := info.Mode().Perm(); gotMode != 0o600 {
			t.Errorf("credential permissions = %o, want 600", gotMode)
		}
	}
}

func TestRecoveryJournalIsReservedBeforeRotationAndPreferredAfterSave(t *testing.T) {
	t.Parallel()

	store := FileStore{Path: filepath.Join(t.TempDir(), "codex", "work", "auth.json")}
	old := Credentials{AccessToken: "old", RefreshToken: "old-refresh", AccountID: "account"}
	if err := store.Write(old); err != nil {
		t.Fatalf("Write(old) error = %v", err)
	}
	journal, err := store.ReserveRecovery()
	if err != nil {
		t.Fatalf("ReserveRecovery() error = %v", err)
	}
	if got, err := store.Read(); err != nil || got.AccessToken != "old" {
		t.Fatalf("Read() with empty reservation = %q, %v; want old, nil", got.AccessToken, err)
	}
	rotated := Credentials{AccessToken: "rotated", RefreshToken: "rotated-refresh", AccountID: "account"}
	if err := journal.Save(rotated); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	if got, err := store.Read(); err != nil || got.RefreshToken != "rotated-refresh" {
		t.Fatalf("Read() with recovery = token saved %t, %v; want true, nil", got.RefreshToken == "rotated-refresh", err)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(journal.path)
		if err != nil {
			t.Fatalf("Stat(recovery) error = %v", err)
		}
		if got := info.Mode().Perm(); got != 0o600 {
			t.Fatalf("recovery permissions = %o, want 600", got)
		}
	}

	oldTime := time.Now().Add(-time.Hour)
	if err := os.Chtimes(journal.path, oldTime, oldTime); err != nil {
		t.Fatalf("Chtimes(recovery) error = %v", err)
	}
	newest := Credentials{AccessToken: "newest", RefreshToken: "newest-refresh", AccountID: "account"}
	if err := store.Write(newest); err != nil {
		t.Fatalf("Write(newest) error = %v", err)
	}
	if got, err := store.Read(); err != nil || got.RefreshToken != "newest-refresh" {
		t.Fatalf("Read() after newer primary = token saved %t, %v; want true, nil", got.RefreshToken == "newest-refresh", err)
	}
	if _, err := os.Stat(journal.path); !os.IsNotExist(err) {
		t.Fatalf("superseded recovery remains after primary write: %v", err)
	}
}

func TestRecoveryJournalDiscardRemovesUnusedReservation(t *testing.T) {
	t.Parallel()

	store := FileStore{Path: filepath.Join(t.TempDir(), "auth.json")}
	journal, err := store.ReserveRecovery()
	if err != nil {
		t.Fatalf("ReserveRecovery() error = %v", err)
	}
	path := journal.path
	if err := journal.Discard(); err != nil {
		t.Fatalf("Discard() error = %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("Stat(discarded recovery) error = %v, want not exist", err)
	}
}

func TestParseCredentialsRejectsMissingAccountID(t *testing.T) {
	t.Parallel()

	_, err := parseCredentials([]byte(`{"auth_mode":"chatgpt","tokens":{"access_token":"access","refresh_token":"refresh"}}`))
	if err == nil {
		t.Fatal("parseCredentials() error = nil, want missing account ID error")
	}
}

func TestLiveFileWritePreservesParentDirectoryPermissions(t *testing.T) {
	t.Parallel()

	directory := filepath.Join(t.TempDir(), "shared")
	if err := os.Mkdir(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	live := LiveFile{Path: filepath.Join(directory, "auth.json")}
	if err := live.Write(Credentials{AccessToken: "access", RefreshToken: "refresh", AccountID: "account"}); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(directory)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != 0o755 {
			t.Fatalf("parent permissions = %o, want unchanged 755", got)
		}
	}
	if _, err := live.Read(); err != nil {
		t.Fatalf("Read() error = %v", err)
	}
}

func TestLiveFileWriteRequiresExistingParentDirectory(t *testing.T) {
	t.Parallel()

	live := LiveFile{Path: filepath.Join(t.TempDir(), "missing", "auth.json")}
	err := live.Write(Credentials{AccessToken: "access", RefreshToken: "refresh", AccountID: "account"})
	if err == nil || !strings.Contains(err.Error(), "must already exist") {
		t.Fatalf("Write() error = %v, want existing-directory requirement", err)
	}
}

func TestLiveFileClearIfMatchesPreservesUnexpectedCredentials(t *testing.T) {
	directory := t.TempDir()
	live := LiveFile{Path: filepath.Join(directory, "auth.json")}
	unexpected := Credentials{AccessToken: "new", RefreshToken: "new-refresh", AccountID: "new-account"}
	if err := live.Write(unexpected); err != nil {
		t.Fatal(err)
	}

	err := live.ClearIfMatches(Credentials{AccessToken: "old", RefreshToken: "old-refresh", AccountID: "old-account"})
	if err == nil || !strings.Contains(err.Error(), "preserved the unexpected login") {
		t.Fatalf("ClearIfMatches() error = %v, want unexpected-login refusal", err)
	}
	written, readErr := live.Read()
	if readErr != nil || written.AccessToken != unexpected.AccessToken || written.RefreshToken != unexpected.RefreshToken || written.AccountID != unexpected.AccountID {
		t.Fatalf("live credentials = %+v, %v; want unexpected credentials preserved", written, readErr)
	}
}

func TestLiveFileClearIfMatchesRemovesExpectedCredentials(t *testing.T) {
	directory := t.TempDir()
	live := LiveFile{Path: filepath.Join(directory, "auth.json")}
	expected := Credentials{AccessToken: "access", RefreshToken: "refresh", AccountID: "account"}
	if err := live.Write(expected); err != nil {
		t.Fatal(err)
	}

	if err := live.ClearIfMatches(expected); err != nil {
		t.Fatalf("ClearIfMatches() error = %v", err)
	}
	if _, err := os.Stat(live.Path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("live credentials still exist: %v", err)
	}
}
