package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"time"
)

const helpText = `hop - glance at and switch between Claude and Codex accounts

Usage:
  hop                              Show usage for every account
  hop <account>                    Switch both providers with that account
  hop <provider> <account>         Switch one provider
  hop login <provider> <account>   Add an account
  hop ls [--json]                  List accounts
  hop rm <provider> <account>      Forget an account
  hop upgrade                      Install the latest verified release
  hop --version                    Show the installed version
  hop help                         Show this help

Providers:
  claude, codex

Examples:
  hop work
  hop claude personal
  hop login codex work
  hop ls --json
  hop rm codex old
`

// developmentVaultWarning is the one line an unreleased build prints when it is
// pointed at the same ~/.hop a released hop uses.
const developmentVaultWarning = "hop: development build using the real ~/.hop — set HOP_HOME to a sandbox to protect production data\n"

// Run executes the CLI and returns a process exit code.
func Run(args []string, stdout, stderr io.Writer) int {
	warnWhenDevelopmentBuildUsesTheRealVault(stderr, version(), os.Getenv("HOP_HOME"))

	err := execute(args, stdout, stderr)
	if err == nil {
		return 0
	}

	_, _ = fmt.Fprintf(stderr, "hop: %s\n", err)
	return 2
}

// warnWhenDevelopmentBuildUsesTheRealVault nudges a developer running an
// unreleased build without a sandbox. It stays on stderr so `hop ls --json`
// consumers read the same stdout a released build gives them. The HOP_HOME test
// matches defaultVault: any non-empty value already redirects the vault.
func warnWhenDevelopmentBuildUsesTheRealVault(stderr io.Writer, runningVersion, hopHome string) {
	if runningVersion != developmentVersion || hopHome != "" {
		return
	}
	_, _ = io.WriteString(stderr, developmentVaultWarning)
}

func execute(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return showAccountsSafely(context.Background(), stdout, stderr, false)
	}

	switch args[0] {
	case "help", "-h", "--help":
		if len(args) != 1 {
			return fmt.Errorf("help takes no arguments; try 'hop --help'")
		}
		_, err := io.WriteString(stdout, helpText)
		return err
	case "--version":
		if len(args) != 1 {
			return fmt.Errorf("--version takes no arguments; try 'hop --version'")
		}
		_, err := fmt.Fprintf(stdout, "hop %s\n", version())
		return err
	case "login":
		if err := requireProviderAccount("login", args[1:]); err != nil {
			return err
		}
		loginContext, stopSignals := signal.NotifyContext(context.Background(), loginTerminationSignals()...)
		defer stopSignals()
		if err := recoverDefaultSwitch(loginContext, stderr); err != nil {
			return err
		}
		return loginAccount(loginContext, args[1], args[2], os.Stdin, stdout, stderr)
	case "ls":
		if len(args) > 2 || len(args) == 2 && args[1] != "--json" {
			return fmt.Errorf("ls accepts only --json; try 'hop ls --json'")
		}
		return showAccountsSafely(context.Background(), stdout, stderr, len(args) == 2)
	case "rm":
		if err := requireProviderAccount("rm", args[1:]); err != nil {
			return err
		}
		return removeAccount(args[1], args[2], stdout)
	case "upgrade":
		if len(args) != 1 {
			return fmt.Errorf("upgrade takes no arguments; try 'hop upgrade'")
		}
		return upgradeHop(context.Background(), stdout)
	case "claude", "codex":
		if len(args) != 2 || strings.TrimSpace(args[1]) == "" {
			return fmt.Errorf("%s needs one account; try 'hop %s work'", args[0], args[0])
		}
		return switchAccount(context.Background(), args[0], args[1], stdout)
	default:
		if len(args) != 1 || strings.TrimSpace(args[0]) == "" {
			return fmt.Errorf("unknown command; try 'hop --help'")
		}
		return switchAccount(context.Background(), "", args[0], stdout)
	}
}

func showAccounts(ctx context.Context, stdout io.Writer, asJSON bool) error {
	accountCatalog, err := defaultCatalog()
	if err != nil {
		return err
	}
	return showAccountsFrom(ctx, stdout, asJSON, accountCatalog, time.Now())
}

func showAccountsFrom(ctx context.Context, stdout io.Writer, asJSON bool, accountCatalog catalog, now time.Time) error {
	document, err := fetchGlance(ctx, accountCatalog)
	if err != nil {
		return err
	}
	if asJSON {
		return writeJSON(stdout, document)
	}
	return writeTable(stdout, document, terminalOptions(stdout, now))
}

func requireProviderAccount(command string, args []string) error {
	if len(args) != 2 {
		return fmt.Errorf("%s needs a provider and account; try 'hop %s claude work'", command, command)
	}
	if args[0] != "claude" && args[0] != "codex" {
		return fmt.Errorf("unknown provider %q; use claude or codex", args[0])
	}
	if strings.TrimSpace(args[1]) == "" {
		return fmt.Errorf("account cannot be empty; try 'hop %s %s work'", command, args[0])
	}
	return nil
}
