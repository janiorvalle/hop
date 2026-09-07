//go:build darwin

package claude

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The service and keychain used here are the test's own. Nothing in this file
// names the service the Claude CLI uses or touches the default keychain, so an
// opted-in run cannot disturb a real login.
const roundTripService = "hop-keychain-round-trip-test"

// TestKeychainRoundTripThroughSecurityInteractiveMode proves the fact the
// fakes elsewhere have to assume: security(1)'s interactive tokenizer returns
// the credential JSON byte for byte.
//
// It is opt-in because it runs the real security(1) against a keychain it
// creates. Set HOP_CLAUDE_KEYCHAIN_TEST=1 to run it.
func TestKeychainRoundTripThroughSecurityInteractiveMode(t *testing.T) {
	if os.Getenv("HOP_CLAUDE_KEYCHAIN_TEST") != "1" {
		t.Skip("set HOP_CLAUDE_KEYCHAIN_TEST=1 to round-trip a throwaway keychain through security(1)")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	keychainPath := createThrowawayKeychain(ctx, t)

	credentials := roundTripCredentials()
	contents := roundTripContents(t, credentials)
	arguments := append(keychainWriteArguments(roundTripItem(contents)), keychainPath)
	if output, err := runSecurity(ctx, t, interactiveSecurityLine(arguments)+"\n"); err != nil {
		t.Fatalf("store the round-trip item: %v: %s", err, output)
	}
	assertKeychainHolds(ctx, t, keychainPath, credentials, contents)
}

// TestKeychainRoundTripOfAnItemPastTheInteractiveLine proves the other write
// path the fakes assume: an item with more MCP logins than fit on security(1)'s
// interactive line is stored whole when passed as an argument. Same opt-in.
func TestKeychainRoundTripOfAnItemPastTheInteractiveLine(t *testing.T) {
	if os.Getenv("HOP_CLAUDE_KEYCHAIN_TEST") != "1" {
		t.Skip("set HOP_CLAUDE_KEYCHAIN_TEST=1 to round-trip a throwaway keychain through security(1)")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	keychainPath := createThrowawayKeychain(ctx, t)

	credentials := roundTripCredentials()
	credentials.MCPTokens = MCPTokens{}
	for _, server := range []string{"linear-server", "streamlyne-research", "plugin:posthog:posthog", "plugin:atlassian:atlassian", "plugin:datadog:mcp"} {
		credentials.MCPTokens[server+"|Y2FsbGJhY2s="] = json.RawMessage(fmt.Sprintf(`{"serverName":%q,"accessToken":%q,"refreshToken":%q,"expiresAt":1700000000000}`, server, strings.Repeat("a", 900), strings.Repeat("r", 300)))
	}
	contents := roundTripContents(t, credentials)
	if len(contents) <= securityCommandLimit {
		t.Fatalf("item is %d bytes, want one past the %d-character interactive line", len(contents), securityCommandLimit)
	}
	arguments := append(keychainWriteArguments(roundTripItem(contents)), keychainPath)
	if output, err := (systemSecurity{}).Run(ctx, "", arguments...); err != nil {
		t.Fatalf("store the oversized round-trip item: %v: %s", err, output)
	}
	restored := assertKeychainHolds(ctx, t, keychainPath, credentials, contents)
	if len(restored.MCPTokens) != len(credentials.MCPTokens) {
		t.Fatalf("restored MCP logins = %d, want %d", len(restored.MCPTokens), len(credentials.MCPTokens))
	}
}

func roundTripCredentials() Credentials {
	return Credentials{
		AccessToken:  `sk-ant-oat01-A"quote\slash/plus+equals=`,
		RefreshToken: "sk-ant-ort01-with spaces and `backticks` $HOME",
		ExpiresAt:    1234567890123,
	}
}

func roundTripContents(t *testing.T, credentials Credentials) string {
	t.Helper()
	contents, err := json.Marshal(credentialItem(credentials))
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	return string(contents)
}

func roundTripItem(contents string) keychainItem {
	return keychainItem{service: roundTripService, account: "hop-round-trip", trustedApplication: "/usr/bin/security", contents: contents}
}

func assertKeychainHolds(ctx context.Context, t *testing.T, keychainPath string, credentials Credentials, contents string) Credentials {
	t.Helper()
	readBack, err := exec.CommandContext(ctx, "security", "find-generic-password", "-s", roundTripService, "-w", keychainPath).Output()
	if err != nil {
		t.Fatalf("read the round-trip item back: %v", err)
	}
	if stored := strings.TrimSpace(string(readBack)); stored != contents {
		t.Fatalf("stored credential =\n%s\nwant\n%s", stored, contents)
	}
	restored, err := parseCredentials(readBack)
	if err != nil || restored.AccessToken != credentials.AccessToken || restored.RefreshToken != credentials.RefreshToken {
		t.Fatalf("restored credentials = %+v, error = %v; want %+v", restored, err, credentials)
	}
	return restored
}

func createThrowawayKeychain(ctx context.Context, t *testing.T) string {
	t.Helper()
	// security(1) falls back to the default keychain when the keychain it is
	// handed cannot be opened, so a failure to create this one has to stop the
	// test rather than let later commands reach a real login.
	keychainPath := filepath.Join(t.TempDir(), "hop-round-trip.keychain")
	if output, err := exec.CommandContext(ctx, "security", "create-keychain", "-p", "hop-round-trip", keychainPath).CombinedOutput(); err != nil {
		t.Fatalf("create the throwaway keychain %s: %v: %s", keychainPath, err, output)
	}
	t.Cleanup(func() {
		cleanupContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		if output, err := exec.CommandContext(cleanupContext, "security", "delete-keychain", keychainPath).CombinedOutput(); err != nil {
			t.Errorf("delete the throwaway keychain %s: %v: %s", keychainPath, err, output)
		}
	})
	return keychainPath
}

func runSecurity(ctx context.Context, t *testing.T, command string) ([]byte, error) {
	t.Helper()
	output, err := systemSecurity{}.Run(ctx, command, "-i")
	var exit *exec.ExitError
	if err != nil && !errors.As(err, &exit) {
		t.Fatalf("run security -i: %v", err)
	}
	return output, err
}
