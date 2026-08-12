#!/usr/bin/env python3
"""The model server the receiver's Sidecar engine talks to.

    python3 serve.py --weights models/symbol-classifier/v1 --port 9800

Three endpoints, and the shape of them is the whole design:

* ``GET  /v1/health``   — what is loaded, on what device. Probed at startup by the Go side, which refuses
  to start when a configured sidecar does not answer, so a deployment cannot quietly run the baseline
  while an operator reads the numbers as a model's work.

* ``POST /v1/classify`` — the endpoint that matters. The body is a block of fixed-size cell records the
  *Go side* sampled, and the response is per-cell posteriors. Patches are sampled in Go rather than here
  on purpose: the homography and the lens model live there, they are already correct, and shipping a
  megabyte image so Python can redo geometry badly would be slower and worse. What crosses the wire is
  already features.

* ``POST /v1/enhance``  — an image in, the same image out, at identical dimensions. Present because the
  Go contract defines it and the path should be exercisable, but with no restoration weights it is the
  identity, and it says so in its model version. That is deliberate: the whole pipeline runs end to end
  from day one while the enhancement stage's measured contribution is exactly zero, rather than an
  unfalsifiable claim.

Why the classifier and not an enhancer is the model worth serving: measured on real captures, failures
concentrate in payload_crc at a near-perfect finder score — geometry is right, individual cell reads are
wrong. That is a per-cell classification problem. An enhancement network would attack no_quad, which at
one or two pixels a cell is missing samples rather than missing contrast, and no network invents samples
that were never captured.
"""

import argparse
import io
import json
import pathlib
import struct
import time

import numpy as np
import torch
import uvicorn
from fastapi import FastAPI, Request, Response
from PIL import Image

from model import CLASSES, PATCH_SIDE, SymbolClassifier

PATCH_BYTES = PATCH_SIDE * PATCH_SIDE * 3
# patch + black[3]f32 + white[3]f32 + frame u32 + label u8. Mirrors shared/cellpatch.RecordBytes; the Go
# side sends whole records including the unused frame and label so one layout serves both directions.
RECORD_BYTES = PATCH_BYTES + 24 + 4 + 1

app = FastAPI()
state: dict = {}


@app.get("/v1/health")
def health() -> dict:
    return {
        "model_version": state["version"],
        "device": state["device"],
        "classes": CLASSES,
        "patch_side": PATCH_SIDE,
        "record_bytes": RECORD_BYTES,
        "enhance": "identity",
    }


@app.post("/v1/classify")
async def classify(request: Request) -> Response:
    """Cell records in, posteriors out.

    The response is float32 (N, 8) little-endian with no framing, because the caller knows N from what it
    sent and a JSON array of a hundred thousand floats costs more to parse than the inference costs to run.
    """
    raw = await request.body()
    if not raw or len(raw) % RECORD_BYTES != 0:
        return Response(
            content=json.dumps({"error": f"body must be a multiple of {RECORD_BYTES} bytes"}),
            media_type="application/json",
            status_code=400,
        )

    n = len(raw) // RECORD_BYTES
    started = time.perf_counter()

    block = np.frombuffer(raw, dtype=np.uint8).reshape(n, RECORD_BYTES)
    patches = block[:, :PATCH_BYTES].reshape(n, PATCH_SIDE, PATCH_SIDE, 3)
    patches = np.ascontiguousarray(patches.transpose(0, 3, 1, 2))
    reference = block[:, PATCH_BYTES : PATCH_BYTES + 24].copy().view(np.float32).reshape(n, 6)

    device = state["device"]
    X = torch.from_numpy(patches).to(device).float().div_(255.0)
    R = torch.from_numpy(reference).to(device).float().div_(255.0).clamp_(0.0, 1.0)

    model = state["model"]
    out = torch.empty((n, CLASSES), dtype=torch.float32, device=device)
    batch = 65536
    with torch.no_grad():
        for i in range(0, n, batch):
            out[i : i + batch] = torch.softmax(model(X[i : i + batch], R[i : i + batch]), dim=1)

    body = out.to("cpu").numpy().astype("<f4").tobytes()
    ms = (time.perf_counter() - started) * 1000.0
    return Response(
        content=body,
        media_type="application/octet-stream",
        headers={
            "X-Otp-Model-Version": state["version"],
            "X-Otp-Stage": "classify",
            "X-Otp-Cells": str(n),
            "X-Otp-Ms": f"{ms:.2f}",
        },
    )


@app.post("/v1/enhance")
async def enhance(request: Request) -> Response:
    """Identity, until restoration weights exist.

    Returned at identical dimensions because the Go side checks that and refuses a rescaled response: a
    service that resized would silently invalidate every geometry the decoder then computed, and the
    symptom would be frames failing for no visible reason.
    """
    raw = await request.body()
    img = Image.open(io.BytesIO(raw))
    buf = io.BytesIO()
    img.save(buf, format="PNG")
    return Response(
        content=buf.getvalue(),
        media_type="image/png",
        headers={"X-Otp-Model-Version": "identity", "X-Otp-Stage": "enhance"},
    )


def main() -> None:
    ap = argparse.ArgumentParser()
    ap.add_argument("--weights", required=True, type=pathlib.Path)
    ap.add_argument("--host", default="0.0.0.0")
    ap.add_argument("--port", type=int, default=9800)
    args = ap.parse_args()

    device = "cuda" if torch.cuda.is_available() else "cpu"
    model = SymbolClassifier().to(device)
    model.load_state_dict(torch.load(args.weights / "weights.pt", map_location=device))
    model.eval()

    # The version is the directory name plus what the training run reported, so a recovery in the
    # receiver's log names the exact weights and the accuracy they were measured at.
    version = args.weights.name
    report_path = args.weights / "report.json"
    if report_path.exists():
        report = json.loads(report_path.read_text())
        version = f"{args.weights.name}/acc{report.get('model_accuracy', 0):.4f}"

    state.update(model=model, device=device, version=version)
    print(f"symbol classifier {version} on {device}, listening on {args.host}:{args.port}", flush=True)
    uvicorn.run(app, host=args.host, port=args.port, log_level="warning")


if __name__ == "__main__":
    main()
