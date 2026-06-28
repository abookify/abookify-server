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

# --- engine variant: cpu (default, every platform) vs cuda (NVIDIA GPU only) ---
# Override with ABOOKIFY_ENGINE_VARIANT=cpu|cuda. Auto: cuda iff the platform is
# CUDA-capable AND an NVIDIA GPU is present. This is the #57 split: Macs / CPU
# boxes never pull torch+CUDA (~2.5 GB), they get the small CPU stack.
if [ -n "${ABOOKIFY_ENGINE_VARIANT:-}" ]; then
  VARIANT="$ABOOKIFY_ENGINE_VARIANT"
elif [ "$GPU_CAPABLE" = 1 ] && command -v nvidia-smi >/dev/null 2>&1 && nvidia-smi -L 2>/dev/null | grep -q GPU; then
  VARIANT="cuda"
else
  VARIANT="cpu"
fi
[ "$GPU_CAPABLE" = 1 ] || VARIANT="cpu"   # cuda wheels are linux/x86_64 only

echo "=== abookify engine build ==="
echo "  bundle : $BUNDLE"
echo "  python : $PY_VERSION (pbs $PBS_TAG, $TRIPLE)"
echo "  variant: $VARIANT $([ "$VARIANT" = cpu ] && echo '(CPU wheels — no torch+CUDA)' || echo '(NVIDIA GPU — torch+CUDA + cuBLAS/cuDNN)')"

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

# 2a. torch FIRST, variant-pinned, so kokoro doesn't drag in the default (CUDA)
#     torch. cpu variant uses the CPU wheel index on Linux/Windows; macOS's
#     default wheel is already CPU/MPS so it needs no special index.
echo "--- installing torch ($VARIANT)"
if [ "$VARIANT" = "cuda" ]; then
  "$PY" -m pip install "torch==2.5.1"                                  # default index = cu124
elif [ "$OS" = "Darwin" ]; then
  "$PY" -m pip install "torch==2.5.1"                                  # mac default = CPU/MPS
else
  "$PY" -m pip install "torch==2.5.1" --index-url https://download.pytorch.org/whl/cpu
fi

# 2b. base deps (torch now satisfied → kokoro won't pull a different one).
echo "--- installing base requirements"
"$PY" -m pip install -r "$HERE/requirements-base.txt"

# 2c. CUDA extras for CTranslate2 (cuBLAS/cuDNN wheels) — GPU builds only.
if [ "$VARIANT" = "cuda" ]; then
  echo "--- installing CUDA extras (cuBLAS/cuDNN for CTranslate2)"
  "$PY" -m pip install -r "$HERE/requirements-cuda.txt"
fi

# --- 3. copy engine code -------------------------------------------------------
echo "--- staging engine code"
mkdir -p "$BUNDLE/engine"
cp "$HERE"/stt_server.py "$HERE"/tts_server.py "$HERE"/_common.py "$HERE"/launch.py "$BUNDLE/engine/"
echo "$VARIANT" > "$BUNDLE/VARIANT"   # stamp so the install/first-run flow knows what it built

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
echo "=== build complete ($VARIANT) ==="
du -sh "$BUNDLE" 2>/dev/null | awk '{print "  bundle size: "$1}'
echo "  launch: $BUNDLE/abookify-engine"
echo "  models will download to ~/.abookify/models/ on first use"
