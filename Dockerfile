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
    && rm -rf /var/lib/apt/lists/*

WORKDIR /app

# Cache module downloads
COPY go.mod go.sum* ./
RUN go mod download 2>/dev/null || true

COPY . .

# Default: run the server
CMD ["go", "run", "./cmd/abookify"]
