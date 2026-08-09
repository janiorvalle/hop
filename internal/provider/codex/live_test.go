package codex

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestLiveUsageReadOnly(t *testing.T) {
	if os.Getenv("HOP_CODEX_LIVE_TEST") != "1" {
		t.Skip("set HOP_CODEX_LIVE_TEST=1 to run one read-only usage request")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	credentials, err := ReadLiveCredentials()
	if err != nil {
		t.Fatalf("ReadLiveCredentials() error = %v", err)
	}
	usage, err := New(Config{}).FetchUsage(ctx, credentials)
	if err != nil {
		t.Fatalf("FetchUsage() error = %v", err)
	}
	receipt, err := json.Marshal(usage)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	t.Logf("read-only normalized usage receipt: %s", receipt)
}

func TestLiveRefreshIsolatedSpare(t *testing.T) {
	path := os.Getenv("HOP_CODEX_SPARE_AUTH")
	if path == "" {
		t.Skip("set HOP_CODEX_SPARE_AUTH to an isolated spare account auth.json")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("UserHomeDir() error = %v", err)
	}
	livePath := filepath.Join(home, ".codex", "auth.json")
	if err := validateIsolatedAuthPath(path, livePath); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	refreshed, err := New(Config{}).Refresh(ctx, FileStore{Path: path})
	if err != nil {
		t.Fatalf("Refresh() error = %v", err)
	}
	t.Logf("isolated refresh receipt: access_token_bytes=%d refresh_token_bytes=%d last_refresh=%s", len(refreshed.AccessToken), len(refreshed.RefreshToken), refreshed.LastRefresh)
}

func TestValidateIsolatedAuthPathRejectsSymlinkToLiveFile(t *testing.T) {
	t.Parallel()

	directory := t.TempDir()
	livePath := filepath.Join(directory, "live-auth.json")
	if err := os.WriteFile(livePath, []byte("live"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	sparePath := filepath.Join(directory, "spare-auth.json")
	if err := os.Symlink(livePath, sparePath); err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("symlink privilege is unavailable: %v", err)
		}
		t.Fatalf("Symlink() error = %v", err)
	}
	if err := validateIsolatedAuthPath(sparePath, livePath); err == nil {
		t.Fatal("validateIsolatedAuthPath() error = nil, want same-file rejection")
	}
}

func TestValidateIsolatedAuthPathAllowsMissingLiveFile(t *testing.T) {
	t.Parallel()

	directory := t.TempDir()
	sparePath := filepath.Join(directory, "spare-auth.json")
	if err := os.WriteFile(sparePath, []byte("spare"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	if err := validateIsolatedAuthPath(sparePath, filepath.Join(directory, "missing-live.json")); err != nil {
		t.Fatalf("validateIsolatedAuthPath() error = %v, want safe isolation", err)
	}
}

func validateIsolatedAuthPath(sparePath, livePath string) error {
	spareInfo, err := os.Stat(sparePath)
	if err != nil {
		return fmt.Errorf("inspect isolated Codex auth file %s: %w", sparePath, err)
	}
	liveInfo, err := os.Stat(livePath)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect live Codex auth file without changing it: %w", err)
	}
	if os.SameFile(spareInfo, liveInfo) {
		return fmt.Errorf("HOP_CODEX_SPARE_AUTH resolves to live ~/.codex/auth.json; use CODEX_HOME=<tempdir> codex login")
	}
	return nil
}
