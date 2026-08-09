//go:build darwin

package claude

import (
	"context"
	"encoding/json"
	"errors"
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

// TestKeychainRoundTripThroughSecurityInteractiveMode proves the two facts the
// fakes elsewhere have to assume: security(1)'s interactive tokenizer returns
// the credential JSON byte for byte, and a missing item exits with the status
// clearLiveCredentials treats as already cleared.
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

	credentials := Credentials{
		AccessToken:  `sk-ant-oat01-A"quote\slash/plus+equals=`,
		RefreshToken: "sk-ant-ort01-with spaces and `backticks` $HOME",
		ExpiresAt:    1234567890123,
	}
	contents, err := json.Marshal(credentialEnvelope{OAuth: credentials})
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	writeCommand, err := keychainWriteCommand(roundTripService, "hop-round-trip", "/usr/bin/security", string(contents))
	if err != nil {
		t.Fatalf("keychainWriteCommand() error = %v", err)
	}
	if output, err := runSecurity(ctx, t, withKeychain(writeCommand, keychainPath)); err != nil {
		t.Fatalf("store the round-trip item: %v: %s", err, output)
	}

	readBack, err := exec.CommandContext(ctx, "security", "find-generic-password", "-s", roundTripService, "-w", keychainPath).Output()
	if err != nil {
		t.Fatalf("read the round-trip item back: %v", err)
	}
	if stored := strings.TrimSpace(string(readBack)); stored != string(contents) {
		t.Fatalf("stored credential =\n%s\nwant\n%s", stored, contents)
	}
	restored, err := parseCredentials(readBack)
	if err != nil || restored.AccessToken != credentials.AccessToken || restored.RefreshToken != credentials.RefreshToken {
		t.Fatalf("restored credentials = %+v, error = %v; want %+v", restored, err, credentials)
	}

	deleteCommand, err := keychainDeleteCommand(roundTripService)
	if err != nil {
		t.Fatalf("keychainDeleteCommand() error = %v", err)
	}
	if output, err := runSecurity(ctx, t, withKeychain(deleteCommand, keychainPath)); err != nil {
		t.Fatalf("delete the round-trip item: %v: %s", err, output)
	}
	_, err = runSecurity(ctx, t, withKeychain(deleteCommand, keychainPath))
	if securityExitCode(err) != securityItemNotFound {
		t.Fatalf("deleting a missing item exited with %d, want %d; clearLiveCredentials reads that status as already cleared", securityExitCode(err), securityItemNotFound)
	}
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

func withKeychain(command, keychainPath string) string {
	return strings.TrimSuffix(command, "\n") + " " + quoteSecurityArgument(keychainPath) + "\n"
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
