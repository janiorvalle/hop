package cli

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
)

func TestHelpShowsPlannedCommandSurface(t *testing.T) {
	// Not parallel: HOP_HOME keeps the test binary, itself a development build,
	// from printing the sandbox warning onto the stderr this test asserts on.
	t.Setenv("HOP_HOME", t.TempDir())

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := Run([]string{"--help"}, &stdout, &stderr)

	if exitCode != 0 {
		t.Fatalf("Run() exit code = %d, want 0; stderr = %q", exitCode, stderr.String())
	}
	commands := []string{
		"hop <account>",
		"hop <provider> <account>",
		"hop login <provider> <account>",
		"hop ls [--json]",
		"hop rm <provider> <account>",
		"hop upgrade",
	}
	for _, command := range commands {
		if !strings.Contains(stdout.String(), command) {
			t.Errorf("help output missing %q", command)
		}
	}
	if stderr.Len() != 0 {
		t.Errorf("stderr = %q, want empty", stderr.String())
	}
}

func TestVersionPrintsTheBuiltVersion(t *testing.T) {
	t.Parallel()

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := Run([]string{"--version"}, &stdout, &stderr)

	if exitCode != 0 {
		t.Fatalf("Run() exit code = %d, want 0; stderr = %q", exitCode, stderr.String())
	}
	if got, want := stdout.String(), "hop "+version()+"\n"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
}

func TestResolveVersionPrefersTheStampedReleaseVersion(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name    string
		stamped string
		module  string
		want    string
	}{
		{name: "released binary carries the linker-stamped tag", stamped: "1.4.0", module: "v1.3.0", want: "1.4.0"},
		{name: "go install records the module version", stamped: "", module: "v1.3.0", want: "v1.3.0"},
		{name: "working-tree build names nothing installable", stamped: "", module: "(devel)", want: "dev"},
		{name: "build info missing entirely", stamped: "", module: "", want: "dev"},
		{name: "clean checkout before the first release tag", stamped: "", module: "v0.0.0-20260809191616-20013681d086", want: "dev"},
		{name: "dirty checkout before the first release tag", stamped: "", module: "v0.0.0-20260809191616-20013681d086+dirty", want: "dev"},
		{name: "clean checkout past a release tag", stamped: "", module: "v1.4.1-0.20260809192816-aa3f2ca330f0", want: "dev"},
		{name: "dirty checkout past a release tag", stamped: "", module: "v1.4.1-0.20260809192816-aa3f2ca330f0+dirty", want: "dev"},
		{name: "dirty checkout sitting on a release tag", stamped: "", module: "v1.4.0+dirty", want: "dev"},
		{name: "clean checkout sitting on a release tag is that release", stamped: "", module: "v1.4.0", want: "v1.4.0"},
		{name: "prerelease tag is still a release someone installed", stamped: "", module: "v1.4.0-rc.1", want: "v1.4.0-rc.1"},
		{name: "pseudo-version yields to a stamped release", stamped: "1.4.0", module: "v0.0.0-20260809191616-20013681d086", want: "1.4.0"},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			if got := resolveVersion(testCase.stamped, testCase.module); got != testCase.want {
				t.Errorf("resolveVersion(%q, %q) = %q, want %q", testCase.stamped, testCase.module, got, testCase.want)
			}
		})
	}
}

func TestVersionRejectsExtraArguments(t *testing.T) {
	t.Parallel()

	var stderr bytes.Buffer
	exitCode := Run([]string{"--version", "work"}, &bytes.Buffer{}, &stderr)

	if exitCode != 2 {
		t.Fatalf("Run() exit code = %d, want 2", exitCode)
	}
	if got := stderr.String(); !strings.Contains(got, "hop --version") {
		t.Fatalf("stderr = %q, want the corrected invocation", got)
	}
}

func TestUpgradeRejectsExtraArguments(t *testing.T) {
	t.Parallel()

	var stderr bytes.Buffer
	exitCode := Run([]string{"upgrade", "now"}, &bytes.Buffer{}, &stderr)

	if exitCode != 2 {
		t.Fatalf("Run() exit code = %d, want 2", exitCode)
	}
	if got := stderr.String(); !strings.Contains(got, "hop upgrade") {
		t.Fatalf("stderr = %q, want the corrected invocation", got)
	}
}

func TestLoginRejectsUnknownProviderWithNextStep(t *testing.T) {
	t.Parallel()

	var stderr bytes.Buffer
	exitCode := Run([]string{"login", "other", "work"}, &bytes.Buffer{}, &stderr)

	if exitCode != 2 {
		t.Fatalf("Run() exit code = %d, want 2", exitCode)
	}
	if got := stderr.String(); !strings.Contains(got, "use claude or codex") {
		t.Fatalf("stderr = %q, want provider correction", got)
	}
}

