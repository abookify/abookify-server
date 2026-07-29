# Local development auth bypass (`ABOOKIFY_DEV_AUTH_TOKEN`)

A **local-development-only** way for a test agent (e.g. the mobile lane capturing
store screenshots) to reach auth-gated surfaces without real credentials.

This is a deliberate dev affordance, **not** a production path and **not** a
backdoor:

- **Off by default.** Nothing changes unless you set `ABOOKIFY_DEV_AUTH_TOKEN`.
- **Cannot be enabled on a real install.** If the instance is network-exposed via
  the relay (`ABOOKIFY_PUBLIC_URL` is set), the token is **ignored** and a
  `SECURITY:` line is logged at startup. It only activates on a local, non-relay
  instance.
- **Does not weaken production auth.** Normal login still works for everyone; this
  only adds one accepted token, constant-time compared, on a local box.
- **Durable across container recreates.** The token is persisted in the settings
  DB (the data-dir volume), not container env — so a routine `docker compose up`
  that recreates the server no longer wipes it. **Set it once; it survives.**

## Enabling it (local test instance only) — set once, it persists

The base `docker-compose.yml` passes `ABOOKIFY_DEV_AUTH_TOKEN` through to the
server (`${ABOOKIFY_DEV_AUTH_TOKEN:-}`, empty by default), so the durable way to
enable it is a **one line in the host-local `.env`** — no
`docker-compose.override.yml` needed (that only auto-loads on a bare `docker
compose up`, which is also the call that strips whisper's GPU, so avoid it):

```bash
# append once to engineering/server/.env (gitignored, host-local) — do NOT clobber
# existing creds; append the line:
#   ABOOKIFY_DEV_AUTH_TOKEN=dev-screenshots-please
```

Then a plain **`make server`** picks it up. On startup the server **saves the
token to its settings DB**, so from then on it's durable: a later recreate that
doesn't re-supply the env still keeps the bypass (the DB copy wins). You only
seed it once.

```bash
# native/desktop:
ABOOKIFY_DEV_AUTH_TOKEN=dev-screenshots-please ./abookify
```

Startup logs `⚠ DEV AUTH BYPASS ACTIVE (persisted) …` when it takes effect. After
that first boot, the token is stored — a plain `make server` recreate keeps it,
and you can drop the `.env` line if you like (the DB copy persists it).

**To rotate/disable:** set a new `ABOOKIFY_DEV_AUTH_TOKEN` and reboot (overwrites
the stored one), or clear the `dev_auth_token` setting. On any relay-exposed
install the stored token is ignored and a `SECURITY:` line is logged instead.

## How the mobile lane uses it

The token is accepted anywhere a normal session token is (see
`tokenFromRequest`): a cookie, a bearer header, or a query param. Pick whichever
fits the client.

**API / bearer (programmatic):**

```bash
curl -H "Authorization: Bearer dev-screenshots-please" http://localhost:7654/api/works
```

**Headless browser (so the web UI login gate never appears):** set the session
cookie to the token before navigating, e.g. with Playwright:

```js
await ctx.addCookies([{
  name: 'abookify_session', value: 'dev-screenshots-please',
  domain: 'localhost', path: '/',
}]);
// /api/auth/status now returns authenticated:true, and every gated surface loads.
```

**Media / WebSocket (tags that can't set headers):** append
`?access_token=dev-screenshots-please` to the URL.

When you're done, unset the env var and restart — the bypass is gone.
