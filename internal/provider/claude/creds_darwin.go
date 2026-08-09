//go:build darwin

package claude

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
)

const keychainService = "Claude Code-credentials"

// security(1) reads each interactive command into a 4096-character buffer and
// runs whatever overflows as the next command, which would store a truncated
// secret without failing. Refusing to build a longer command keeps that silent
// corruption impossible.
const securityCommandLimit = 4096

// security(1) exits with 44 when no Keychain item matches the search.
const securityItemNotFound = 44

// securityCommander runs macOS's security(1) tool. Tests inject a fake so the
// commands hop builds can be asserted without touching a Keychain.
type securityCommander interface {
	Run(ctx context.Context, input string, args ...string) ([]byte, error)
}

type systemSecurity struct{}

// LiveCredentialsTarget names where live Claude credentials are stored, for
// switch-transaction fingerprints.
func LiveCredentialsTarget() (string, error) {
	return "keychain:" + keychainService, nil
}

// ReadLiveCredentials reads Claude Code's Keychain item without changing it.
func ReadLiveCredentials(ctx context.Context) (Credentials, error) {
	return readLiveCredentials(ctx, systemSecurity{})
}

// WriteLiveCredentials replaces Claude Code's Keychain item.
func WriteLiveCredentials(ctx context.Context, credentials Credentials) error {
	return writeLiveCredentials(ctx, systemSecurity{}, credentials)
}

// ClearLiveCredentials removes Claude Code's Keychain item so the Claude CLI
// sees no login and opens its browser sign-in. An item that is already absent
// counts as cleared.
func ClearLiveCredentials(ctx context.Context) error {
	return clearLiveCredentials(ctx, systemSecurity{})
}

func readLiveCredentials(ctx context.Context, security securityCommander) (Credentials, error) {
	contents, err := security.Run(ctx, "", "find-generic-password", "-s", keychainService, "-w")
	if err != nil {
		return Credentials{}, fmt.Errorf("read the %q Keychain item; unlock Keychain or run 'claude /login': %w", keychainService, err)
	}
	return parseCredentials([]byte(strings.TrimSpace(string(contents))))
}

func writeLiveCredentials(ctx context.Context, security securityCommander, credentials Credentials) error {
	contents, err := json.Marshal(credentialEnvelope{OAuth: credentials})
	if err != nil {
		return err
	}
	currentUser, err := user.Current()
	if err != nil {
		return fmt.Errorf("find the macOS account that owns the Claude Keychain item: %w", err)
	}
	claudePath, err := claudeExecutablePath()
	if err != nil {
		return err
	}
	command, err := keychainWriteCommand(keychainService, currentUser.Username, claudePath, string(contents))
	if err != nil {
		return err
	}
	if _, err := security.Run(ctx, command, "-i"); err != nil {
		return fmt.Errorf("write the %q Keychain item; unlock Keychain and retry: %w", keychainService, err)
	}
	written, err := readLiveCredentials(ctx, security)
	if err != nil {
		return fmt.Errorf("verify the restored %q Keychain item; stop using Claude and retry restoration from the active hop slot: %w", keychainService, err)
	}
	if written.AccessToken != credentials.AccessToken || written.RefreshToken != credentials.RefreshToken {
		return fmt.Errorf("verify the restored %q Keychain item; stored credentials did not match, stop using Claude and retry restoration from the active hop slot", keychainService)
	}
	return verifyClaudeAcceptsLogin(ctx, claudePath)
}

func clearLiveCredentials(ctx context.Context, security securityCommander) error {
	command, err := keychainDeleteCommand(keychainService)
	if err != nil {
		return err
	}
	// Exactly one delete, because a Keychain search returns the same first match
	// to every tool: this removes the item ReadLiveCredentials handed to the
	// caller to stash, and nothing hop has no copy of.
	if _, err := security.Run(ctx, command, "-i"); err != nil && securityExitCode(err) != securityItemNotFound {
		return fmt.Errorf("clear the %q Keychain item so Claude opens a fresh browser login; unlock Keychain and retry: %w", keychainService, err)
	}
	remaining, err := keychainItemExists(ctx, security)
	if err != nil {
		return err
	}
	if remaining {
		return fmt.Errorf("clear the %q Keychain item so Claude opens a fresh browser login; a second item still uses that service and hop holds no copy of it, so open Keychain Access, remove or rename the duplicate, and retry", keychainService)
	}
	return nil
}

