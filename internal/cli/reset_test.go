package cli

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/janiorvalle/hop/internal/provider/codex"
	"github.com/janiorvalle/hop/internal/state"
	"github.com/janiorvalle/hop/internal/vault"
)

var uuidV4 = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

// fakeCodexResetServer serves usage with the current credit count and spends a
// credit per accepted consume call, logging every redeem id it receives.
type fakeCodexResetServer struct {
	mu                    sync.Mutex
	credits               int
	consumeFailures       int
	usageDown             bool
	usageDownAfterConsume bool
	redeemIDs             []string
	bearers               []string
	server                *httptest.Server
}

func newFakeCodexResetServer(t *testing.T, credits int) *fakeCodexResetServer {
	t.Helper()
	fake := &fakeCodexResetServer{credits: credits}
	fake.server = httptest.NewServer(http.HandlerFunc(fake.handle))
	t.Cleanup(fake.server.Close)
	return fake
}

func (fake *fakeCodexResetServer) handle(writer http.ResponseWriter, request *http.Request) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	switch {
	case request.URL.Path == "/token":
		_, _ = io.WriteString(writer, `{"access_token":"fresh","refresh_token":"rotated"}`)
	case request.URL.Path == "/credits/consume" && request.Method == http.MethodPost:
		var body struct {
			RedeemRequestID string `json:"redeem_request_id"`
		}
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			http.Error(writer, err.Error(), http.StatusBadRequest)
			return
		}
		fake.redeemIDs = append(fake.redeemIDs, body.RedeemRequestID)
		fake.bearers = append(fake.bearers, request.Header.Get("Authorization"))
		if fake.consumeFailures > 0 {
			fake.consumeFailures--
			writer.WriteHeader(http.StatusInternalServerError)
			return
		}
		fake.credits--
		_, _ = io.WriteString(writer, `{}`)
	case request.URL.Path == "/usage" && (fake.usageDown || fake.usageDownAfterConsume && len(fake.redeemIDs) > 0):
		writer.WriteHeader(http.StatusBadGateway)
	case request.URL.Path == "/usage":
		credits := make([]string, 0, fake.credits)
		for index := range fake.credits {
			credits = append(credits, fmt.Sprintf(`{"id":"credit-%d","status":"available","reset_type":"codex_rate_limits","granted_at":"2026-08-01T00:00:00Z","expires_at":"2026-09-%02dT00:00:00Z"}`, index, 20+index))
		}
		_, _ = fmt.Fprintf(writer, `{
			"plan_type": "pro",
			"rate_limit": {
				"primary_window": {"used_percent": 90, "limit_window_seconds": 18000, "reset_after_seconds": 7200},
				"secondary_window": {"used_percent": 80, "limit_window_seconds": 604800, "reset_after_seconds": 180000}
			},
			"rate_limit_reset_credits": {"available_count": %d, "credits": [%s]}
		}`, fake.credits, strings.Join(credits, ","))
	default:
		http.NotFound(writer, request)
	}
}

func (fake *fakeCodexResetServer) snapshot() (redeemIDs, bearers []string, credits int) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	return append([]string(nil), fake.redeemIDs...), append([]string(nil), fake.bearers...), fake.credits
}

type resetFixture struct {
	vault    vault.Vault
	server   *fakeCodexResetServer
	slotPath string
	stdout   bytes.Buffer
	stderr   bytes.Buffer
	live     *fakeCodexLiveStore
}

func newResetFixture(t *testing.T, credits int) *resetFixture {
	t.Helper()
	fixture := &resetFixture{vault: newTestVault(t), server: newFakeCodexResetServer(t, credits), live: &fakeCodexLiveStore{readErr: os.ErrNotExist}}
	credentialsPath, err := fixture.vault.CredentialsPath("codex", "work")
	if err != nil {
		t.Fatalf("CredentialsPath() error = %v", err)
	}
	fixture.slotPath = filepath.Dir(credentialsPath)
	if err := (codex.FileStore{Path: credentialsPath}).Write(codex.Credentials{AccessToken: "slot-access", RefreshToken: "slot-refresh", AccountID: "account"}); err != nil {
		t.Fatalf("seed codex slot: %v", err)
	}
	return fixture
}

