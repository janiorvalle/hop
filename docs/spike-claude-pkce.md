# Spike: can hop own the Claude login?

Ticket: `tasks/doing/spike-claude-pkce.md`. Code: `spike/main.go`, a
throwaway `package main` with its own `go.mod` so it never links into hop.

## Where the spike got to

The authorize URL built with hop's existing public client id is accepted by
Anthropic. A real browser lands on the Claude sign-in page with the whole
authorize request carried in the `returnTo` parameter, and Anthropic raised no
complaint about the redirect URI, the scopes, or the PKCE challenge. The
browser step needs a human with MFA, so the spike stopped there. The callback,
state check, and code exchange are written and vetted but not yet exercised
against a live login.

## Finish the spike

From the worktree root, in one terminal:

```
cd spike && go run .
```

Copy the printed `https://claude.ai/oauth/authorize?...` URL into a normal
browser (not headless, see surprises), sign in, and approve. The spike catches
the redirect on `http://localhost:54545/callback`, checks `state`, exchanges the
code, and prints the token shape with every secret cut to its first six
characters plus its length. It never prints a full token.

If the exchange fails at hop's token endpoint, retry with CLIProxyAPI's:

```
cd spike && go run . -token-url https://api.anthropic.com/v1/oauth/token
```

Port 54545 is taken by CLIProxyAPI if it is running. `-port 54546` moves the
listener and the redirect URI together.

## What the spike sends

| Field | Value |
|---|---|
| authorize | `https://claude.ai/oauth/authorize` |
| client_id | `9d1c250a-e61b-44d9-88ed-5944d1962f5e` (hop's `defaultClientID`) |
| redirect_uri | `http://localhost:54545/callback` |
| scope | `user:profile user:inference user:sessions:claude_code user:mcp_servers user:file_upload` |
| extra params | `code=true`, `response_type=code`, `code_challenge_method=S256`, `state` (16 random bytes, hex) |
| PKCE verifier | 96 random bytes, base64url without padding (128 chars), S256 challenge |
| token endpoint | `https://platform.claude.com/v1/oauth/token` (hop's), JSON body |
| exchange body | `grant_type=authorization_code`, `code`, `state`, `client_id`, `redirect_uri`, `code_verifier` |
| headers | `Content-Type: application/json`, `Accept: application/json`, `anthropic-beta: oauth-2025-04-20` |

## Compared with CLIProxyAPI

Source: `internal/auth/claude/anthropic_auth.go` and `pkce.go` in the
CLIProxyAPI clone.

Identical: client id, authorize URL, redirect URI and port, scope list, the
`code=true` param, PKCE construction, the state format, and the exchange body.

Different on purpose:

- Token endpoint. CLIProxyAPI exchanges at `api.anthropic.com/v1/oauth/token`.
  The spike defaults to `platform.claude.com/v1/oauth/token` because that is
  where hop already refreshes with plain `net/http`, so we learn whether the
  same host also does the code exchange. The flag switches to CLIProxyAPI's.
- `anthropic-beta` header. hop sends `oauth-2025-04-20` on refresh, so the spike
  sends it on exchange too. CLIProxyAPI sends none.
- TLS. CLIProxyAPI wraps the token request in a uTLS transport that imitates a
  Firefox fingerprint to get past Cloudflare on Anthropic hosts. The spike uses
  Go's default client. hop's refresh already works that way against
  `platform.claude.com`, so this is the thing to watch when the human finishes
  the spike: a Cloudflare 403 on exchange means hop needs that transport too.
- Callback surface. CLIProxyAPI also serves `/success` and accepts a pasted
  callback URL for SSH sessions. The spike only serves `/callback` and binds
  `127.0.0.1` rather than all interfaces.

## Surprises

- Cloudflare Turnstile sits in front of `claude.ai/oauth/authorize`. A headless
  Chrome never gets past "Verify you are human". A headed Chrome passes without
  a click. So `hop login` must open the user's real browser; it can never drive
  the flow itself, and any agent-browser test of the login has to run headed.
- Anthropic bounces to `claude.ai/login?selectAccount=true&returnTo=...`. The
  `selectAccount=true` matters for hop: the user picks which account to sign in
  with on Anthropic's side, so enrolling a second account does not require
  logging the first one out anywhere.
- The redirect URI is plain `http://localhost:<port>/callback`, and Anthropic
  accepted it in the authorize request. Whether the exchange also accepts it is
  what the human's run answers.
- `refresh_token_expires_in` is in hop's refresh response struct but not in
  CLIProxyAPI's. The spike prints both expiry fields and says "absent" when one
  is missing, so the run settles which one the exchange returns.

## What would change in hop if the callback works

`hop login claude <account>` stops borrowing the live Claude Code login. The
adapter in `internal/provider/claude` gains a `Login` that does what the spike
does: build the URL, open the system browser, catch the callback on a
localhost port, exchange the code, and write the result straight into the
account's slot as `Credentials` (`accessToken`, `refreshToken`, `expiresAt`,
`refreshTokenExpiresAt` when present, `scopes` from the response or the
requested list). The stash, logout, restore, and `HOP_CLAUDE_LIVE_LOGIN`
approval gate in `internal/cli/login.go` and the darwin credential reader go
away for enrollment, and the README loses its "never starts a Claude browser
login behind your back" caveat because the browser flow no longer touches the
live seat. Refresh keeps working as it does today. Plan B in PLAN.md stays
documented only as history.

If the exchange fails with a Cloudflare block, the change is the same plus a
uTLS transport for the token endpoint, which is the one dependency hop would
rather not carry. That is the decision point the human's run produces.
