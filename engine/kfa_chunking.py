"""Kokoro chunking + boundary trimming, ported from kokoro-fastapi so the
hermetic engine renders the SAME audio as the Docker TTS service.

Board 17 (2026-09-21). The August GPU render came out 6.8 s shorter than the
approved CPU render over a five-minute sample. Measured cause (not ear): the
two paths fed the model DIFFERENT sequences. kokoro-fastapi normalizes the
text, splits it into sentence groups of ~175–250 espeak tokens (hard cap 450),
runs the model once per group, then trims each group's boundary silence to a
50 ms lead-in and a punctuation-scaled tail (410 ms after '.', ×0.9 after '!',
×0.8 after ','). The engine used to hand the whole 500-word chunk to
KPipeline, which cut it at ≤510 phonemes and kept the raw boundary silence
(~0.3 s lead / ~0.8 s tail) — and Kokoro paces a 500-phoneme sequence
differently from a 250-token one, so the speech itself drifted both ways.

This module is that pipeline, step for step:

  smart_split(text)          -> [(chunk_text, pause_secs|None), ...]
  trim_chunk(int16, text)    -> int16   (kokoro-fastapi AudioService.trim_audio)

Sources (remsky/Kokoro-FastAPI, Apache-2.0, image VERSION 0.7.2):
  api/src/services/text_processing/{text_processor,phonemizer,vocabulary}.py
  api/src/services/audio.py (AudioNormalizer + trim_audio)
  api/src/core/config.py (the constants below)
The normalizer is a verbatim copy in kfa_normalizer.py. Token counting uses
espeak-ng through `phonemizer`, exactly as upstream does (NOT misaki — misaki
is what the model reads; espeak only decides where the chunks cut). The
engine bundle ships espeak-ng as the espeakng-loader wheel, so we point
`phonemizer` at it before import.
"""
import math
import os
import re
from typing import Generator, List, Optional, Tuple

import numpy as np

from kfa_normalizer import NormalizationOptions, normalize_text

# --- kokoro-fastapi config.py defaults ---------------------------------------
SAMPLE_RATE = 24000
TARGET_MIN_TOKENS = 175
TARGET_MAX_TOKENS = 250
ABSOLUTE_MAX_TOKENS = 450
GAP_TRIM_MS = 1
DYNAMIC_GAP_TRIM_PADDING_MS = 410
DYNAMIC_GAP_TRIM_PADDING_CHAR_MULTIPLIER = {".": 1, "!": 0.9, "?": 1, ",": 0.8}
ADVANCED_TEXT_NORMALIZATION = True

# --- espeak via the bundled wheel --------------------------------------------
try:  # pragma: no cover - environment plumbing
    import espeakng_loader as _el

    os.environ.setdefault("PHONEMIZER_ESPEAK_LIBRARY", str(_el.get_library_path()))
    os.environ.setdefault("ESPEAK_DATA_PATH", str(_el.get_data_path()))
except Exception:  # noqa: BLE001 — fall back to a system espeak-ng if present
    pass

import phonemizer  # noqa: E402
from phonemizer.backend import EspeakBackend  # noqa: E402

# --- vocabulary.py -----------------------------------------------------------


def _get_vocab():
    _pad = "$"
    _punctuation = ';:,.!?¡¿—…"«»"" '
    _letters = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
    _letters_ipa = "ɑɐɒæɓʙβɔɕçɗɖðʤəɘɚɛɜɝɞɟʄɡɠɢʛɦɧħɥʜɨɪʝɭɬɫɮʟɱɯɰŋɳɲɴøɵɸθœɶʘɹɺɾɻʀʁɽʂʃʈʧʉʊʋⱱʌɣɤʍχʎʏʑʐʒʔʡʕʢǀǁǂǃˈˌːˑʼʴʰʱʲʷˠˤ˞↓↑→↗↘'̩'ᵻ"
    symbols = [_pad] + list(_punctuation) + list(_letters) + list(_letters_ipa)
    return {symbol: i for i, symbol in enumerate(symbols)}


VOCAB = _get_vocab()


def tokenize(phonemes: str) -> List[int]:
    phonemes = phonemes.strip()
    return [i for i in map(VOCAB.get, phonemes) if i is not None]


# --- phonemizer.py -----------------------------------------------------------

_phonemizers: dict = {}
_LANG_MAP = {"a": "en-us", "b": "en-gb"}


