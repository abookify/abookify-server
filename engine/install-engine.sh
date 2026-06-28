#!/usr/bin/env bash
# First-run engine install (#57). The desktop installer ships only the Go binary
# + Tauri shell + this engine SOURCE (small: .py + build.sh + requirements). On
# first run we build the host-sized hermetic engine into ~/.abookify/engine:
#   - downloads python-build-standalone for this OS/arch
#   - installs the CPU stack by default, or the CUDA stack iff an NVIDIA GPU is
#     present (build.sh auto-detects; override with ABOOKIFY_ENGINE_VARIANT)
# Models (~1.5–3 GB) still download lazily to ~/.abookify/models on first use.
#
# The shell resolves ~/.abookify/engine/abookify-engine on later launches.
set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
TARGET="${ABOOKIFY_ENGINE_DIR:-$HOME/.abookify/engine}"

echo "Installing the abookify speech engine to: $TARGET"
exec "$HERE/build.sh" "$TARGET"
