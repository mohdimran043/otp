#!/usr/bin/env python3
"""Train the optical symbol classifier and score it against the decoder's own rule.

    python3 train.py --data /path/to/ds --out models/symbol-classifier/v1 --epochs 8

The split is by *frame*, not by cell. Cells from one frame share its exposure, blur and geometry, so a
random cell split scores the model partly on frames it has already seen and the number comes out
flattering. Frames are held out whole.

Every accuracy is reported beside the baseline's on the same records, and separately on the subset of
cells the baseline found ambiguous. That last figure is the one that matters: the baseline already reads
the easy cells, so a model earns its place only where the baseline was unsure.
"""

import argparse
import json
import pathlib
import time

import numpy as np
import torch
import torch.nn as nn

from model import CLASSES, PATCH_SIDE, SymbolClassifier, baseline_predict

PATCH_BYTES = PATCH_SIDE * PATCH_SIDE * 3
RECORD_BYTES = PATCH_BYTES + 6 * 4 + 4 + 1


def load(data_dir: pathlib.Path):
    """Reads the fixed-layout record file the Go exporter wrote."""
    meta = json.loads((data_dir / "cells.json").read_text())
    if meta["record_bytes"] != RECORD_BYTES:
        raise SystemExit(
            f"record layout mismatch: file says {meta['record_bytes']}, this script expects {RECORD_BYTES}"
        )

    raw = np.fromfile(data_dir / "cells.bin", dtype=np.uint8)
    n = raw.size // RECORD_BYTES
    if n * RECORD_BYTES != raw.size:
        raise SystemExit("record file is not a whole number of records")
    raw = raw.reshape(n, RECORD_BYTES)

    patches = raw[:, :PATCH_BYTES].reshape(n, PATCH_SIDE, PATCH_SIDE, 3)
    # Go wrote channel-last; torch convolutions want channel-first.
    patches = np.ascontiguousarray(patches.transpose(0, 3, 1, 2))

    reference = raw[:, PATCH_BYTES : PATCH_BYTES + 24].copy().view(np.float32).reshape(n, 6)
    frames = raw[:, PATCH_BYTES + 24 : PATCH_BYTES + 28].copy().view(np.uint32).reshape(n)
    labels = raw[:, -1]

    return patches, reference, frames, labels, meta


def split_by_frame(frames: np.ndarray, holdout: float, seed: int = 11):
    unique = np.unique(frames)
    rng = np.random.default_rng(seed)
    rng.shuffle(unique)
    cut = max(1, int(len(unique) * holdout))
    test_frames = set(unique[:cut].tolist())
    is_test = np.fromiter((f in test_frames for f in frames), dtype=bool, count=len(frames))
    return ~is_test, is_test, len(unique) - cut, cut


