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

## Enabling it (local test instance only)

Set the env var to any secret string and start the server without a relay URL:

```bash
# docker compose: add to your .env (local test box only, no ABOOKIFY_PUBLIC_URL)
ABOOKIFY_DEV_AUTH_TOKEN=dev-screenshots-please

# or native/desktop:
ABOOKIFY_DEV_AUTH_TOKEN=dev-screenshots-please ./abookify
```

Startup logs `⚠ DEV AUTH BYPASS ACTIVE …` when it takes effect.

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
