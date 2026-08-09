package claude

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestFileStoreRoundTripUsesClaudeCredentialShape(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "claude", "work", "credentials.json")
	store := FileStore{Path: path}
	want := Credentials{
		AccessToken:           "access",
		RefreshToken:          "refresh",
		ExpiresAt:             1000,
		RefreshTokenExpiresAt: 2000,
		SubscriptionType:      "pro",
		Scopes:                []string{"user:profile"},
	}
	if err := store.Write(want); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	got, err := store.Read()
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if got.AccessToken != want.AccessToken || got.RefreshToken != want.RefreshToken || got.SubscriptionType != want.SubscriptionType {
		t.Errorf("Read() = %+v, want credential fields preserved", got)
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

	store := FileStore{Path: filepath.Join(t.TempDir(), "claude", "work", "credentials.json")}
	old := Credentials{AccessToken: "old", RefreshToken: "old-refresh", ExpiresAt: 1}
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
	rotated := Credentials{AccessToken: "rotated", RefreshToken: "rotated-refresh", ExpiresAt: 2}
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
	newest := Credentials{AccessToken: "newest", RefreshToken: "newest-refresh", ExpiresAt: 3}
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

	store := FileStore{Path: filepath.Join(t.TempDir(), "credentials.json")}
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

func TestParseCredentialsRejectsMissingAccessToken(t *testing.T) {
	t.Parallel()

	_, err := parseCredentials([]byte(`{"claudeAiOauth":{"refreshToken":"refresh"}}`))
	if err == nil {
		t.Fatal("parseCredentials() error = nil, want missing access token error")
	}
}

func TestLiveFileWritePreservesParentDirectoryPermissions(t *testing.T) {
	t.Parallel()

	directory := filepath.Join(t.TempDir(), "claude-home")
	if err := os.Mkdir(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	live := LiveFile{Path: filepath.Join(directory, ".credentials.json")}
	if err := live.Write(Credentials{AccessToken: "access", RefreshToken: "refresh"}); err != nil {
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