func (fixture *resetFixture) resetter(t *testing.T, approval string, stdinIsTTY bool) codexResetter {
	t.Helper()
	fixedNow := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	return codexResetter{
		vault:      fixture.vault,
		adapter:    codex.New(codex.Config{UsageURL: fixture.server.server.URL + "/usage", ResetCreditsURL: fixture.server.server.URL + "/credits", TokenURL: fixture.server.server.URL + "/token", Now: func() time.Time { return fixedNow }}),
		live:       fixture.live,
		stdout:     &fixture.stdout,
		stderr:     &fixture.stderr,
		getenv:     func(string) string { return approval },
		stdinIsTTY: func(io.Reader) bool { return stdinIsTTY },
		now:        func() time.Time { return fixedNow },
	}
}

func (fixture *resetFixture) pendingID(t *testing.T) (string, bool) {
	t.Helper()
	pending, found, err := readPendingReset(filepath.Join(fixture.slotPath, pendingResetFilename))
	if err != nil {
		t.Fatalf("readPendingReset() error = %v", err)
	}
	return pending.RedeemRequestID, found
}

func TestResetCodexSpendsOneCreditAndPrintsTheRefreshedRow(t *testing.T) {
	t.Parallel()

	fixture := newResetFixture(t, 2)
	resetter := fixture.resetter(t, "approved", false)

	if err := resetter.Reset(context.Background(), "work", strings.NewReader("")); err != nil {
		t.Fatalf("Reset() error = %v", err)
	}
	redeemIDs, bearers, credits := fixture.server.snapshot()
	if len(redeemIDs) != 1 || !uuidV4.MatchString(redeemIDs[0]) {
		t.Fatalf("redeem ids = %q, want one uuid v4", redeemIDs)
	}
	if bearers[0] != "Bearer slot-access" {
		t.Fatalf("Authorization = %q, want the slot token", bearers[0])
	}
	if credits != 1 {
		t.Fatalf("server credits = %d, want 1 after one reset", credits)
	}
	if _, found := fixture.pendingID(t); found {
		t.Fatalf("pending reset record survived a successful reset")
	}
	got := fixture.stdout.String()
	for _, want := range []string{`Spent 1 manual reset on codex account "work".`, "CODEX", "work", "1 reset"} {
		if !strings.Contains(got, want) {
			t.Fatalf("stdout = %q, want it to contain %q", got, want)
		}
	}
	if fixture.stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want no prompt when approved by environment", fixture.stderr.String())
	}
}

func TestResetCodexRetriesWithTheSameRedeemID(t *testing.T) {
	t.Parallel()

	fixture := newResetFixture(t, 1)
	fixture.server.consumeFailures = 1
	resetter := fixture.resetter(t, "approved", false)

	err := resetter.Reset(context.Background(), "work", strings.NewReader(""))
	if err == nil || !strings.Contains(err.Error(), "[CODEX_RESET_FAILED]") || !strings.Contains(err.Error(), "same request id") {
		t.Fatalf("first Reset() error = %v, want the retry guidance", err)
	}
	recordedID, found := fixture.pendingID(t)
	if !found || !uuidV4.MatchString(recordedID) {
		t.Fatalf("pending id = %q, %t; want the redeem id kept after a failed POST", recordedID, found)
	}
	if _, _, credits := fixture.server.snapshot(); credits != 1 {
		t.Fatalf("server credits = %d after a failed POST, want 1", credits)
	}

	if err := resetter.Reset(context.Background(), "work", strings.NewReader("")); err != nil {
		t.Fatalf("second Reset() error = %v", err)
	}
	redeemIDs, _, credits := fixture.server.snapshot()
	if len(redeemIDs) != 2 || redeemIDs[0] != recordedID || redeemIDs[1] != recordedID {
		t.Fatalf("redeem ids = %q, want %q sent twice", redeemIDs, recordedID)
	}
	if credits != 0 {
		t.Fatalf("server credits = %d, want 0 after the retry landed", credits)
	}
	if _, found := fixture.pendingID(t); found {
		t.Fatalf("pending reset record survived the successful retry")
	}
}

