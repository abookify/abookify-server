# BookNLP cast-of-characters service (EXPERIMENTAL, opt-in via the `booknlp`
# compose profile). BookNLP is an unmaintained 2021 artifact (MIT) that
# declares unbounded spacy>=3; on py3.7 that otherwise resolves to a spaCy
# whose backend needs cython>=3.1 (py>=3.8) and fails. The pins below are the
# hard-won recipe from the validated prototype (/home/pj/booknlp-prototype).
FROM python:3.7

RUN pip install --no-cache-dir --only-binary=:all: \
      'spacy>=3.0,<3.3' \
      'transformers>=4.11,<4.25' \
      'torch==1.13.1' \
      flask

RUN python -m spacy download en_core_web_sm

RUN pip install --no-cache-dir --no-build-isolation booknlp

# Pre-download BookNLP's own models at build time so runs are offline/fast.
RUN python -c "from booknlp.booknlp import BookNLP; BookNLP('en', {'pipeline':'entity,quote,coref','model':'small'})" || true

WORKDIR /app
COPY cast_pipeline.py server.py /app/

EXPOSE 5300
CMD ["python", "server.py"]
