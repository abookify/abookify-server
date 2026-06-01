"""BookNLP cast-of-characters HTTP service (EXPERIMENTAL).

Mirrors the whisper/kokoro service shape: a small Flask app the Go server
calls over HTTP. POST /extract {text} runs BookNLP on the text and returns a
book-agnostic named cast (see cast_pipeline.py). Opt-in via the `booknlp`
docker-compose profile — the image is large and the feature is experimental.
"""
import os
import tempfile
import traceback
from flask import Flask, request, jsonify

from booknlp.booknlp import BookNLP

from cast_pipeline import extract_cast

app = Flask(__name__)

MODEL = os.environ.get("BOOKNLP_MODEL", "small")  # "small" | "big"

print(f"Loading BookNLP (model={MODEL})...")
_model = BookNLP("en", {"pipeline": "entity,quote,coref", "model": MODEL})
print("BookNLP loaded.")


@app.route("/health")
def health():
    return jsonify({"status": "ok", "model": MODEL})


@app.route("/extract", methods=["POST"])
def extract():
    body = request.get_json(silent=True) or {}
    text = body.get("text", "")
    if not text.strip():
        return jsonify({"error": "empty text"}), 400
    try:
        with tempfile.TemporaryDirectory() as tmp:
            in_path = os.path.join(tmp, "book.txt")
            with open(in_path, "w", encoding="utf-8") as f:
                f.write(text)
            out_dir = os.path.join(tmp, "out")
            os.makedirs(out_dir, exist_ok=True)
            book_id = "book"
            _model.process(in_path, out_dir, book_id)
            cast = extract_cast(os.path.join(out_dir, book_id + ".book"))
        return jsonify({"characters": cast})
    except Exception as e:
        traceback.print_exc()
        return jsonify({"error": str(e)}), 500


if __name__ == "__main__":
    app.run(host="0.0.0.0", port=int(os.environ.get("PORT", "5300")))