func TestResetCodexSucceedsWhenOnlyTheRefreshedGlanceFails(t *testing.T) {
	t.Parallel()

	fixture := newResetFixture(t, 2)
	fixture.server.usageDownAfterConsume = true
	resetter := fixture.resetter(t, "approved", false)

	if err := resetter.Reset(context.Background(), "work", strings.NewReader("")); err != nil {
		t.Fatalf("Reset() error = %v, want success because the credit was spent", err)
	}
	if got := fixture.stdout.String(); !strings.Contains(got, `Spent 1 manual reset on codex account "work".`) {
		t.Fatalf("stdout = %q, want the receipt", got)
	}
	if got := fixture.stderr.String(); !strings.Contains(got, "refreshed usage") || !strings.Contains(got, "hop ls") {
		t.Fatalf("stderr = %q, want the glance note with the next step", got)
	}
	if _, found := fixture.pendingID(t); found {
		t.Fatalf("pending reset record survived a spent credit; a retry would mint a second id")
	}
}

func TestResetCodexRefusesWithoutCredits(t *testing.T) {
	t.Parallel()

	fixture := newResetFixture(t, 0)
	resetter := fixture.resetter(t, "approved", false)

	err := resetter.Reset(context.Background(), "work", strings.NewReader(""))
	for _, want := range []string{"[CODEX_RESET_NO_CREDITS]", "0 manual resets", "resets on its own in 2d02h", "hop ls"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("Reset() error = %v, want it to contain %q", err, want)
		}
	}
	if redeemIDs, _, _ := fixture.server.snapshot(); len(redeemIDs) != 0 {
		t.Fatalf("consume calls = %q, want none without credits", redeemIDs)
	}
	if _, found := fixture.pendingID(t); found {
		t.Fatalf("pending reset record written although no request was sent")
	}
}

func TestResetCodexNeedsATerminalWithoutApproval(t *testing.T) {
	t.Parallel()

	fixture := newResetFixture(t, 2)
	resetter := fixture.resetter(t, "", false)

	err := resetter.Reset(context.Background(), "work", strings.NewReader(""))
	for _, want := range []string{"[CODEX_RESET_NEEDS_CONFIRMATION]", "HOP_CODEX_RESET=approved", "nothing was sent"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("Reset() error = %v, want it to contain %q", err, want)
		}
	}
	if redeemIDs, _, _ := fixture.server.snapshot(); len(redeemIDs) != 0 {
		t.Fatalf("consume calls = %q, want none without confirmation", redeemIDs)
	}
	if _, found := fixture.pendingID(t); found {
		t.Fatalf("pending reset record written although the reset was refused")
	}
}

func TestResetCodexAsksOnATerminal(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name     string
		answer   string
		wantErr  string
		wantPost int
	}{
		{name: "yes spends the credit", answer: "y\n", wantPost: 1},
		{name: "no keeps it", answer: "n\n", wantErr: "[CODEX_RESET_DECLINED]"},
		{name: "enter keeps it", answer: "\n", wantErr: "[CODEX_RESET_DECLINED]"},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			fixture := newResetFixture(t, 2)
			resetter := fixture.resetter(t, "", true)

			err := resetter.Reset(context.Background(), "work", strings.NewReader(testCase.answer))
			if testCase.wantErr == "" && err != nil {
				t.Fatalf("Reset() error = %v", err)
			}
			if testCase.wantErr != "" && (err == nil || !strings.Contains(err.Error(), testCase.wantErr)) {
				t.Fatalf("Reset() error = %v, want %q", err, testCase.wantErr)
			}
			if got := fixture.stderr.String(); got != `This spends 1 of 2 manual resets on codex account "work". Continue? [y/N] ` {
				t.Fatalf("prompt = %q", got)
			}
			if redeemIDs, _, _ := fixture.server.snapshot(); len(redeemIDs) != testCase.wantPost {
				t.Fatalf("consume calls = %q, want %d", redeemIDs, testCase.wantPost)
			}
		})
	}
}

