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
import threading
from contextlib import contextmanager
from functools import wraps
from pathlib import Path

# NOTE: flask is imported lazily inside install_auth so the GPU lock + bind
# helpers below are usable/testable without flask present.

LOOPBACK = {"127.0.0.1", "::1", "localhost", ""}


# --- GPU serialization (#132) ------------------------------------------------
# The engine is the single GPU dispatcher: ALL GPU work (STT in one process, TTS
# in another, from any caller — server job queue, Q&A, CLI, LAN) runs one job at
# a time. Flask is multi-threaded and STT/TTS are separate processes, so an
# in-process lock isn't enough; we take an exclusive *file* lock (flock) shared
# by both services. A CPU-only / non-Unix bundle degrades to a per-process lock.
_GPU_LOCK_PATH = Path(
    os.environ.get("ABOOKIFY_GPU_LOCK", Path.home() / ".abookify" / "gpu.lock")
)
_THREAD_LOCK = threading.Lock()

try:
    import fcntl  # Unix only
except ImportError:  # pragma: no cover - Windows
    fcntl = None


@contextmanager
def gpu_lock(label: str = ""):
    """Serialize GPU inference across both engine processes and all callers."""
    # Per-process gate first (cheap, also covers the no-fcntl path).
    with _THREAD_LOCK:
        if fcntl is None:
            yield
            return
        try:
            _GPU_LOCK_PATH.parent.mkdir(parents=True, exist_ok=True)
            fd = os.open(str(_GPU_LOCK_PATH), os.O_CREAT | os.O_RDWR, 0o644)
        except OSError as e:  # lock dir unwritable → degrade to thread lock only
            sys.stderr.write(f"[engine] gpu_lock file unavailable ({e}); thread-only\n")
            yield
            return
        try:
            fcntl.flock(fd, fcntl.LOCK_EX)
            yield
        finally:
            fcntl.flock(fd, fcntl.LOCK_UN)
            os.close(fd)


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
    from flask import request, jsonify  # lazy — keeps _common importable w/o flask

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
