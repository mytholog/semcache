"""JSON stdin/stdout sidecar for a CrossEncoder reranker.

Request:  {"model": "BAAI/bge-reranker-base", "pairs": [["a", "b"], ...]}
Response: {"scores": [0.12, 0.87, ...]}  # sigmoid probabilities in [0, 1]
"""

from __future__ import annotations

import json
import sys

import numpy as np
import torch
from sentence_transformers import CrossEncoder


def scores(model_name: str, pairs: list[list[str]]) -> list[float]:
    model = CrossEncoder(model_name, activation_fn=torch.nn.Sigmoid())
    raw = model.predict(pairs, show_progress_bar=False)
    arr = np.asarray(raw, dtype=np.float64)
    return [float(x) for x in np.atleast_1d(arr)]


def main() -> None:
    req = json.load(sys.stdin)
    json.dump({"scores": scores(req["model"], req["pairs"])}, sys.stdout)


if __name__ == "__main__":
    main()
