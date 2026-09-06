package cli

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/janiorvalle/hop/internal/provider/claude"
	"github.com/janiorvalle/hop/internal/provider/codex"
	"github.com/janiorvalle/hop/internal/state"
	"github.com/janiorvalle/hop/internal/vault"
)

type requestLog struct {
	mu       sync.Mutex
	requests []string
}

func (log *requestLog) record(request *http.Request) {
	log.mu.Lock()
	defer log.mu.Unlock()
	log.requests = append(log.requests, request.Method+" "+request.URL.Path)
}

func (log *requestLog) entries() []string {
	log.mu.Lock()
	defer log.mu.Unlock()
	return append([]string(nil), log.requests...)
}

func TestRefreshRotatesDueSlotsAndReportsEveryAccount(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.September, 5, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	tokenLog := &requestLog{}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		tokenLog.record(request)
		_, _ = writer.Write([]byte(`{"access_token":"fresh","refresh_token":"rotated","expires_in":3600,"refresh_token_expires_in":2592000}`))
	}))
	t.Cleanup(server.Close)

	accountVault, err := vault.New(filepath.Join(t.TempDir(), "hop-home"))
	if err != nil {
		t.Fatalf("vault.New() error = %v", err)
	}
	seedClaudeSlot(t, accountVault, "due", claude.Credentials{AccessToken: "valid", RefreshToken: "refresh", ExpiresAt: now.Add(time.Hour).UnixMilli(), RefreshTokenExpiresAt: now.Add(6 * 24 * time.Hour).UnixMilli()}, managedRefreshPolicy)
	seedClaudeSlot(t, accountVault, "fresh", claude.Credentials{AccessToken: "valid", RefreshToken: "refresh", ExpiresAt: now.Add(time.Hour).UnixMilli(), RefreshTokenExpiresAt: now.Add(30 * 24 * time.Hour).UnixMilli()}, managedRefreshPolicy)
	seedClaudeSlot(t, accountVault, "seeded", claude.Credentials{AccessToken: "valid", RefreshToken: "refresh", ExpiresAt: now.Add(-time.Hour).UnixMilli()}, "")
	seedClaudeSlot(t, accountVault, "broken", claude.Credentials{AccessToken: "stale", ExpiresAt: now.Add(-time.Hour).UnixMilli()}, managedRefreshPolicy)
	if _, err := accountVault.EnsureSlot("codex", "live"); err != nil {
		t.Fatalf("EnsureSlot(codex, live) error = %v", err)
	}
	freshBefore := readSlotFile(t, accountVault, "claude", "fresh")
	activeState := state.New()
	activeState.SetActive("codex", "live")
	accountCatalog := vaultCatalog{
		vault:         accountVault,
		state:         activeState,
		claudeAdapter: claude.New(claude.Config{TokenURL: server.URL + "/token", Now: clock}),
		codexAdapter:  codex.New(codex.Config{TokenURL: server.URL + "/token", Now: clock}),
		now:           clock,
	}

	var stdout bytes.Buffer
	err = refreshAccountsFrom(context.Background(), &stdout, accountCatalog)
	t.Logf("hop refresh output:\n%s", stdout.String())
	t.Logf("hop refresh error: %v", err)
	t.Logf("fake token server requests: %q", tokenLog.entries())

	if err == nil || !strings.HasPrefix(err.Error(), "[REFRESH_FAILED] 1 of 5 accounts could not be rotated.") {
		t.Fatalf("refreshAccountsFrom() error = %v, want one failure counted", err)
	}
	lines := strings.Split(strings.TrimSuffix(stdout.String(), "\n"), "\n")
	wantLines := []string{
		"claude broken: failed: refresh token is missing; run 'hop login claude broken': claude credentials are invalid",
		"claude due: rotated, refresh token good until 2026-10-05",
		"claude fresh: fresh, no rotation needed",
		"claude seeded: skipped: not managed by hop, run 'hop rm claude seeded' and then 'hop login claude seeded' to let hop rotate it",
		"codex live: skipped: active account, hop never rotates the live login",
	}
	if len(lines) != len(wantLines) {
		t.Fatalf("output lines = %d, want %d:\n%s", len(lines), len(wantLines), stdout.String())
	}
	for index, want := range wantLines {
		if lines[index] != want {
			t.Errorf("line %d = %q, want %q", index+1, lines[index], want)
		}
	}
	if got := tokenLog.entries(); len(got) != 1 || got[0] != "POST /token" {
		t.Fatalf("token server requests = %q, want exactly one POST /token for the due slot", got)
	}
	rotated, err := (claude.FileStore{Path: slotCredentialsPath(t, accountVault, "claude", "due")}).Read()
	if err != nil {
		t.Fatalf("Read(due) error = %v", err)
	}
	if rotated.RefreshToken != "rotated" || rotated.RefreshTokenExpiresAt != now.Add(30*24*time.Hour).UnixMilli() {
		t.Fatalf("due slot after refresh = %+v, want the rotated tokens and the new expiry", rotated)
	}
	if freshAfter := readSlotFile(t, accountVault, "claude", "fresh"); !bytes.Equal(freshBefore, freshAfter) {
		t.Fatalf("fresh slot changed on disk:\n%s", freshAfter)
	}
}

