#!/usr/bin/env bash
# Capture a page with headless Chrome in a container.
#
# In a container because the point is that this repository needs nothing installed to reproduce its own
# documentation: the same command works on a machine with no browser. --no-sandbox is safe here for the same
# reason it is usually not — the container is throwaway and the only page it visits is one we just built.
set -euo pipefail

url="$1"
out="$2"
width="${3:-1440}"
height="${4:-1000}"

docker run --rm --network host \
    -v "$(pwd)/docs/screenshots:/out" \
    --entrypoint chromium-browser \
    zenika/alpine-chrome:latest \
        --no-sandbox --headless --disable-gpu --hide-scrollbars \
        --force-color-profile=srgb \
        --window-size="${width},${height}" \
        --screenshot="/out/${out}" \
        --virtual-time-budget=12000 \
        "$url" > /dev/null 2>&1

echo "docs/screenshots/${out}"
