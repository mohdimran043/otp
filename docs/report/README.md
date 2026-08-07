# The technical overview, and how to rebuild it

`optical-transport-overview.pdf` is generated from `overview.html`, which is assembled from the numbered
parts beside it. Nothing in it is hand-copied: the figures come from the measurements in `shared/rate_test.go`
and `shared/decode_rate_test.go`, and the two frame images are real frames pulled from a running transfer
through the sender's audit endpoint.

To rebuild it after changing a part:

```bash
docker run --rm -v "$PWD/docs/report:/work" -w /work \
  --entrypoint chromium-browser zenika/alpine-chrome:latest \
  --no-sandbox --headless --disable-gpu --no-pdf-header-footer \
  --print-to-pdf=/work/optical-transport-overview.pdf \
  --virtual-time-budget=8000 overview.html
```

The parts are assembled in order — `part1` supplies the title, `part0` the management summary that follows it,
then `part2` through `part5`. Keeping them separate is what makes a single section editable without
reflowing the rest.