func TestResetCodexRetryPromptNamesTheRecordedRequest(t *testing.T) {
	t.Parallel()

	fixture := newResetFixture(t, 1)
	if err := writePendingReset(filepath.Join(fixture.slotPath, pendingResetFilename), pendingReset{RedeemRequestID: "recorded-id", AccountID: "account"}); err != nil {
		t.Fatalf("writePendingReset() error = %v", err)
	}
	resetter := fixture.resetter(t, "", true)

	if err := resetter.Reset(context.Background(), "work", strings.NewReader("y\n")); err != nil {
		t.Fatalf("Reset() error = %v", err)
	}
	if got := fixture.stderr.String(); !strings.Contains(got, "Retrying the reset request an earlier run recorded") {
		t.Fatalf("prompt = %q, want the retry wording", got)
	}
	if redeemIDs, _, _ := fixture.server.snapshot(); len(redeemIDs) != 1 || redeemIDs[0] != "recorded-id" {
		t.Fatalf("redeem ids = %q, want the recorded id reused", redeemIDs)
	}
}

func TestResetCodexRetriesARecordedRequestWhileUsageIsDown(t *testing.T) {
	t.Parallel()

	fixture := newResetFixture(t, 1)
	fixture.server.usageDown = true
	if err := writePendingReset(filepath.Join(fixture.slotPath, pendingResetFilename), pendingReset{RedeemRequestID: "recorded-id", AccountID: "account"}); err != nil {
		t.Fatalf("writePendingReset() error = %v", err)
	}
	resetter := fixture.resetter(t, "approved", false)

	if err := resetter.Reset(context.Background(), "work", strings.NewReader("")); err != nil {
		t.Fatalf("Reset() error = %v, want the recorded request retried without a usage preflight", err)
	}
	if redeemIDs, _, _ := fixture.server.snapshot(); len(redeemIDs) != 1 || redeemIDs[0] != "recorded-id" {
		t.Fatalf("redeem ids = %q, want the recorded id sent once", redeemIDs)
	}
	if _, found := fixture.pendingID(t); found {
		t.Fatalf("pending reset record survived the successful retry")
	}
	if !strings.Contains(fixture.stderr.String(), "hop ls") {
		t.Fatalf("stderr = %q, want the glance note since usage is down", fixture.stderr.String())
	}
}

func TestResetCodexCancelAtThePromptReleasesTheSlotLock(t *testing.T) {
	t.Parallel()

	fixture := newResetFixture(t, 2)
	resetter := fixture.resetter(t, "", true)
	blockedStdin, _ := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		for fixture.stderr.Len() == 0 {
			time.Sleep(5 * time.Millisecond)
		}
		cancel()
	}()

	err := resetter.Reset(ctx, "work", blockedStdin)
	if err == nil || !strings.Contains(err.Error(), "[CODEX_RESET_CANCELED]") {
		t.Fatalf("Reset() error = %v, want the cancel refusal", err)
	}
	if _, err := os.Stat(filepath.Join(fixture.slotPath, refreshLockFilename)); !os.IsNotExist(err) {
		t.Fatalf("refresh lock still held after cancel: %v", err)
	}
	if redeemIDs, _, _ := fixture.server.snapshot(); len(redeemIDs) != 0 {
		t.Fatalf("consume calls = %q, want none after cancel", redeemIDs)
	}
}

