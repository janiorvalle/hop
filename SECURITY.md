# Security

Please report security issues through GitHub's private vulnerability reporting
for this repository. Do not open a public issue containing exploit details,
credentials, tokens, or other sensitive data.

You should receive an initial response within three business days. Security
fixes target the latest supported release.

## Trust Model

`hop` handles Claude Code and Codex OAuth credentials. It keeps account copies
in its own data directory — `~/.hop`, or `HOP_HOME` when set — with private
directory and file permissions, and it sends credentials only to the provider's
own token and usage endpoints. Nothing is uploaded anywhere else, and there is
no hop server.

Live credential locations belong to the provider. `hop` replaces the file or
Keychain item in place and never creates or re-permissions `~/.claude`,
`~/.codex`, or the Keychain itself. Account slots are read-only until `hop
login` takes custody of their refresh token, so a manually seeded slot is never
rotated behind your back.

Reports are most useful when they identify the input, affected version,
concrete impact, and a minimal reproduction. Scanner output without a credible
impact path is not enough by itself, but uncertain reports are still welcome
through the private channel.

Never attach real credentials to a report. A redacted excerpt showing the shape
of the data is enough.
