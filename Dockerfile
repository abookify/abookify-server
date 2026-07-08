FROM golang:1.24-bookworm

# System deps for media processing.
#  - ffmpeg: audio probe / waveform / chapter marker reads
#  - sqlite3: DB inspection from inside the container
#  - calibre: provides ebook-convert for MOBI/AZW3/AZW → EPUB at scan time
#    (see internal/library/mobi_convert.go). --no-install-recommends keeps
#    the image lean; the headless CLI still works for mobi→epub.
RUN apt-get update && apt-get install -y --no-install-recommends \
    sqlite3 \
    ffmpeg \
    calibre \
    ca-certificates curl gnupg \
    && rm -rf /var/lib/apt/lists/*

# Docker CLI + compose plugin — the server manages the optional BookNLP cast
# engine's lifecycle (in-UI enable → start; idle auto-stop) by driving the
# `booknlp` compose profile through the mounted docker socket. CLI only, no
# daemon. See internal/server/booknlp_lifecycle.go.
RUN install -m 0755 -d /etc/apt/keyrings \
    && curl -fsSL https://download.docker.com/linux/debian/gpg -o /etc/apt/keyrings/docker.asc \
    && chmod a+r /etc/apt/keyrings/docker.asc \
    && echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/debian bookworm stable" > /etc/apt/sources.list.d/docker.list \
    && apt-get update \
    && apt-get install -y --no-install-recommends docker-ce-cli docker-compose-plugin \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /app

# Cache module downloads
COPY go.mod go.sum* ./
RUN go mod download 2>/dev/null || true

COPY . .

# Default: run the server
CMD ["go", "run", "./cmd/abookify"]