func TestResetCodexRefusesARecordedRequestFromAnotherIdentity(t *testing.T) {
	t.Parallel()

	fixture := newResetFixture(t, 1)
	pendingPath := filepath.Join(fixture.slotPath, pendingResetFilename)
	if err := writePendingReset(pendingPath, pendingReset{RedeemRequestID: "recorded-id", AccountID: "previous-identity"}); err != nil {
		t.Fatalf("writePendingReset() error = %v", err)
	}
	resetter := fixture.resetter(t, "approved", false)

	err := resetter.Reset(context.Background(), "work", strings.NewReader(""))
	for _, want := range []string{"[CODEX_RESET_STALE_REQUEST]", pendingPath, "nothing was sent"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("Reset() error = %v, want it to contain %q", err, want)
		}
	}
	if redeemIDs, _, _ := fixture.server.snapshot(); len(redeemIDs) != 0 {
		t.Fatalf("consume calls = %q, want none for a stale record", redeemIDs)
	}
}

func TestResetCodexUsesLiveCredentialsForTheActiveAccount(t *testing.T) {
	t.Parallel()

	fixture := newResetFixture(t, 2)
	activeState := state.New()
	activeState.SetActive("codex", "work")
	if err := activeState.Save(fixture.vault.Root()); err != nil {
		t.Fatalf("state.Save() error = %v", err)
	}
	fixture.live = &fakeCodexLiveStore{credentials: codex.Credentials{AccessToken: "live-access", RefreshToken: "live-refresh", AccountID: "account"}}
	resetter := fixture.resetter(t, "approved", false)

	if err := resetter.Reset(context.Background(), "work", strings.NewReader("")); err != nil {
		t.Fatalf("Reset() error = %v", err)
	}
	if _, bearers, _ := fixture.server.snapshot(); len(bearers) != 1 || bearers[0] != "Bearer live-access" {
		t.Fatalf("Authorization = %q, want the live token for the active account", bearers)
	}
	if !strings.Contains(fixture.stdout.String(), "> ~ work") {
		t.Fatalf("stdout = %q, want the active marker on the row", fixture.stdout.String())
	}
}

func TestResetCodexRefusesWhenTheLiveLoginIsAnotherIdentity(t *testing.T) {
	t.Parallel()

	fixture := newResetFixture(t, 2)
	activeState := state.New()
	activeState.SetActive("codex", "work")
	if err := activeState.Save(fixture.vault.Root()); err != nil {
		t.Fatalf("state.Save() error = %v", err)
	}
	fixture.live = &fakeCodexLiveStore{credentials: codex.Credentials{AccessToken: "other-access", RefreshToken: "other-refresh", AccountID: "someone-else"}}
	resetter := fixture.resetter(t, "approved", false)

	err := resetter.Reset(context.Background(), "work", strings.NewReader(""))
	for _, want := range []string{"[CODEX_RESET_WRONG_LOGIN]", "no credit was spent", "hop codex work"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("Reset() error = %v, want it to contain %q", err, want)
		}
	}
	if redeemIDs, _, _ := fixture.server.snapshot(); len(redeemIDs) != 0 {
		t.Fatalf("consume calls = %q, want none for a foreign live login", redeemIDs)
	}
}

func TestResetCodexRefreshesAnExpiredManagedSlotFirst(t *testing.T) {
	t.Parallel()

	fixture := newResetFixture(t, 2)
	expiredPayload := base64.RawURLEncoding.EncodeToString([]byte(`{"exp":1}`))
	credentialsPath := filepath.Join(fixture.slotPath, vault.CredentialsFilename)
	if err := (codex.FileStore{Path: credentialsPath}).Write(codex.Credentials{AccessToken: "header." + expiredPayload + ".signature", RefreshToken: "refresh", AccountID: "account"}); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	if err := writeSlotMetadata(fixture.slotPath, slotMetadata{RefreshPolicy: managedRefreshPolicy}); err != nil {
		t.Fatalf("writeSlotMetadata() error = %v", err)
	}
	resetter := fixture.resetter(t, "approved", false)

	if err := resetter.Reset(context.Background(), "work", strings.NewReader("")); err != nil {
		t.Fatalf("Reset() error = %v", err)
	}
	if _, bearers, _ := fixture.server.snapshot(); len(bearers) != 1 || bearers[0] != "Bearer fresh" {
		t.Fatalf("Authorization = %q, want the refreshed token", bearers)
	}
	rotated, err := (codex.FileStore{Path: credentialsPath}).Read()
	if err != nil || rotated.RefreshToken != "rotated" {
		t.Fatalf("slot credentials = %+v, %v; want the rotated refresh token saved", rotated, err)
	}
}