class _Espeak:
    def __init__(self, language: str):
        self.backend = EspeakBackend(language=language, preserve_punctuation=True, with_stress=True)
        self.language = language

    def phonemize(self, text: str) -> str:
        ps = self.backend.phonemize([text])
        ps = ps[0] if ps else ""
        ps = ps.replace("kəkˈoːɹoʊ", "kˈoʊkəɹoʊ").replace("kəkˈɔːɹəʊ", "kˈəʊkəɹəʊ")
        ps = ps.replace("ʲ", "j").replace("r", "ɹ").replace("x", "k").replace("ɬ", "l")
        ps = re.sub(r"(?<=[a-zɹː])(?=hˈʌndɹɪd)", " ", ps)
        ps = re.sub(r' z(?=[;:,.!?¡¿—…"«»"" ]|$)', "z", ps)
        if self.language == "en-us":
            ps = re.sub(r"(?<=nˈaɪn)ti(?!ː)", "di", ps)
        return ps.strip()


def phonemize(text: str, language: str = "a") -> str:
    text = text.strip()
    if language not in _phonemizers:
        _phonemizers[language] = _Espeak(_LANG_MAP.get(language, "en-us"))
    return _phonemizers[language].phonemize(text).strip()


# --- text_processor.py -------------------------------------------------------

CUSTOM_PHONEMES = re.compile(r"(\[[^\[\]]*?\]\(\/[^\/\(\)]*?\/\))")
PAUSE_TAG_PATTERN = re.compile(r"\[pause:(\d+(?:\.\d+)?)s\]", re.IGNORECASE)


def process_text_chunk(text: str, language: str = "a") -> List[int]:
    text = text.strip()
    if not text:
        return []
    return tokenize(phonemize(text, language).strip())


def get_sentence_info(text: str, lang_code: str = "a") -> List[Tuple[str, List[int], int]]:
    is_chinese = lang_code.startswith("z") or re.search(r"[一-鿿]", text)
    if is_chinese:
        sentences = re.split(r"([，。！？；])+", text)
    else:
        sentences = re.split(r"([.!?;:])(?=\s|$)", text)
    results = []
    for i in range(0, len(sentences), 2):
        sentence = sentences[i].strip()
        punct = sentences[i + 1] if i + 1 < len(sentences) else ""
        if not sentence:
            continue
        full = (sentence + punct).strip()
        if not full:
            continue
        # upstream counts with the DEFAULT language ("a") regardless of voice
        tokens = process_text_chunk(full)
        results.append((full, tokens, len(tokens)))
    return results


def smart_split(
    text: str,
    max_tokens: int = ABSOLUTE_MAX_TOKENS,
    lang_code: str = "a",
    normalization_options: Optional[NormalizationOptions] = None,
) -> Generator[Tuple[str, Optional[float]], None, None]:
    """Yield (chunk_text, None) for speech chunks and ("", secs) for [pause:Ns] tags.

    Same algorithm and thresholds as kokoro-fastapi's smart_split (sync, and
    without the token lists — the model re-phonemizes the text itself).
    """
    if normalization_options is None:
        normalization_options = NormalizationOptions()
    parts = PAUSE_TAG_PATTERN.split(text)
    part_idx = 0
    while part_idx < len(parts):
        text_part_raw = parts[part_idx]
        part_idx += 1
        if text_part_raw and text_part_raw.strip():
            text_part_raw = text_part_raw.strip()
            processed_text = text_part_raw
            if ADVANCED_TEXT_NORMALIZATION and normalization_options.normalize:
                if lang_code in ["a", "b", "en-us", "en-gb"]:
                    pieces = CUSTOM_PHONEMES.split(processed_text)
                    for index in range(0, len(pieces), 2):
                        pieces[index] = normalize_text(pieces[index], normalization_options)
                    processed_text = "".join(pieces).strip()

            sentences = get_sentence_info(processed_text, lang_code=lang_code)
            current_chunk: List[str] = []
            current_count = 0
            for sentence, _tokens, count in sentences:
                if count > max_tokens:
                    if current_chunk:
                        yield " ".join(current_chunk).strip(), None
                        current_chunk = []
                        current_count = 0
                    clauses = re.split(r"([,])", sentence)
                    clause_chunk: List[str] = []
                    clause_count = 0
                    for j in range(0, len(clauses), 2):
                        clause = clauses[j].strip()
                        comma = clauses[j + 1] if j + 1 < len(clauses) else ""
                        if not clause:
                            continue
                        full_clause = clause + comma
                        ccount = len(process_text_chunk(full_clause))
                        if clause_count + ccount <= max_tokens and clause_count + ccount <= TARGET_MAX_TOKENS:
                            clause_chunk.append(full_clause)
                            clause_count += ccount
                        else:
                            if clause_chunk:
                                yield " ".join(clause_chunk).strip(), None
                            clause_chunk = [full_clause]
                            clause_count = ccount
                    if clause_chunk:
                        yield " ".join(clause_chunk).strip(), None
                elif current_count >= TARGET_MIN_TOKENS and current_count + count > TARGET_MAX_TOKENS:
                    yield " ".join(current_chunk).strip(), None
                    current_chunk = [sentence]
                    current_count = count
                elif current_count + count <= TARGET_MAX_TOKENS:
                    current_chunk.append(sentence)
                    current_count += count
                elif current_count + count <= max_tokens and current_count < TARGET_MIN_TOKENS:
                    current_chunk.append(sentence)
                    current_count += count
                else:
                    if current_chunk:
                        yield " ".join(current_chunk).strip(), None
                    current_chunk = [sentence]
                    current_count = count
            if current_chunk:
                yield " ".join(current_chunk).strip(), None

        if part_idx < len(parts):
            duration_str = parts[part_idx]
            if re.fullmatch(r"\d+(?:\.\d+)?", duration_str):
                part_idx += 1
                try:
                    duration = float(duration_str)
                    if duration > 0:
                        yield "", duration
                except (ValueError, TypeError):
                    pass


