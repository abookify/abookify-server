"""abookify hermetic engine entrypoint — starts STT (:5200) + TTS (:8880).

The critical hermetic trick lives here: CTranslate2 (faster-whisper's backend)
loads cuDNN/cuBLAS from the *pip* wheels (nvidia-cublas-cu12, nvidia-cudnn-cu12)
rather than any system CUDA. Those .so files sit under site-packages/nvidia/*/lib
and are NOT on the default loader path, so we prepend them to LD_LIBRARY_PATH
here, before spawning the service processes. With this, the engine reaches the
GPU purely through the user's NVIDIA *driver* — no CUDA toolkit, no container
runtime, no system libs.

Usage:
  python launch.py                 # both services, localhost
  python launch.py --host 0.0.0.0  # expose on LAN (requires ABOOKIFY_ENGINE_TOKEN)
  python launch.py --stt-only
  python launch.py --tts-only
"""
import argparse
import os
import signal
import site
import subprocess
import sys
from pathlib import Path

HERE = Path(__file__).resolve().parent


def nvidia_lib_dirs() -> list[str]:
    """Every site-packages/nvidia/*/lib dir holding the CUDA .so wheels."""
    dirs: list[str] = []
    roots = list(site.getsitepackages()) if hasattr(site, "getsitepackages") else []
    user = site.getusersitepackages() if hasattr(site, "getusersitepackages") else None
    if user:
        roots.append(user)
    for root in roots:
        nv = Path(root) / "nvidia"
        if nv.is_dir():
            for lib in nv.glob("*/lib"):
                dirs.append(str(lib))
    return dirs


def child_env() -> dict:
    env = dict(os.environ)
    libdirs = nvidia_lib_dirs()
    if libdirs:
        existing = env.get("LD_LIBRARY_PATH", "")
        env["LD_LIBRARY_PATH"] = os.pathsep.join(libdirs + ([existing] if existing else []))
        print(f"[engine] LD_LIBRARY_PATH += {len(libdirs)} nvidia wheel lib dir(s)")
    else:
        print("[engine] no nvidia wheel libs found (CPU-only bundle or wheels absent)")
    return env


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--host", default=None, help="bind address (default 127.0.0.1; env ABOOKIFY_ENGINE_HOST)")
    ap.add_argument("--stt-only", action="store_true")
    ap.add_argument("--tts-only", action="store_true")
    args = ap.parse_args()

    if args.host:
        os.environ["ABOOKIFY_ENGINE_HOST"] = args.host

    env = child_env()
    py = sys.executable

    procs: list[subprocess.Popen] = []
    if not args.tts_only:
        procs.append(subprocess.Popen([py, str(HERE / "stt_server.py")], env=env))
    if not args.stt_only:
        procs.append(subprocess.Popen([py, str(HERE / "tts_server.py")], env=env))

    def shutdown(*_):
        for p in procs:
            if p.poll() is None:
                p.terminate()
        sys.exit(0)

    signal.signal(signal.SIGINT, shutdown)
    signal.signal(signal.SIGTERM, shutdown)

    # If any child exits, bring the whole engine down (lifecycle owner restarts it).
    while True:
        for p in procs:
            ret = p.poll()
            if ret is not None:
                print(f"[engine] child pid {p.pid} exited ({ret}); shutting down engine")
                shutdown()
        try:
            os.waitpid(-1, os.WNOHANG)
        except ChildProcessError:
            shutdown()
        signal.pause()


if __name__ == "__main__":
    main()
