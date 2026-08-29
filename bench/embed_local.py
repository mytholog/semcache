"""JSON stdin/stdout sidecar for local embedding models.

Request:  {"model": "BAAI/bge-m3", "texts": ["..."]}
Response: {"vectors": [[...], ...]}
"""

from __future__ import annotations

import json
import sys


def embed_fastembed(model: str, texts: list[str]) -> list[list[float]]:
    from fastembed import TextEmbedding

    encoder = TextEmbedding(model_name=model)
    return [vec.tolist() for vec in encoder.embed(texts, batch_size=64)]


def embed_sentence_transformers(model: str, texts: list[str]) -> list[list[float]]:
    from sentence_transformers import SentenceTransformer

    encoder = SentenceTransformer(model)
    vectors = encoder.encode(
        texts,
        batch_size=32,
        normalize_embeddings=True,
        show_progress_bar=False,
    )
    return [vec.tolist() for vec in vectors]


def embed(model: str, texts: list[str]) -> list[list[float]]:
    try:
        return embed_fastembed(model, texts)
    except Exception as fastembed_err:
        try:
            return embed_sentence_transformers(model, texts)
        except Exception as st_err:
            raise RuntimeError(
                f"fastembed failed ({fastembed_err}); sentence-transformers failed ({st_err})"
            ) from st_err


def main() -> None:
    req = json.load(sys.stdin)
    model = req["model"]
    texts = req["texts"]
    vectors = embed(model, texts)
    json.dump({"vectors": vectors}, sys.stdout)


if __name__ == "__main__":
    main()
