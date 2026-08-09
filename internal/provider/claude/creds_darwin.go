//go:build darwin

package claude

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
)

const keychainService = "Claude Code-credentials"

// LiveCredentialsTarget names where live Claude credentials are stored, for
// switch-transaction fingerprints.
func LiveCredentialsTarget() (string, error) {
	return "keychain:" + keychainService, nil
}

// ReadLiveCredentials reads Claude Code's Keychain item without changing it.
func ReadLiveCredentials(ctx context.Context) (Credentials, error) {
	command := exec.CommandContext(ctx, "security", "find-generic-password", "-s", keychainService, "-w")
	contents, err := command.Output()
	if err != nil {
		return Credentials{}, fmt.Errorf("read the %q Keychain item; unlock Keychain or run 'claude /login': %w", keychainService, err)
	}
	return parseCredentials([]byte(strings.TrimSpace(string(contents))))
}

// WriteLiveCredentials replaces Claude Code's Keychain item.
func WriteLiveCredentials(ctx context.Context, credentials Credentials) error {
	contents, err := json.Marshal(credentialEnvelope{OAuth: credentials})
	if err != nil {
		return err
	}
	currentUser, err := user.Current()
	if err != nil {
		return fmt.Errorf("find the macOS account that owns the Claude Keychain item: %w", err)
	}
	claudePath, err := exec.LookPath("claude")
	if err != nil {
		return fmt.Errorf("find the Claude CLI before restoring its Keychain access; install Claude or add it to PATH, then retry: %w", err)
	}
	claudePath, err = filepath.EvalSymlinks(claudePath)
	if err != nil {
		return fmt.Errorf("resolve the Claude CLI at %s before restoring its Keychain access; fix the installation and retry: %w", claudePath, err)
	}
	command := exec.CommandContext(ctx, "security", "add-generic-password", "-U", "-a", currentUser.Username, "-s", keychainService, "-T", claudePath, "-w")
	command.Stdin = bytes.NewReader(append(contents, '\n'))
	if output, err := command.CombinedOutput(); err != nil {
		return fmt.Errorf("write the %q Keychain item; unlock Keychain and retry: %s: %w", keychainService, strings.TrimSpace(string(output)), err)
	}
	written, err := ReadLiveCredentials(ctx)
	if err != nil {
		return fmt.Errorf("verify the restored %q Keychain item; stop using Claude and retry restoration from the active hop slot: %w", keychainService, err)
	}
	if written.AccessToken != credentials.AccessToken || written.RefreshToken != credentials.RefreshToken {
		return fmt.Errorf("verify the restored %q Keychain item; stored credentials did not match, stop using Claude and retry restoration from the active hop slot", keychainService)
	}
	statusCommand := exec.CommandContext(ctx, claudePath, "auth", "status", "--json")
	statusOutput, err := statusCommand.Output()
	if err != nil {
		return fmt.Errorf("verify Claude can use the restored %q Keychain item; stop using Claude and retry restoration from the active hop slot: %w", keychainService, err)
	}
	var status struct {
		LoggedIn bool `json:"loggedIn"`
	}
	if err := json.Unmarshal(statusOutput, &status); err != nil || !status.LoggedIn {
		return fmt.Errorf("verify Claude can use the restored %q Keychain item; auth status did not confirm a login, stop using Claude and retry restoration from the active hop slot", keychainService)
	}
	return nil
}