def main() -> None:
    ap = argparse.ArgumentParser()
    ap.add_argument("--data", required=True, type=pathlib.Path)
    ap.add_argument("--out", required=True, type=pathlib.Path)
    ap.add_argument("--epochs", type=int, default=8)
    ap.add_argument("--batch", type=int, default=4096)
    ap.add_argument("--lr", type=float, default=2e-3)
    ap.add_argument("--holdout", type=float, default=0.2)
    args = ap.parse_args()

    device = "cuda" if torch.cuda.is_available() else "cpu"
    print(f"device: {device}", flush=True)
    if device == "cuda":
        print(f"gpu: {torch.cuda.get_device_name(0)}", flush=True)

    patches, reference, frames, labels, meta = load(args.data)
    train_mask, test_mask, n_train_frames, n_test_frames = split_by_frame(frames, args.holdout)
    print(
        f"{len(labels):,} cells from {len(np.unique(frames))} frames; "
        f"{train_mask.sum():,} train ({n_train_frames} frames) / "
        f"{test_mask.sum():,} test ({n_test_frames} frames)",
        flush=True,
    )

    # Scaled to 0..1 once, on the GPU, rather than per batch. The whole set is a few hundred megabytes,
    # which fits comfortably on a 24 GB card and removes the host-to-device copy from the inner loop.
    X = torch.from_numpy(patches).to(device).float().div_(255.0)
    R = torch.from_numpy(reference).to(device).float().div_(255.0).clamp_(0.0, 1.0)
    Y = torch.from_numpy(labels).to(device).long()

    tr = torch.from_numpy(train_mask).to(device)
    te = torch.from_numpy(test_mask).to(device)
    Xtr, Rtr, Ytr = X[tr], R[tr], Y[tr]
    Xte, Rte, Yte = X[te], R[te], Y[te]

    # The baseline on the held-out set, computed first so training cannot be tuned against it.
    with torch.no_grad():
        base_test = baseline_predict(Xte, Rte)
        base_acc = (base_test == Yte).float().mean().item()
        base_wrong = base_test != Yte
    print(f"baseline (decoder's rule) held-out accuracy: {base_acc*100:.3f}%", flush=True)
    print(f"baseline errors in held-out set: {int(base_wrong.sum()):,}", flush=True)

    model = SymbolClassifier().to(device)
    params = sum(p.numel() for p in model.parameters())
    print(f"model parameters: {params:,}", flush=True)

    opt = torch.optim.AdamW(model.parameters(), lr=args.lr, weight_decay=1e-4)
    sched = torch.optim.lr_scheduler.OneCycleLR(
        opt, max_lr=args.lr, total_steps=args.epochs * max(1, len(Ytr) // args.batch + 1)
    )
    loss_fn = nn.CrossEntropyLoss()

    started = time.time()
    for epoch in range(args.epochs):
        model.train()
        perm = torch.randperm(len(Ytr), device=device)
        total_loss, seen = 0.0, 0
        for i in range(0, len(perm), args.batch):
            idx = perm[i : i + args.batch]
            opt.zero_grad(set_to_none=True)
            out = model(Xtr[idx], Rtr[idx])
            loss = loss_fn(out, Ytr[idx])
            loss.backward()
            opt.step()
            sched.step()
            total_loss += loss.item() * len(idx)
            seen += len(idx)

        model.eval()
        with torch.no_grad():
            pred = torch.empty_like(Yte)
            for i in range(0, len(Yte), args.batch):
                pred[i : i + args.batch] = model(Xte[i : i + args.batch], Rte[i : i + args.batch]).argmax(1)
            acc = (pred == Yte).float().mean().item()
        print(
            f"epoch {epoch+1}/{args.epochs}  loss {total_loss/max(seen,1):.4f}  "
            f"held-out {acc*100:.3f}%  (baseline {base_acc*100:.3f}%)",
            flush=True,
        )

    elapsed = time.time() - started

    # Final accounting, including the only figure that really matters: what the model does on the cells
    # the baseline got wrong, and — just as important — how many it breaks that the baseline got right.
    model.eval()
    with torch.no_grad():
        pred = torch.empty_like(Yte)
        for i in range(0, len(Yte), args.batch):
            pred[i : i + args.batch] = model(Xte[i : i + args.batch], Rte[i : i + args.batch]).argmax(1)

    model_acc = (pred == Yte).float().mean().item()
    fixed = int(((base_test != Yte) & (pred == Yte)).sum())
    broken = int(((base_test == Yte) & (pred != Yte)).sum())

    print()
    print(f"trained in {elapsed:.1f}s")
    print(f"baseline accuracy   {base_acc*100:.4f}%")
    print(f"model accuracy      {model_acc*100:.4f}%")
    print(f"cells fixed         {fixed:,}   (baseline wrong, model right)")
    print(f"cells broken        {broken:,}   (baseline right, model wrong)")
    print(f"net                 {fixed-broken:+,}")
    if int(base_wrong.sum()) > 0:
        print(f"share of baseline errors repaired: {100*fixed/int(base_wrong.sum()):.1f}%")

    args.out.mkdir(parents=True, exist_ok=True)
    torch.save(model.state_dict(), args.out / "weights.pt")
    report = {
        "device": device,
        "records": int(len(labels)),
        "frames": int(len(np.unique(frames))),
        "train_cells": int(train_mask.sum()),
        "test_cells": int(test_mask.sum()),
        "parameters": params,
        "epochs": args.epochs,
        "train_seconds": round(elapsed, 1),
        "baseline_accuracy": base_acc,
        "model_accuracy": model_acc,
        "cells_fixed": fixed,
        "cells_broken": broken,
        "net": fixed - broken,
        "dataset_geometries": meta.get("per_geometry", {}),
    }
    (args.out / "report.json").write_text(json.dumps(report, indent=2))
    print(f"\nwrote {args.out}/weights.pt and report.json")


if __name__ == "__main__":
    main()
