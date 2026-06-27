#!/usr/bin/env bash
# Build the abookify hermetic ML engine bundle.
#
# Produces a self-contained directory with:
#   <bundle>/python/         python-build-standalone runtime (no system Python)
#   <bundle>/python/.../site-packages/  faster-whisper, kokoro, torch, CUDA wheels
#   <bundle>/engine/         our service code (stt_server, tts_server, launch, ...)
#   <bundle>/abookify-engine launcher wrapper
#
# Models are NOT bundled — they download to ~/.abookify/models/ on first use.
# CUDA reaches the GPU via the user's NVIDIA driver only (CUDA libs are pip wheels).
#
# Usage: ./build.sh [bundle_dir]   (default: ./dist/engine)
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BUNDLE="${1:-$HERE/dist/engine}"
PY_VERSION="${ABOOKIFY_PY_VERSION:-3.11.11}"
PBS_TAG="${ABOOKIFY_PBS_TAG:-20250115}"

# --- platform → python-build-standalone triple ---------------------------------
OS="$(uname -s)"; ARCH="$(uname -m)"
case "$OS-$ARCH" in
  Linux-x86_64)   TRIPLE="x86_64-unknown-linux-gnu" ; GPU_CAPABLE=1 ;;
  Linux-aarch64)  TRIPLE="aarch64-unknown-linux-gnu"; GPU_CAPABLE=0 ;;
  Darwin-arm64)   TRIPLE="aarch64-apple-darwin"     ; GPU_CAPABLE=0 ;;
  Darwin-x86_64)  TRIPLE="x86_64-apple-darwin"      ; GPU_CAPABLE=0 ;;
  *) echo "unsupported platform: $OS-$ARCH" >&2; exit 1 ;;
esac

PBS_ASSET="cpython-${PY_VERSION}+${PBS_TAG}-${TRIPLE}-install_only.tar.gz"
PBS_URL="https://github.com/astral-sh/python-build-standalone/releases/download/${PBS_TAG}/${PBS_ASSET}"

echo "=== abookify engine build ==="
echo "  bundle : $BUNDLE"
echo "  python : $PY_VERSION (pbs $PBS_TAG, $TRIPLE)"
echo "  gpu    : $([ "$GPU_CAPABLE" = 1 ] && echo 'CUDA wheels included' || echo 'CPU only (no nvidia wheels)')"

mkdir -p "$BUNDLE"
CACHE="$HERE/.build-cache"; mkdir -p "$CACHE"

# --- 1. fetch + extract standalone python --------------------------------------
if [ ! -x "$BUNDLE/python/bin/python3" ]; then
  if [ ! -f "$CACHE/$PBS_ASSET" ]; then
    echo "--- downloading $PBS_ASSET"
    if ! curl -fSL --retry 3 -o "$CACHE/$PBS_ASSET" "$PBS_URL"; then
      echo "ERROR: could not download python-build-standalone from:" >&2
      echo "  $PBS_URL" >&2
      echo "Pin a valid release via ABOOKIFY_PBS_TAG / ABOOKIFY_PY_VERSION." >&2
      exit 1
    fi
  fi
  echo "--- extracting standalone python"
  tar -xzf "$CACHE/$PBS_ASSET" -C "$BUNDLE"   # extracts a top-level python/
fi
PY="$BUNDLE/python/bin/python3"
"$PY" --version

# --- 2. pip install deps -------------------------------------------------------
echo "--- upgrading pip"
"$PY" -m pip install --upgrade pip wheel >/dev/null

# On non-GPU platforms, strip the nvidia-*-cu12 lines (they're linux/x86_64 only).
REQ="$HERE/requirements.txt"
if [ "$GPU_CAPABLE" != 1 ]; then
  REQ="$CACHE/requirements.cpu.txt"
  grep -v '^nvidia-' "$HERE/requirements.txt" > "$REQ"
  echo "--- (CPU build: nvidia wheels stripped)"
fi
echo "--- installing requirements (this is the big step)"
"$PY" -m pip install -r "$REQ"

# --- 3. copy engine code -------------------------------------------------------
echo "--- staging engine code"
mkdir -p "$BUNDLE/engine"
cp "$HERE"/stt_server.py "$HERE"/tts_server.py "$HERE"/_common.py "$HERE"/launch.py "$BUNDLE/engine/"

# --- 4. launcher wrapper -------------------------------------------------------
cat > "$BUNDLE/abookify-engine" <<'WRAP'
#!/usr/bin/env bash
# abookify hermetic engine launcher. Passes args through to launch.py.
#   ./abookify-engine               # both services, localhost
#   ./abookify-engine --host 0.0.0.0  # LAN (needs ABOOKIFY_ENGINE_TOKEN)
#   ./abookify-engine --stt-only | --tts-only
set -euo pipefail
DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
exec "$DIR/python/bin/python3" "$DIR/engine/launch.py" "$@"
WRAP
chmod +x "$BUNDLE/abookify-engine"

echo ""
echo "=== build complete ==="
du -sh "$BUNDLE" 2>/dev/null | awk '{print "  bundle size: "$1}'
echo "  launch: $BUNDLE/abookify-engine"
echo "  models will download to ~/.abookify/models/ on first use"
