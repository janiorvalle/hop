//go:build darwin

package claude

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"testing"
)

type recordedSecurityCall struct {
	input string
	args  []string
}

type fakeSecurity struct {
	calls   []recordedSecurityCall
	results []error
}

func (security *fakeSecurity) Run(_ context.Context, input string, args ...string) ([]byte, error) {
	security.calls = append(security.calls, recordedSecurityCall{input: input, args: args})
	if len(security.results) == 0 {
		return nil, nil
	}
	result := security.results[0]
	security.results = security.results[1:]
	return nil, result
}

// exitStatus produces the error a finished process yields, so the not-found
// status security(1) reports can be exercised without a Keychain.
func exitStatus(t *testing.T, code int) error {
	t.Helper()
	err := exec.Command("/bin/sh", "-c", fmt.Sprintf("exit %d", code)).Run()
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		t.Fatalf("run a process exiting with %d: %v", code, err)
	}
	return err
}

func TestKeychainWriteCommandCarriesTheSecretThroughStdin(t *testing.T) {
	t.Parallel()

	contents := `{"claudeAiOauth":{"accessToken":"sk\"quote\\slash","refreshToken":"r"}}`
	command, err := keychainWriteCommand("Claude Code-credentials", "owner", "/usr/local/bin/claude", contents)
	if err != nil {
		t.Fatalf("keychainWriteCommand() error = %v", err)
	}
	want := `add-generic-password -U -a "owner" -s "Claude Code-credentials" -T "/usr/local/bin/claude" ` +
		`-w "{\"claudeAiOauth\":{\"accessToken\":\"sk\\\"quote\\\\slash\",\"refreshToken\":\"r\"}}"` + "\n"
	if command != want {
		t.Fatalf("keychainWriteCommand() =\n%q\nwant\n%q", command, want)
	}
}

// security(1)'s interactive tokenizer strips one layer of double quotes and
// resolves backslash escapes, so escaping has to survive that pass byte for
// byte: the Claude CLI reads the very same item.
func TestQuoteSecurityArgumentSurvivesTheInteractiveTokenizer(t *testing.T) {
	t.Parallel()

	for name, value := range map[string]string{
		"plain":            "token",
		"double quotes":    `{"a":"b"}`,
		"backslashes":      `a\\b\nc`,
		"quoted backslash": `he said \"hi\"`,
		"spaces and tabs":  "a b\tc",
		"shell characters": "$HOME `id` ; rm -rf / | tee",
		"unicode":          "café ✓",
		"empty":            "",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			quoted := quoteSecurityArgument(value)
			if got := untokenizeSecurityArgument(t, quoted); got != value {
				t.Fatalf("tokenizing %q gave %q, want %q", quoted, got, value)
			}
		})
	}
}

// untokenizeSecurityArgument reverses the tokenizer the way security(1) does:
// drop the surrounding quotes, then unescape backslash pairs.
func untokenizeSecurityArgument(t *testing.T, quoted string) string {
	t.Helper()
	if len(quoted) < 2 || quoted[0] != '"' || quoted[len(quoted)-1] != '"' {
		t.Fatalf("quoted argument %q is not wrapped in double quotes", quoted)
	}
	body := quoted[1 : len(quoted)-1]
	var value strings.Builder
	for index := 0; index < len(body); index++ {
		if body[index] == '\\' && index+1 < len(body) {
			index++
		}
		value.WriteByte(body[index])
	}
	return value.String()
}

func TestKeychainWriteCommandRefusesCredentialsSecurityWouldTruncate(t *testing.T) {
	t.Parallel()

	oversized := strings.Repeat("A", securityCommandLimit)
	_, err := keychainWriteCommand("Claude Code-credentials", "owner", "/usr/local/bin/claude", oversized)
	if err == nil || !strings.Contains(err.Error(), "truncated login") {
		t.Fatalf("keychainWriteCommand() error = %v, want a refusal that names the truncation risk", err)
	}
}

