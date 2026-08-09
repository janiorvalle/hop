# Contributing

Thanks for pitching in. Here's what you need to get going.

## Development Setup

Requirements:

- Go 1.25 or newer
- Git
- `shellcheck` if you touch `install.sh` or anything in `scripts/`

Clone the repository and run the fast gate:

```sh
make fast
```

`make fast` is format, vet, lint, and test — the same gate CI's `Verify` job
runs. Run it before opening a pull request.

Changes that affect a release — `.goreleaser.yaml`, `install.sh`, the build
flags, or anything else that ships in an archive — also need the full gate:

```sh
make full
```

`make full` validates the GoReleaser configuration, builds a snapshot across
the macOS, Linux, and Windows matrix, and installs from that snapshot. It takes
minutes rather than seconds, which is why it's not part of `make fast`.

## Development vs. your real hop

Development builds and the hop you actually use are separate things, and the
separation is deliberate — a dev build that writes to `~/.hop` can corrupt the
accounts you switch between every day.

```sh
make dev                                   # builds dist/hop-dev
HOP_HOME=/tmp/hop-sandbox ./dist/hop-dev ls
```

- The development binary is `dist/hop-dev`. It is never named `hop` and never
  goes on `PATH`, so it cannot shadow the installed one.
- `~/.local/bin/hop` belongs to `install.sh` and `hop upgrade`. Nothing else
  writes there — never `go build -o ~/.local/bin/hop`.
- Every dev run sets `HOP_HOME` to a throwaway directory. A dev build started
  without it prints a one-line warning to stderr and then proceeds, because
  sometimes you really do mean it. Released builds never print it.
- `HOP_HOME` redirects the vault, not the live credentials. Read-only commands
  like `ls` are fully sandboxed by it; a dev build that actually switches still
  writes the real `~/.codex/auth.json` and Keychain item unless you also set
  `HOP_CODEX_AUTH_FILE`, `HOP_CLAUDE_CREDENTIALS_FILE`, and
  `HOP_CLAUDE_ACCOUNT_EMAIL` — the same overrides the test policy below uses.

## Test Policy

Tests must never touch real credentials. Use `HOP_CLAUDE_CREDENTIALS_FILE` and
`HOP_CODEX_AUTH_FILE` to point the live credential targets at a temporary
directory, or inject a fake store. A Claude file override also requires
`HOP_CLAUDE_ACCOUNT_EMAIL` so copy-back ownership can be verified. A test that
can reach a developer's Keychain or `~/.codex/auth.json` is a bug, even when it
passes.

Tests must not depend on the network, on an installed `claude` or `codex`, or
on a populated `~/.hop`. The opt-in live tests behind `HOP_CLAUDE_LIVE_TEST`
and `HOP_CODEX_LIVE_TEST` must keep skipping cleanly when those variables are
absent, and they never run in normal CI.

## Release Setup

The repository owner must create a GitHub environment named `release` under
Settings > Environments and add the desired deployment protection rules. The
tag workflow is already bound to that environment; repository settings are not
changed by the workflow itself.

## Pull Requests

- Behavior changes come with tests. Bug fixes come with a test that fails
  before the fix.
- CI must be green. The required checks are `Verify`, `Go (macos-latest)`,
  `Go (ubuntu-latest)`, `Go (windows-latest)`, `Installer smoke`,
  `Workflow lint`, `Secret scan`, and `CLA check`.
- Keep the change focused. A rename or a cleanup that is not part of the fix
  belongs in its own pull request.
- Errors are written for whoever hits them: say what was wrong, what was
  expected, and what to do next. Match the style of the errors already in the
  package you are editing.

## Licensing

Contributions are licensed under the MIT License and the project's
[Individual Contributor License Agreement](docs/CLA.md). The CLA check runs
automatically on a contributor's first pull request; sign once by posting the
comment the bot asks for, and it covers your future contributions here.
