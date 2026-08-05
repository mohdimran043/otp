#!/usr/bin/env bash
# Submit the colour test image to the sender's API — the same endpoint the UI's form posts to.
set -euo pipefail
file="${1:-colour-test.png}"
callback="${2:-http://callback:9000/deliveries}"

curl -sS -X POST http://localhost:8080/api/v1/transfers \
    -F "file=@${file}" \
    -F "filename=$(basename "$file")" \
    -F "callback_url=${callback}"