func TestRefreshReportsCodexExpiryFromLastRefresh(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.September, 5, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte(`{"access_token":"fresh","refresh_token":"rotated"}`))
	}))
	t.Cleanup(server.Close)
	accountVault, err := vault.New(filepath.Join(t.TempDir(), "hop-home"))
	if err != nil {
		t.Fatalf("vault.New() error = %v", err)
	}
	validPayload := base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"exp":%d}`, now.Add(time.Hour).Unix())))
	slotPath, err := accountVault.EnsureSlot("codex", "aging")
	if err != nil {
		t.Fatalf("EnsureSlot() error = %v", err)
	}
	store := codex.FileStore{Path: slotCredentialsPath(t, accountVault, "codex", "aging")}
	if err := store.Write(codex.Credentials{AccessToken: "header." + validPayload + ".signature", RefreshToken: "refresh", AccountID: "account", LastRefresh: now.Add(-9 * 24 * time.Hour).Format(time.RFC3339Nano)}); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	if err := writeSlotMetadata(slotPath, slotMetadata{RefreshPolicy: managedRefreshPolicy}); err != nil {
		t.Fatalf("writeSlotMetadata() error = %v", err)
	}
	accountCatalog := vaultCatalog{
		vault:         accountVault,
		state:         state.New(),
		claudeAdapter: claude.New(claude.Config{TokenURL: server.URL, Now: clock}),
		codexAdapter:  codex.New(codex.Config{TokenURL: server.URL, Now: clock}),
		now:           clock,
	}

	var stdout bytes.Buffer
	if err := refreshAccountsFrom(context.Background(), &stdout, accountCatalog); err != nil {
		t.Fatalf("refreshAccountsFrom() error = %v", err)
	}
	if got := stdout.String(); got != "codex aging: rotated, refresh token good until 2026-09-20\n" {
		t.Fatalf("output = %q, want the fifteen-day Codex expiry", got)
	}
}

func TestRefreshWithoutAccountsGivesEnrollmentStep(t *testing.T) {
	t.Parallel()

	var stdout bytes.Buffer
	if err := refreshAccountsFrom(context.Background(), &stdout, staticCatalog{}); err != nil {
		t.Fatalf("refreshAccountsFrom() error = %v", err)
	}
	if got := stdout.String(); got != "No accounts enrolled. Run 'hop login claude work' or 'hop login codex work'.\n" {
		t.Fatalf("output = %q", got)
	}
}

func TestRefreshRejectsExtraArguments(t *testing.T) {
	t.Parallel()

	var stderr bytes.Buffer
	exitCode := Run([]string{"refresh", "now"}, &bytes.Buffer{}, &stderr)

	if exitCode != 2 {
		t.Fatalf("Run() exit code = %d, want 2", exitCode)
	}
	if got := stderr.String(); !strings.Contains(got, "hop refresh") {
		t.Fatalf("stderr = %q, want the corrected invocation", got)
	}
}

func seedClaudeSlot(t *testing.T, accountVault vault.Vault, name string, credentials claude.Credentials, refreshPolicy string) {
	t.Helper()
	slotPath, err := accountVault.EnsureSlot("claude", name)
	if err != nil {
		t.Fatalf("EnsureSlot(claude, %s) error = %v", name, err)
	}
	if err := (claude.FileStore{Path: slotCredentialsPath(t, accountVault, "claude", name)}).Write(credentials); err != nil {
		t.Fatalf("Write(%s) error = %v", name, err)
	}
	if refreshPolicy == "" {
		return
	}
	if err := writeSlotMetadata(slotPath, slotMetadata{RefreshPolicy: refreshPolicy}); err != nil {
		t.Fatalf("writeSlotMetadata(%s) error = %v", name, err)
	}
}

func slotCredentialsPath(t *testing.T, accountVault vault.Vault, providerName, name string) string {
	t.Helper()
	path, err := accountVault.CredentialsPath(providerName, name)
	if err != nil {
		t.Fatalf("CredentialsPath(%s, %s) error = %v", providerName, name, err)
	}
	return path
}

func readSlotFile(t *testing.T, accountVault vault.Vault, providerName, name string) []byte {
	t.Helper()
	contents, err := os.ReadFile(slotCredentialsPath(t, accountVault, providerName, name))
	if err != nil {
		t.Fatalf("ReadFile(%s/%s) error = %v", providerName, name, err)
	}
	return contents
}