func TestKeychainWriteCommandRefusesValuesThatWouldSplitTheCommand(t *testing.T) {
	t.Parallel()

	_, err := keychainWriteCommand("Claude Code-credentials", "owner", "/usr/local/bin/claude", "first\ndelete-generic-password -s x")
	if err == nil || !strings.Contains(err.Error(), "line break") {
		t.Fatalf("keychainWriteCommand() error = %v, want a refusal that names the line break", err)
	}
}

func TestClearLiveCredentialsDeletesTheItemThenConfirmsNoneRemains(t *testing.T) {
	t.Parallel()

	security := &fakeSecurity{results: []error{nil, exitStatus(t, securityItemNotFound)}}
	if err := clearLiveCredentials(context.Background(), security); err != nil {
		t.Fatalf("clearLiveCredentials() error = %v", err)
	}
	if len(security.calls) != 2 {
		t.Fatalf("security calls = %d, want one delete and one confirmation", len(security.calls))
	}
	deleteCall := security.calls[0]
	if len(deleteCall.args) != 1 || deleteCall.args[0] != "-i" {
		t.Fatalf("delete args = %v, want interactive mode", deleteCall.args)
	}
	if deleteCall.input != `delete-generic-password -s "Claude Code-credentials"`+"\n" {
		t.Fatalf("delete input = %q, want the delete command on stdin", deleteCall.input)
	}
	confirmCall := security.calls[1]
	want := []string{"find-generic-password", "-s", "Claude Code-credentials"}
	if strings.Join(confirmCall.args, " ") != strings.Join(want, " ") {
		t.Fatalf("confirmation args = %v, want %v without -w", confirmCall.args, want)
	}
}

func TestClearLiveCredentialsTreatsAMissingItemAsCleared(t *testing.T) {
	t.Parallel()

	security := &fakeSecurity{results: []error{exitStatus(t, securityItemNotFound), exitStatus(t, securityItemNotFound)}}
	if err := clearLiveCredentials(context.Background(), security); err != nil {
		t.Fatalf("clearLiveCredentials() error = %v", err)
	}
}

// Deleting every item that shares the service would destroy a login hop never
// copied anywhere, so a leftover duplicate has to stop the enrollment instead.
func TestClearLiveCredentialsRefusesToDeleteAnItemItHasNoCopyOf(t *testing.T) {
	t.Parallel()

	security := &fakeSecurity{results: []error{nil, nil}}
	err := clearLiveCredentials(context.Background(), security)
	if err == nil || !strings.Contains(err.Error(), "Keychain Access") {
		t.Fatalf("clearLiveCredentials() error = %v, want a refusal that names the manual fix", err)
	}
	if len(security.calls) != 2 {
		t.Fatalf("security calls = %d, want the delete to stop after the duplicate is seen", len(security.calls))
	}
}

func TestClearLiveCredentialsReportsAFailureItCannotExplainAway(t *testing.T) {
	t.Parallel()

	security := &fakeSecurity{results: []error{exitStatus(t, 1)}}
	err := clearLiveCredentials(context.Background(), security)
	if err == nil || !strings.Contains(err.Error(), "unlock Keychain and retry") {
		t.Fatalf("clearLiveCredentials() error = %v, want a failure that tells the reader what to do", err)
	}
	if len(security.calls) != 1 {
		t.Fatalf("security calls = %d, want nothing after a delete hop could not explain", len(security.calls))
	}
}

func TestReadLiveCredentialsAsksSecurityForTheItemOnly(t *testing.T) {
	t.Parallel()

	security := &fakeSecurity{results: []error{errors.New("locked")}}
	if _, err := readLiveCredentials(context.Background(), security); err == nil {
		t.Fatal("readLiveCredentials() error = nil, want the failure surfaced")
	}
	want := []string{"find-generic-password", "-s", "Claude Code-credentials", "-w"}
	if len(security.calls) != 1 || strings.Join(security.calls[0].args, " ") != strings.Join(want, " ") {
		t.Fatalf("security calls = %+v, want %v", security.calls, want)
	}
	if security.calls[0].input != "" {
		t.Fatalf("security input = %q, want nothing on stdin for a read", security.calls[0].input)
	}
}
