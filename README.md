# hop

<p align="center">
  <img src="assets/hero.png" alt="hop — character select for your Claude and Codex accounts. glance. hop. keep playing." width="840">
</p>

You're running a few Claude Code accounts and a few Codex accounts, because
that's what it takes now. Somewhere around the second "approaching your weekly
limit" of the day, the question is always the same: which account still has
room? Logging into each one to check — MFA and all — is nobody's idea of a
good time.

`hop` answers it in one command. Type `hop` and every account gets one line:
a headroom bar, the number, and when the limit behind it resets, live from the
same endpoints the CLIs use. Most room sorts first. Type `hop work` and that
account becomes the live login for every provider that has one. Character
select, for your accounts.

```
CLAUDE
    ACCOUNT    HEADROOM                      RESET
  ● personal   █████████████████░░░    83%   6d18h
> ◐ work       ██████████░░░░░░░░░░    49%   4d17h
  ! old                              ERROR

CODEX
    ACCOUNT    HEADROOM                      RESET
> ● work       ███████████████████░    95%   6d20h
  ○ personal   ██░░░░░░░░░░░░░░░░░░     8%   11h07m

attention
 1. claude/personal: token expires 1d13h; hop rm claude personal; hop login claude personal
 2. claude/old: usage unavailable (HTTP 400); hop login claude old

● 50-100 plenty   ◐ 10-49 tight   ○ 0-9 nearly/full   ! error   > active
```

Headroom is what's left at the tightest limit on the account, and the reset
is that limit's. Anything that needs a hand, a token about to expire or a
fetch that failed, waits in the numbered attention list under the tables with
the command that fixes it, so the rows stay clean. The 5-hour and weekly
meters, every model-scoped limit, the plan, and the manual resets left are in
`hop ls --json`. With `NO_COLOR` set or a pipe on stdout the bars are `#` and
`.` and the glyphs carry the state.

The switch is careful on purpose: before installing the account you asked
for, hop copies the current live credentials back to the slot they came from,
so the login you're leaving is never lost. It swaps credentials and nothing
else — provider settings, skills, and history stay where they are. One
caveat: stop running Claude and Codex sessions before hopping, since a
session already in flight can fail its next token refresh.

An account you are not using right now can be parked instead of deleted:
`hop disable codex old` stops the glance from fetching its usage and shows the
row dimmed as `disabled`, and `hop old` refuses to switch to it until
`hop enable codex old` brings it back. The credentials stay in the slot the
whole time, so there is nothing to re-enroll.

## Spending a Codex manual reset

OpenAI grants some Codex plans a few manual quota resets. `hop ls --json`
reports them as `reset_credits`, and `hop reset codex work` spends one without
opening the ChatGPT app. It asks first, since a reset is gone once spent:

```
This spends 1 of 2 manual resets on codex account "work". Continue? [y/N]
```

Automation can answer with `HOP_CODEX_RESET=approved`. Hop records the request
id in the account slot before it calls OpenAI and sends the same id again on a
retry, so a dropped connection never spends two credits. While that request is
still unconfirmed, `hop rm codex work` refuses to forget the account, and
`hop login codex work` signs the same identity in again without touching the
record, so an expired token never costs a second credit. An account with no
credits left is refused with the count and when its weekly limit resets on its
own. Claude has no manual resets, so `hop reset claude` is refused.

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
  they came from, so the login you're leaving is never lost. Claude Code keeps
  its MCP server logins in that same item, and those ride with the machine,
  not the account: every hop carries the live set along, so Linear and the
  rest stay connected and a server you disconnect stays disconnected.
- **Idle account tokens, before they die.** Every glance rotates a managed
  account's tokens when its access token is about to expire, or when its
  refresh token has less than seven days left, so an account you haven't
  looked at in weeks doesn't quietly send you back through MFA. Claude says
  when its refresh token expires. Codex doesn't, so hop treats a Codex refresh
  token as good for fifteen days after its `last_refresh`, which rotates it at
  eight days, the same age the Codex CLI itself renews at. Any account, managed
  or not, gets a line in the attention list under the glance inside seven
  days, red inside two, with the commands that renew it: `hop login codex
  <name>` signs a Codex account in again in place, and a Claude account needs
  `hop rm` before `hop login`. `hop ls --json` carries the same thing as
  `refresh_token_expiry`. `hop refresh` does the same rotation without
  fetching usage, so a scheduler can run it for you.

What hop never does:

- **Never creates or re-permissions provider-owned locations.** `~/.claude`,
  `~/.codex`, and the macOS Keychain belong to the provider. Hop replaces the
  file or Keychain item in place, and if the directory doesn't already exist
  it stops and tells you rather than creating one.
- **Never refreshes tokens in a slot it doesn't manage.** Slots are
  default-deny: an account you seeded by hand is read-only until `hop login`
  enrolls it and takes custody of its refresh token.
- **Never touches your live Claude login to add an account.** `hop login
  claude <name>` opens your browser, catches the sign-in on
  `http://localhost:54545/callback`, trades the code for tokens itself, and
  writes them straight into the new slot. The Keychain item is never read or
  written, so running Claude sessions keep going. Set `BROWSER` to pick the
  opener and `HOP_CLAUDE_LOGIN_PORT` if something else holds the port. The
  first Claude account still adopts the login you already have.
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

## Keep idle accounts warm

An account you never glance at still dies when its refresh token expires.
`hop refresh` makes the same rotation decision a glance makes, for every
account hop manages, and fetches nothing else. It runs with no daemon, so
schedule it once a day and read its log.

```sh
hop refresh
```

Each run prints one line per account, in the same order `hop ls` uses:

```
claude personal: rotated, refresh token good until 2026-10-05
claude work: fresh, no rotation needed
claude seeded: skipped: not managed by hop, run 'hop rm claude seeded' and then 'hop login claude seeded' to let hop rotate it
codex work: skipped: active account, hop never rotates the live login
codex old: failed: codex token endpoint returned HTTP 502; the slot was not changed, run 'hop login codex old'
```

Grep the log for `failed`. Every failed line ends with the next step, and the
exit code is non-zero whenever any line failed. Everything else is
informational.

On macOS, save this as `~/Library/LaunchAgents/com.hop.refresh.plist` and load
it with `launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/com.hop.refresh.plist`.
Replace `/Users/you` with your home directory; launchd does not expand `~`.

```xml
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>com.hop.refresh</string>
  <key>ProgramArguments</key>
  <array>
    <string>/Users/you/.local/bin/hop</string>
    <string>refresh</string>
  </array>
  <key>StartCalendarInterval</key>
  <dict>
    <key>Hour</key>
    <integer>9</integer>
    <key>Minute</key>
    <integer>0</integer>
  </dict>
  <key>StandardOutPath</key>
  <string>/Users/you/Library/Logs/hop-refresh.log</string>
  <key>StandardErrorPath</key>
  <string>/Users/you/Library/Logs/hop-refresh.log</string>
</dict>
</plist>
```

On Linux, or with cron on macOS, add one line with `crontab -e`:

```
0 9 * * * "$HOME/.local/bin/hop" refresh >> "$HOME/.hop/refresh.log" 2>&1
```

Cron mails a job's output and ignores its exit code, so drop the redirection
and set `MAILTO` if you would rather get every run by mail than in a log.

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
