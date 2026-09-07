//go:build darwin

package claude

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
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
	outputs [][]byte
}

func (security *fakeSecurity) Run(_ context.Context, input string, args ...string) ([]byte, error) {
	security.calls = append(security.calls, recordedSecurityCall{input: input, args: args})
	var output []byte
	if len(security.outputs) > 0 {
		output = security.outputs[0]
		security.outputs = security.outputs[1:]
	}
	if len(security.results) == 0 {
		return output, nil
	}
	result := security.results[0]
	security.results = security.results[1:]
	return output, result
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

func TestStoreKeychainItemCarriesTheSecretThroughStdin(t *testing.T) {
	t.Parallel()

	contents := `{"claudeAiOauth":{"accessToken":"sk\"quote\\slash","refreshToken":"r"}}`
	security := &fakeSecurity{}
	if err := storeKeychainItem(context.Background(), security, testKeychainItem(contents)); err != nil {
		t.Fatalf("storeKeychainItem() error = %v", err)
	}
	want := `"add-generic-password" "-U" "-a" "owner" "-s" "Claude Code-credentials" "-T" "/usr/local/bin/claude" ` +
		`"-w" "{\"claudeAiOauth\":{\"accessToken\":\"sk\\\"quote\\\\slash\",\"refreshToken\":\"r\"}}"` + "\n"
	if len(security.calls) != 1 || strings.Join(security.calls[0].args, " ") != "-i" {
		t.Fatalf("security calls = %+v, want one interactive-mode call", security.calls)
	}
	if security.calls[0].input != want {
		t.Fatalf("security input =\n%q\nwant\n%q", security.calls[0].input, want)
	}
}

func testKeychainItem(contents string) keychainItem {
	return keychainItem{service: "Claude Code-credentials", account: "owner", trustedApplication: "/usr/local/bin/claude", contents: contents}
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

// An item longer than security(1)'s interactive line goes as an argument, so a
// Keychain full of MCP logins is stored whole instead of split into a bogus
// second command.
func TestStoreKeychainItemPassesAnOversizedItemAsAnArgument(t *testing.T) {
	t.Parallel()

	oversized := strings.Repeat("A", securityCommandLimit)
	security := &fakeSecurity{}
	if err := storeKeychainItem(context.Background(), security, testKeychainItem(oversized)); err != nil {
		t.Fatalf("storeKeychainItem() error = %v", err)
	}
	assertArgumentWrite(t, security, oversized)
}

func TestStoreKeychainItemPassesAnItemWithALineBreakAsAnArgument(t *testing.T) {
	t.Parallel()

	split := "first\ndelete-generic-password -s x"
	security := &fakeSecurity{}
	if err := storeKeychainItem(context.Background(), security, testKeychainItem(split)); err != nil {
		t.Fatalf("storeKeychainItem() error = %v", err)
	}
	assertArgumentWrite(t, security, split)
}

func assertArgumentWrite(t *testing.T, security *fakeSecurity, contents string) {
	t.Helper()
	want := []string{"add-generic-password", "-U", "-a", "owner", "-s", "Claude Code-credentials", "-T", "/usr/local/bin/claude", "-w", contents}
	if len(security.calls) != 1 || !slices.Equal(security.calls[0].args, want) {
		t.Fatalf("security calls = %+v, want one argument write %v", security.calls, want)
	}
	if security.calls[0].input != "" {
		t.Fatalf("security input = %q, want nothing on stdin for an argument write", security.calls[0].input)
	}
}

// Five MCP logins the size Claude Code stores put the item well past the
// interactive line; the argument write must carry every one of them intact.
func TestStoreKeychainItemKeepsEveryMCPLoginOfAnOversizedItem(t *testing.T) {
	t.Parallel()

	credentials := Credentials{AccessToken: "access", RefreshToken: "refresh", MCPTokens: MCPTokens{}}
	for _, server := range []string{"linear-server", "streamlyne-research", "plugin:posthog:posthog", "plugin:atlassian:atlassian", "plugin:datadog:mcp"} {
		credentials.MCPTokens[server+"|callback"] = json.RawMessage(fmt.Sprintf(`{"serverName":%q,"accessToken":%q,"refreshToken":%q,"expiresAt":1700000000000}`, server, strings.Repeat("a", 900), strings.Repeat("r", 300)))
	}
	contents, err := json.Marshal(credentialItem(credentials))
	if err != nil {
		t.Fatal(err)
	}
	if len(contents) <= securityCommandLimit {
		t.Fatalf("item is %d bytes, want one past the %d-character interactive line", len(contents), securityCommandLimit)
	}
	security := &fakeSecurity{}
	if err := storeKeychainItem(context.Background(), security, testKeychainItem(string(contents))); err != nil {
		t.Fatalf("storeKeychainItem() error = %v", err)
	}
	assertArgumentWrite(t, security, string(contents))
	stored, err := parseCredentials([]byte(security.calls[0].args[len(security.calls[0].args)-1]))
	if err != nil {
		t.Fatalf("parseCredentials() error = %v", err)
	}
	if len(stored.MCPTokens) != len(credentials.MCPTokens) {
		t.Fatalf("stored MCP logins = %d, want %d", len(stored.MCPTokens), len(credentials.MCPTokens))
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

func TestReadLiveCredentialsReportsMissingKeychainItemAsAbsent(t *testing.T) {
	security := &fakeSecurity{results: []error{exitStatus(t, securityItemNotFound)}}
	_, err := readLiveCredentials(context.Background(), security)
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("readLiveCredentials() error = %v, want missing credentials", err)
	}
}

func TestClearLiveCredentialsIfMatchesRefusesNonConditionalKeychainDelete(t *testing.T) {
	credentials := Credentials{AccessToken: "access", RefreshToken: "refresh"}
	contents, err := json.Marshal(credentialItem(credentials))
	if err != nil {
		t.Fatal(err)
	}
	security := &fakeSecurity{outputs: [][]byte{contents}}
	err = clearLiveCredentialsIfMatches(context.Background(), security, credentials)
	if err == nil || !strings.Contains(err.Error(), "cannot delete it conditionally") {
		t.Fatalf("ClearLiveCredentialsIfMatches() error = %v, want the non-conditional Keychain refusal", err)
	}
	if !strings.Contains(err.Error(), "security delete-generic-password -s \""+keychainService+"\"") {
		t.Fatalf("ClearLiveCredentialsIfMatches() error = %v, want the local Keychain deletion step", err)
	}
	// Signing out revokes the OAuth grant server-side, which would destroy the
	// copies hop stashed for every other slot.
	if strings.Contains(err.Error(), "auth logout") {
		t.Fatalf("ClearLiveCredentialsIfMatches() error = %v, want no sign-out recommendation", err)
	}
}
