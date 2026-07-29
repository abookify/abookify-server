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

Set the env var to any secret string and start the server without a relay URL.
On startup the server **saves the token to its settings DB**, so you only do this
ONCE — every later recreate/reboot loads it back automatically (no need to keep
the env around, though keeping it in `.env` is fine and re-asserts it).

```bash
# docker compose: set it once, then `make server` (or up -d) — it's now persisted
ABOOKIFY_DEV_AUTH_TOKEN=dev-screenshots-please docker compose \
  -f docker-compose.yml -f docker-compose.gpu.yml up -d --no-deps server

# or put it in .env once (also fine — re-asserts the same token every boot):
#   ABOOKIFY_DEV_AUTH_TOKEN=dev-screenshots-please

# native/desktop:
ABOOKIFY_DEV_AUTH_TOKEN=dev-screenshots-please ./abookify
```

Startup logs `⚠ DEV AUTH BYPASS ACTIVE (persisted) …` when it takes effect. After
that first boot, the token is stored — a plain `make server` recreate keeps it.

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
