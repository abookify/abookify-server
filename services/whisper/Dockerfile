FROM python:3.11-slim-bookworm

RUN apt-get update && apt-get install -y --no-install-recommends \
    ffmpeg \
    && rm -rf /var/lib/apt/lists/*

RUN pip install --no-cache-dir faster-whisper flask

# Pre-download model on build (small for CPU, good quality-speed tradeoff)
RUN python -c "from faster_whisper import WhisperModel; WhisperModel('small', device='cpu', compute_type='int8')"

COPY server.py /app/server.py

WORKDIR /app

EXPOSE 5200

CMD ["python", "server.py"]
