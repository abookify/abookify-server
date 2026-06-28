.PHONY: up down restart logs build test relay relay-down health build-cli access-log access-log-remote css fonts build-server

# Desktop-bundle server binaries (distribution / #56 Tauri shell). Produces a
# STANDALONE, STATIC Go binary per target platform — CGO_ENABLED=0 (pure-Go
# modernc.org/sqlite, no libc dependency), web UI + fonts embedded via
# go:embed, version stamped from git. Output: dist/abookify-<os>-<arch>[.exe].
# The Tauri shell (transcription #56) wraps the host-matching binary; the
# launch/health/setup contract it builds against is in
# ../handoff/server-web.md. Runs in Docker — no host Go toolchain.
#
# Override platforms:  make build-server PLATFORMS="linux/amd64 darwin/arm64"
PLATFORMS ?= linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64
VERSION   ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
build-server:
	@mkdir -p dist
	docker run --rm -v "$$(pwd)":/app -w /app -e VERSION="$(VERSION)" -e PLATFORMS="$(PLATFORMS)" \
		golang:1.24-bookworm sh -c '\
		set -e; \
		for p in $$PLATFORMS; do \
		  os=$${p%/*}; arch=$${p#*/}; \
		  ext=""; [ "$$os" = "windows" ] && ext=".exe"; \
		  out="dist/abookify-$$os-$$arch$$ext"; \
		  echo "building $$out (version $$VERSION)"; \
		  CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -buildvcs=false \
		    -ldflags="-s -w -X main.version=$$VERSION" -o "$$out" ./cmd/abookify; \
		done'
	@echo ""
	@echo "Built static server binaries in dist/ (version $(VERSION)):"
	@ls -lh dist/abookify-* 2>/dev/null | awk '{print "  " $$9 "  " $$5}'
	@echo "Each is self-contained (embedded web UI + fonts, pure-Go SQLite, no CGO)."

# Regenerate the embedded design-system stylesheet (#205 / Phase 1 #144).
# Runs Tailwind v3 standalone via an ephemeral Docker container — no host
# toolchain, consistent with the Go-in-Docker build rule. Output is
# committed (internal/server/static/app.css) so `go build` / go:embed
# stays self-contained and offline. Re-run after changing markup classes,
# tailwind.config.js, or src/app.css. (CI will diff-check this — TODO.)
css:
	docker run --rm -v "$$(pwd)":/app -w /app node:20-bookworm-slim \
		npx --yes tailwindcss@3.4.17 \
		-c ./tailwind.config.js \
		-i ./internal/server/static/src/app.css \
		-o ./internal/server/static/app.css --minify

# One-time: fetch the bundled webfonts and commit them under static/fonts/.
# Bundled locally (no runtime CDN) so the embed works fully offline. Runs
# in Docker; the only repo artifacts are the committed woff2 files.
#  - Inter (UI): variable woff2, all weights.
#  - Fraunces (warm-bookish display serif, #213): variable woff2 with the
#    standard axes (wght 100–900 + opsz), from @fontsource-variable/fraunces.
fonts:
	docker run --rm -v "$$(pwd)":/app -w /app debian:bookworm-slim sh -c '\
		apt-get update -qq && apt-get install -y -qq curl unzip >/dev/null && \
		mkdir -p internal/server/static/fonts && \
		curl -fsSL -o /tmp/inter.zip https://github.com/rsms/inter/releases/download/v4.1/Inter-4.1.zip && \
		unzip -o -q /tmp/inter.zip web/InterVariable.woff2 -d /tmp/inter && \
		cp /tmp/inter/web/InterVariable.woff2 internal/server/static/fonts/InterVariable.woff2 && \
		curl -fsSL -o internal/server/static/fonts/Fraunces-standard.woff2 https://cdn.jsdelivr.net/npm/@fontsource-variable/fraunces@5.2.9/files/fraunces-latin-standard-normal.woff2 && \
		echo "fonts: bundled $$(du -h internal/server/static/fonts/InterVariable.woff2 | cut -f1) InterVariable.woff2 + $$(du -h internal/server/static/fonts/Fraunces-standard.woff2 | cut -f1) Fraunces-standard.woff2"'

up:
	docker compose up -d --build

down:
	docker compose down

restart:
	docker compose restart

logs:
	docker compose logs -f --tail=100

build:
	docker run --rm -v "$$(pwd)":/app -w /app golang:1.24-bookworm go build -buildvcs=false ./cmd/abookify

test:
	docker run --rm -v "$$(pwd)":/app -w /app golang:1.24-bookworm go test -buildvcs=false ./internal/... -v

# Start everything + nullbore tunnel (reads engineering/relay/.env)
relay:
	docker compose up -d --build
	../relay/start.sh

relay-down:
	docker compose --profile relay down

# Build CLI tools as static binaries (copy to GPU box via scp)
build-cli:
	docker run --rm -v "$$(pwd)":/app -w /app golang:1.24-bookworm \
		sh -c 'CGO_ENABLED=0 go build -buildvcs=false -ldflags="-s -w" -o bin/stt-cli ./cmd/stt-cli && \
		       CGO_ENABLED=0 go build -buildvcs=false -ldflags="-s -w" -o bin/tts-cli ./cmd/tts-cli'
	@echo "Built: bin/stt-cli  bin/tts-cli"
	@echo "Copy to GPU box:  scp bin/stt-cli bin/tts-cli user@gpu-host:~/"
	@echo ""
	@echo "Usage on GPU box (with Whisper/Kokoro running):"
	@echo "  ./stt-cli --audio book.mp3 --detect-chapters --output result.json"
	@echo "  ./tts-cli --text chapter.txt --voice af_heart --output chapter.mp3"
	@echo "  ./tts-cli --voices  (list available voices)"

health:
	@curl -s http://localhost:7654/api/health && echo
	@curl -s http://localhost:8880/v1/models > /dev/null && echo "kokoro: ok" || echo "kokoro: down"
	@curl -s http://localhost:5200/health > /dev/null && echo "whisper: ok" || echo "whisper: down"

# Show every HTTP request in the access log. WS pings + static asset
# fetches are already filtered server-side, so this is signal-only.
access-log:
	@docker logs server-server-1 2>&1 | grep ' ACCESS ' | tail -100

# Just the requests whose original client IP is NOT localhost or the
# local LAN. With the nullbore tunnel in front, every request shows up
# with the tunnel container as the immediate peer, so the meaningful
# field is fwd= (X-Forwarded-For). Catches outside-the-house traffic
# you didn't initiate.
access-log-remote:
	@docker logs server-server-1 2>&1 | grep ' ACCESS ' \
	  | grep -v 'fwd=127\.' | grep -v 'fwd=192\.168\.' \
	  | grep -v 'fwd=10\.' | grep -v 'fwd=172\.\(1[6-9]\|2[0-9]\|3[01]\)\.' \
	  | grep -v 'fwd=-' \
	  | tail -100
