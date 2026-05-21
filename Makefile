.PHONY: up down restart logs build test relay relay-down health build-cli access-log access-log-remote

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
