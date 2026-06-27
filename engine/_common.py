"""Shared bind-address + bearer-auth helpers for the engine HTTP services.

Security posture (PJ, #55): the engine is NEVER an unauthenticated network
service by default.
  - Default bind is 127.0.0.1 (loopback only) — local server + CLIs only.
  - To expose on the LAN, set ABOOKIFY_ENGINE_HOST=0.0.0.0 (or pass --host).
  - When bound to a non-loopback address, a bearer token is REQUIRED:
    set ABOOKIFY_ENGINE_TOKEN=<secret>. Refuse to start otherwise.
  - When a token is set, every request except /health must carry
    `Authorization: Bearer <token>`.
"""
import os
import sys
from functools import wraps

from flask import request, jsonify

LOOPBACK = {"127.0.0.1", "::1", "localhost", ""}


def resolve_host(cli_host: str | None = None) -> str:
    return cli_host or os.environ.get("ABOOKIFY_ENGINE_HOST", "127.0.0.1")


def engine_token() -> str | None:
    tok = os.environ.get("ABOOKIFY_ENGINE_TOKEN", "").strip()
    return tok or None


def enforce_bind_policy(host: str, service: str):
    """Abort startup if exposed on the network without a token."""
    if host not in LOOPBACK and not engine_token():
        sys.stderr.write(
            f"[{service}] REFUSING to bind {host} without ABOOKIFY_ENGINE_TOKEN set.\n"
            f"[{service}] Set a bearer token to expose on the network, or bind 127.0.0.1.\n"
        )
        sys.exit(2)


def install_auth(app, service: str):
    """If a token is configured, require Bearer auth on all routes but /health."""
    tok = engine_token()
    if not tok:
        return

    @app.before_request
    def _check():  # noqa: ANN202
        if request.path == "/health":
            return None
        auth = request.headers.get("Authorization", "")
        if auth.startswith("Bearer ") and auth[7:].strip() == tok:
            return None
        return jsonify({"error": "unauthorized"}), 401

    print(f"[{service}] bearer auth ENABLED")
