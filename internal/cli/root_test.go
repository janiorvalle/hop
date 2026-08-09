package cli

import (
	"bytes"
	"strings"
	"testing"
)

func TestHelpShowsPlannedCommandSurface(t *testing.T) {
	t.Parallel()

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