func TestOnlyDevelopmentBuildsWithoutASandboxWarn(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name           string
		runningVersion string
		hopHome        string
		want           string
	}{
		{name: "working-tree build on the real vault", runningVersion: resolveVersion("", "(devel)"), hopHome: "", want: developmentVaultWarning},
		{name: "clean-checkout build on the real vault", runningVersion: resolveVersion("", "v0.0.0-20260809191616-20013681d086"), hopHome: "", want: developmentVaultWarning},
		{name: "dirty-checkout build on the real vault", runningVersion: resolveVersion("", "v0.0.0-20260809191616-20013681d086+dirty"), hopHome: "", want: developmentVaultWarning},
		{name: "post-tag checkout build on the real vault", runningVersion: resolveVersion("", "v1.4.1-0.20260809192816-aa3f2ca330f0"), hopHome: "", want: developmentVaultWarning},
		{name: "working-tree build in a sandbox", runningVersion: resolveVersion("", "(devel)"), hopHome: "/tmp/hop-sandbox", want: ""},
		{name: "stamped release build on the real vault", runningVersion: resolveVersion("1.4.0", "(devel)"), hopHome: "", want: ""},
		{name: "go install build on the real vault", runningVersion: resolveVersion("", "v1.3.0"), hopHome: "", want: ""},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			var stderr bytes.Buffer
			warnWhenDevelopmentBuildUsesTheRealVault(&stderr, testCase.runningVersion, testCase.hopHome)
			if got := stderr.String(); got != testCase.want {
				t.Errorf("stderr = %q, want %q", got, testCase.want)
			}
		})
	}
}

func TestRunWarnsOnceAndLeavesStdoutAlone(t *testing.T) {
	sandboxHome(t)
	t.Setenv("HOP_HOME", "")

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if exitCode := Run([]string{"--version"}, &stdout, &stderr); exitCode != 0 {
		t.Fatalf("Run() exit code = %d, want 0; stderr = %q", exitCode, stderr.String())
	}

	if got := strings.Count(stderr.String(), developmentVaultWarning); got != 1 {
		t.Errorf("warning printed %d times, want 1; stderr = %q", got, stderr.String())
	}
	if got, want := stdout.String(), "hop "+version()+"\n"; got != want {
		t.Errorf("stdout = %q, want %q", got, want)
	}
}

func TestListJSONPrintsTheSameStdoutWithOrWithoutTheWarning(t *testing.T) {
	home := sandboxHome(t)

	t.Setenv("HOP_HOME", "")
	var warnedStdout, warnedStderr bytes.Buffer
	if exitCode := Run([]string{"ls", "--json"}, &warnedStdout, &warnedStderr); exitCode != 0 {
		t.Fatalf("Run() exit code = %d, want 0; stderr = %q", exitCode, warnedStderr.String())
	}

	// The same vault, now named explicitly: only the warning may differ.
	t.Setenv("HOP_HOME", filepath.Join(home, ".hop"))
	var sandboxedStdout, sandboxedStderr bytes.Buffer
	if exitCode := Run([]string{"ls", "--json"}, &sandboxedStdout, &sandboxedStderr); exitCode != 0 {
		t.Fatalf("Run() exit code = %d, want 0; stderr = %q", exitCode, sandboxedStderr.String())
	}

	if warnedStdout.String() != sandboxedStdout.String() {
		t.Errorf("stdout differs with the warning: %q vs %q", warnedStdout.String(), sandboxedStdout.String())
	}
	if got := warnedStderr.String(); got != developmentVaultWarning {
		t.Errorf("stderr = %q, want the sandbox warning", got)
	}
	if got := sandboxedStderr.String(); got != "" {
		t.Errorf("stderr = %q, want empty for a sandboxed run", got)
	}
}

// sandboxHome points the home directory and every live credential path at
// throwaway directories so a run with HOP_HOME unset still cannot reach a
// developer's real ~/.hop, Keychain, or ~/.codex.
func sandboxHome(t *testing.T) string {
	t.Helper()

	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv(claudeCredentialsFileOverride, filepath.Join(t.TempDir(), "credentials.json"))
	t.Setenv(claudeAccountEmailOverride, "sandbox@example.com")
	t.Setenv(codexAuthFileOverride, filepath.Join(t.TempDir(), "auth.json"))
	return home
}

func TestSwitchCommandsFailClearlyWhenAccountIsMissing(t *testing.T) {
	t.Setenv("HOP_HOME", t.TempDir())
	testCases := [][]string{
		{"work"},
		{"claude", "work"},
	}
	for _, args := range testCases {
		var stderr bytes.Buffer
		exitCode := Run(args, &bytes.Buffer{}, &stderr)
		if exitCode != 2 {
			t.Errorf("Run(%q) exit code = %d, want 2", args, exitCode)
		}
		if got := stderr.String(); !strings.Contains(got, "hop login") {
			t.Errorf("Run(%q) stderr = %q, want enrollment guidance", args, got)
		}
	}
}