// keychainItemExists reports whether any item still uses the service. It asks
// for the item's attributes rather than its password, which keeps the check
// away from the access controls that guard the secret itself.
func keychainItemExists(ctx context.Context, security securityCommander) (bool, error) {
	_, err := security.Run(ctx, "", "find-generic-password", "-s", keychainService)
	if err == nil {
		return true, nil
	}
	if securityExitCode(err) == securityItemNotFound {
		return false, nil
	}
	return false, fmt.Errorf("confirm the %q Keychain item is gone before Claude's browser login opens; unlock Keychain and retry: %w", keychainService, err)
}

// keychainWriteCommand builds the security(1) interactive-mode command that
// stores contents as the item's password. Interactive mode reads its commands
// from stdin, so the secret never reaches the process arguments that every
// other program on the machine can read.
func keychainWriteCommand(service, account, trustedApplication, contents string) (string, error) {
	command := strings.Join([]string{
		"add-generic-password", "-U",
		"-a", quoteSecurityArgument(account),
		"-s", quoteSecurityArgument(service),
		"-T", quoteSecurityArgument(trustedApplication),
		"-w", quoteSecurityArgument(contents),
	}, " ")
	return terminateSecurityCommand(command, service)
}

// keychainDeleteCommand builds the security(1) interactive-mode command that
// removes the item.
func keychainDeleteCommand(service string) (string, error) {
	return terminateSecurityCommand("delete-generic-password -s "+quoteSecurityArgument(service), service)
}

func terminateSecurityCommand(command, service string) (string, error) {
	if strings.ContainsAny(command, "\n\r") {
		return "", fmt.Errorf("build the security command for the %q Keychain item; a line break in the macOS account name, the Claude CLI path, or the credential would split the command, so fix that value and retry", service)
	}
	if len(command) > securityCommandLimit {
		return "", fmt.Errorf("build the security command for the %q Keychain item; it is %d characters over the %d security(1) accepts on one line, so report this credential size to hop rather than storing a truncated login", service, len(command)-securityCommandLimit, securityCommandLimit)
	}
	return command + "\n", nil
}

// quoteSecurityArgument wraps value as one argument for security(1)'s
// interactive tokenizer, which strips a surrounding pair of double quotes and
// resolves backslash escapes inside them.
func quoteSecurityArgument(value string) string {
	var quoted strings.Builder
	quoted.Grow(len(value) + 2)
	quoted.WriteByte('"')
	for index := range len(value) {
		character := value[index]
		if character == '\\' || character == '"' {
			quoted.WriteByte('\\')
		}
		quoted.WriteByte(character)
	}
	quoted.WriteByte('"')
	return quoted.String()
}

func verifyClaudeAcceptsLogin(ctx context.Context, claudePath string) error {
	statusOutput, err := exec.CommandContext(ctx, claudePath, "auth", "status", "--json").Output()
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

func claudeExecutablePath() (string, error) {
	path, err := exec.LookPath("claude")
	if err != nil {
		return "", fmt.Errorf("find the Claude CLI before restoring its Keychain access; install Claude or add it to PATH, then retry: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", fmt.Errorf("resolve the Claude CLI at %s before restoring its Keychain access; fix the installation and retry: %w", path, err)
	}
	return resolved, nil
}

// securityExitCode reports the status security(1) exited with, or -1 when the
// command never finished.
func securityExitCode(err error) int {
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode()
	}
	return -1
}

func (systemSecurity) Run(ctx context.Context, input string, args ...string) ([]byte, error) {
	command := exec.CommandContext(ctx, "security", args...)
	command.Stdin = strings.NewReader(input)
	var failure bytes.Buffer
	command.Stderr = &failure
	output, err := command.Output()
	if err == nil {
		return output, nil
	}
	// security(1) echoes only the command name on failure, never its
	// arguments, so its own message is safe to carry into hop's error.
	if message := strings.TrimSpace(failure.String()); message != "" {
		return output, fmt.Errorf("%s: %w", message, err)
	}
	return output, err
}
