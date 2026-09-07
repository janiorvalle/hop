//go:build darwin

package claude

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
)

const keychainService = "Claude Code-credentials"

// security(1) reads each interactive command into a 4096-character buffer and
// runs whatever overflows as the next command. An item that does not fit on
// one line goes to security(1) as an argument instead, the way the Claude CLI
// stores the same item once its MCP logins outgrow the line.
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

// ClearLiveCredentialsIfMatches refuses to turn a read-then-delete into a
// compare-and-delete promise Keychain cannot provide. The user can remove the
// item explicitly, after which retrying hop completes recovery from absence.
func ClearLiveCredentialsIfMatches(ctx context.Context, expected Credentials) error {
	return clearLiveCredentialsIfMatches(ctx, systemSecurity{}, expected)
}

func clearLiveCredentialsIfMatches(ctx context.Context, security securityCommander, expected Credentials) error {
	live, err := readLiveCredentials(ctx, security)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if live.AccessToken != expected.AccessToken || live.RefreshToken != expected.RefreshToken {
		return fmt.Errorf("the live Claude Keychain item changed before hop could restore its previous absence; hop left the unexpected login untouched")
	}
	// Deleting the item locally leaves the OAuth grant intact, so the copies hop
	// stashed for other slots keep working; signing out of Claude would revoke it
	// server-side and force every slot to enroll again.
	return fmt.Errorf("the Claude Keychain item still contains the target login, but Keychain cannot delete it conditionally; delete the item yourself without signing out of Claude — open Keychain Access and delete the %q item, or run 'security delete-generic-password -s %q' — then retry hop to finish restoring the previous absence", keychainService, keychainService)
}

func readLiveCredentials(ctx context.Context, security securityCommander) (Credentials, error) {
	contents, err := security.Run(ctx, "", "find-generic-password", "-s", keychainService, "-w")
	if err != nil {
		if securityExitCode(err) == securityItemNotFound {
			err = errors.Join(os.ErrNotExist, err)
		}
		return Credentials{}, fmt.Errorf("read the %q Keychain item; unlock Keychain or run 'claude /login': %w", keychainService, err)
	}
	return parseCredentials([]byte(strings.TrimSpace(string(contents))))
}

func writeLiveCredentials(ctx context.Context, security securityCommander, credentials Credentials) error {
	contents, err := json.Marshal(credentialItem(credentials))
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
	item := keychainItem{service: keychainService, account: currentUser.Username, trustedApplication: claudePath, contents: string(contents)}
	if err := storeKeychainItem(ctx, security, item); err != nil {
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

// keychainItem is one generic password the way security(1) addresses it.
type keychainItem struct {
	service            string
	account            string
	trustedApplication string
	contents           string
}

// storeKeychainItem hands the item to security(1) on stdin, where no other
// process can read it, and as an argument only when interactive mode cannot
// take it on one line.
func storeKeychainItem(ctx context.Context, security securityCommander, item keychainItem) error {
	arguments := keychainWriteArguments(item)
	line := interactiveSecurityLine(arguments)
	if strings.ContainsAny(line, "\n\r") || len(line)+1 > securityCommandLimit {
		_, err := security.Run(ctx, "", arguments...)
		return err
	}
	_, err := security.Run(ctx, line+"\n", "-i")
	return err
}

func keychainWriteArguments(item keychainItem) []string {
	return []string{
		"add-generic-password", "-U",
		"-a", item.account,
		"-s", item.service,
		"-T", item.trustedApplication,
		"-w", item.contents,
	}
}

// interactiveSecurityLine writes the arguments the way security(1)'s
// interactive mode tokenizes a line read from stdin, without the line break
// that ends the command.
func interactiveSecurityLine(arguments []string) string {
	quoted := make([]string, len(arguments))
	for index, argument := range arguments {
		quoted[index] = quoteSecurityArgument(argument)
	}
	return strings.Join(quoted, " ")
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