func TestResetCodexRefusesAnExpiredUnmanagedSlot(t *testing.T) {
	t.Parallel()

	fixture := newResetFixture(t, 2)
	expiredPayload := base64.RawURLEncoding.EncodeToString([]byte(`{"exp":1}`))
	if err := (codex.FileStore{Path: filepath.Join(fixture.slotPath, vault.CredentialsFilename)}).Write(codex.Credentials{AccessToken: "header." + expiredPayload + ".signature", RefreshToken: "refresh", AccountID: "account"}); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	resetter := fixture.resetter(t, "approved", false)

	err := resetter.Reset(context.Background(), "work", strings.NewReader(""))
	if err == nil || !strings.Contains(err.Error(), "hop login codex work") {
		t.Fatalf("Reset() error = %v, want the login next step", err)
	}
	if redeemIDs, _, _ := fixture.server.snapshot(); len(redeemIDs) != 0 {
		t.Fatalf("consume calls = %q, want none with expired credentials", redeemIDs)
	}
}

func TestResetCodexExplainsMissingSlot(t *testing.T) {
	t.Parallel()

	fixture := newResetFixture(t, 2)
	resetter := fixture.resetter(t, "approved", false)

	err := resetter.Reset(context.Background(), "missing", strings.NewReader(""))
	if err == nil || !strings.Contains(err.Error(), "run 'hop ls'") {
		t.Fatalf("Reset() error = %v, want the account-list next step", err)
	}
}

func TestResetCodexRefusesSlotBeingEnrolled(t *testing.T) {
	t.Parallel()

	fixture := newResetFixture(t, 2)
	reservation, err := loginManager{vault: fixture.vault}.reserveNewSlot("codex", "fresh")
	if err != nil {
		t.Fatalf("reserveNewSlot() error = %v", err)
	}
	defer reservation.Cleanup()
	resetter := fixture.resetter(t, "approved", false)

	err = resetter.Reset(context.Background(), "fresh", strings.NewReader(""))
	if err == nil || !strings.Contains(err.Error(), "being enrolled") {
		t.Fatalf("Reset() error = %v, want enrollment-in-progress guidance", err)
	}
}

func TestReadPendingResetExplainsACorruptRecord(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), pendingResetFilename)
	if err := os.WriteFile(path, []byte("not json"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	_, _, err := readPendingReset(path)
	if err == nil || !strings.Contains(err.Error(), "delete the file") {
		t.Fatalf("readPendingReset() error = %v, want repair guidance", err)
	}
}

func TestRequireCodexResetValidatesArguments(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name string
		args []string
		want string
	}{
		{name: "claude is refused", args: []string{"claude", "work"}, want: "[RESET_UNSUPPORTED_PROVIDER]"},
		{name: "missing account", args: []string{"codex"}, want: "try 'hop reset codex work'"},
		{name: "unknown provider", args: []string{"gemini", "work"}, want: "try 'hop reset codex work'"},
		{name: "blank account", args: []string{"codex", " "}, want: "account cannot be empty"},
		{name: "codex account", args: []string{"codex", "work"}},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			err := requireCodexReset(testCase.args)
			if testCase.want == "" {
				if err != nil {
					t.Fatalf("requireCodexReset(%q) error = %v", testCase.args, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("requireCodexReset(%q) error = %v, want %q", testCase.args, err, testCase.want)
			}
		})
	}
}

func TestResetCodexViaExecuteRefusesClaudeWithoutTouchingTheVault(t *testing.T) {
	t.Parallel()

	var stdout, stderr bytes.Buffer
	err := execute([]string{"reset", "claude", "work"}, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "[RESET_UNSUPPORTED_PROVIDER]") {
		t.Fatalf("execute() error = %v, want the provider refusal", err)
	}
}
