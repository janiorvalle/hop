# hop

<p align="center">
  <img src="assets/hero.png" alt="hop — character select for your Claude and Codex accounts. glance. hop. keep playing." width="840">
</p>

You're running a few Claude Code accounts and a few Codex accounts, because
that's what it takes now. Somewhere around the second "approaching your weekly
limit" of the day, the question is always the same: which account still has
room? Logging into each one to check — MFA and all — is nobody's idea of a
good time.

`hop` answers it in one command. Type `hop` and every account lines up in one
table — 5-hour and weekly usage, live from the same endpoints the CLIs use.
Type `hop work` and that account becomes the live login for every provider
that has one. Character select, for your accounts.

The switch is careful on purpose: before installing the account you asked
for, hop copies the current live credentials back to the slot they came from,
so the login you're leaving is never lost. It swaps credentials and nothing
else — provider settings, skills, and history stay where they are. One
caveat: stop running Claude and Codex sessions before hopping, since a
session already in flight can fail its next token refresh.

## What hop touches (and what it never does)

Hop talks to the same usage and OAuth endpoints the Claude Code and Codex CLIs
use. Those endpoints are undocumented and unsupported for outside use, so a
vendor change can break usage numbers or token refresh with no warning and no
version bump. Hop is not affiliated with, endorsed by, or supported by
Anthropic or OpenAI.

What hop touches:

- **Credentials, and only credentials.** Provider settings, skills, MCP
  servers, and history stay exactly where they are.
- **Its own directory.** Account copies live in `~/.hop`, or `HOP_HOME` when
  you set it, with private permissions.
- **The live credential slot, in place.** Before installing the account you
  asked for, hop copies the current live credentials back to the account slot
  they came from, so the login you're leaving is never lost.

What hop never does:

- **Never creates or re-permissions provider-owned locations.** `~/.claude`,
  `~/.codex`, and the macOS Keychain belong to the provider. Hop replaces the
  file or Keychain item in place, and if the directory doesn't already exist
  it stops and tells you rather than creating one.
- **Never refreshes tokens in a slot it doesn't manage.** Slots are
  default-deny: an account you seeded by hand is read-only until `hop login`
  enrolls it and takes custody of its refresh token.
- **Never starts a Claude browser login behind your back.** Adding a second
  Claude account has to borrow the live login while the browser flow runs, so
  hop refuses until you stop your Claude sessions and rerun with
  `HOP_CLAUDE_LIVE_LOGIN=approved`.
- **Never restores a sandboxed switch into your real credentials.** If a
  switch is interrupted, recovery refuses any transaction that was recorded
  against different live targets than the ones in play now.
- **Never sends your credentials anywhere but the provider.** There is no hop
  server and no telemetry.

## Install

**Not published yet.** This repository is still private with no tagged
release, so the commands below return a 404 today. They start working with the
first public release. Until then, build from a clone:

```sh
go build -o hop .
```

Once hop is published — macOS and Linux, no sudo. The script verifies the
release checksum before it installs `hop` into `~/.local/bin`:

```sh
curl -fsSL https://raw.githubusercontent.com/janiorvalle/hop/main/install.sh | sh
```

Set `HOP_INSTALL_DIR` to install somewhere else, or `HOP_INSTALL_VERSION` to
pin a version. Windows builds ship as a zip on the
[releases page](https://github.com/janiorvalle/hop/releases). To install from
source instead:

```sh
go install github.com/janiorvalle/hop@latest
```

`hop --version` reports the installed version.

Once a published release is installed, update it in place without rerunning
the installer:

```sh
hop upgrade
```

Hop downloads the archive for the current OS and architecture, verifies it
against the release's `checksums.txt`, and only then replaces the running
binary. Development and dirty builds refuse self-upgrade; install a published
release first.

## Development

Run the fast gate while you work:

```sh
make fast
```

Build and smoke the full release matrix before release-affecting changes:

```sh
make full
```

See [CONTRIBUTING.md](CONTRIBUTING.md) for the test policy — tests never touch
real credentials — and the release environment setup.
