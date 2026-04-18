FROM golang:1.24-bookworm

# System deps for media processing (will be needed later for FFmpeg, etc.)
RUN apt-get update && apt-get install -y --no-install-recommends \
    sqlite3 \
    ffmpeg \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /app

# Cache module downloads
COPY go.mod go.sum* ./
RUN go mod download 2>/dev/null || true

COPY . .

# Default: run the server
CMD ["go", "run", "./cmd/abookify"]