# --- audio.py: AudioNormalizer.normalize + find_first_last_non_silent + trim_audio


def to_int16(audio: np.ndarray) -> np.ndarray:
    if audio.dtype != np.int16:
        return np.clip(audio * 32767, -32768, 32767).astype(np.int16)
    return audio


_SAMPLES_TO_TRIM = int(GAP_TRIM_MS * SAMPLE_RATE / 1000)
_SAMPLES_TO_PAD_START = int(50 * SAMPLE_RATE / 1000)


def _find_first_last_non_silent(
    audio_data: np.ndarray, chunk_text: str, speed: float, silence_threshold_db: int = -45, is_last_chunk: bool = False
) -> Tuple[int, int]:
    pad_multiplier = 1
    split_character = chunk_text.strip()
    if len(split_character) > 0:
        split_character = split_character[-1]
        if split_character in DYNAMIC_GAP_TRIM_PADDING_CHAR_MULTIPLIER:
            pad_multiplier = DYNAMIC_GAP_TRIM_PADDING_CHAR_MULTIPLIER[split_character]
    if not is_last_chunk:
        samples_to_pad_end = max(
            int((DYNAMIC_GAP_TRIM_PADDING_MS * SAMPLE_RATE * pad_multiplier) / 1000) - _SAMPLES_TO_PAD_START, 0
        )
    else:
        samples_to_pad_end = _SAMPLES_TO_PAD_START
    amplitude_threshold = np.iinfo(audio_data.dtype).max * (10 ** (silence_threshold_db / 20))
    loud = np.abs(audio_data.astype(np.int32)) > amplitude_threshold
    idx = np.flatnonzero(loud)
    if idx.size == 0:
        return 0, len(audio_data)
    non_silent_index_start, non_silent_index_end = int(idx[0]), int(idx[-1])
    return max(non_silent_index_start - _SAMPLES_TO_PAD_START, 0), min(
        non_silent_index_end + math.ceil(samples_to_pad_end / speed), len(audio_data)
    )


def trim_chunk(audio: np.ndarray, chunk_text: str = "", speed: float = 1.0, is_last_chunk: bool = False) -> np.ndarray:
    """kokoro-fastapi's trim_audio for one generated chunk (int16 in, int16 out).

    Note: in the service every speech chunk goes through this with
    is_last_chunk=False (the stream's real last call carries no audio), so the
    final chunk of a request keeps the full punctuation tail too. Callers
    should pass is_last_chunk=False to stay byte-for-byte on the same policy.
    """
    audio = to_int16(audio)
    if len(audio) > (2 * _SAMPLES_TO_TRIM):
        audio = audio[_SAMPLES_TO_TRIM:-_SAMPLES_TO_TRIM]
    start_index, end_index = _find_first_last_non_silent(audio, chunk_text, speed, is_last_chunk=is_last_chunk)
    return audio[start_index:end_index]
