#!/usr/bin/env bash
# Run every module's tests, in order of what they cover.
#
# The order is deliberate: the protocol first, because everything else rests on it, then each
# application, then the loopback that runs both together. A failure early is more informative than the
# same failure surfacing later through three layers of machinery.
set -euo pipefail

cd "$(dirname "$0")/.."

status=0

run() {
    local name="$1" dir="$2"
    shift 2
    echo
    echo "=============================================================="
    echo " $name"
    echo "=============================================================="
    if ! (cd "$dir" && go test "$@" ./...); then
        status=1
        echo "FAILED: $name"
    fi
}

# -count=1 so a cached pass is never mistaken for a run. These tests touch databases and directories,
# and a cached result says nothing about the state of either.
run "shared protocol, codecs, and error correction" shared -count=1 -timeout 900s
run "sender: configuration, schema, jobs, storage, pipeline" sender -count=1 -timeout 900s
run "receiver: configuration, schema, storage, pipeline" receiver -count=1 -timeout 900s
# The end-to-end suite is not checked in, so a clone of this repository will not have it. Skipped with a
# notice rather than failing, because "the directory is absent" and "the tests failed" are different facts
# and a suite that conflated them would report a healthy checkout as broken.
if [ -d e2e ]; then
    run "end-to-end loopback over the optical channel" e2e -count=1 -timeout 1800s
else
    echo
    echo "=============================================================="
    echo " end-to-end loopback: skipped — no e2e directory in this checkout"
    echo "=============================================================="
    echo "The suite that proves the losslessness claim lives outside this"
    echo "repository. Everything below is the unit and integration coverage."
fi

echo
if [ "$status" -eq 0 ]; then
    echo "all suites passed"
else
    echo "one or more suites failed"
fi
exit "$status"
