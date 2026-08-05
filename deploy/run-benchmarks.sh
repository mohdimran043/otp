#!/usr/bin/env bash
# Regenerate the figures the README quotes.
#
# Two kinds of measurement, because they answer different questions. The capacity table is what one frame
# carries, which is a property of the encoding and the grid; the loopback figures are what the whole path
# achieves, which is a property of this hardware as much as of the code. Quoting only one of them would
# mislead in one direction or the other.
set -euo pipefail
cd "$(dirname "$0")/.."

echo "=============================================================="
echo " Payload capacity per frame, by encoding"
echo "=============================================================="
(cd shared && go test ./encoding/ -run TestCapacityOrdering -count=1 -v 2>&1 | grep -A12 "capacity at")

echo
echo "=============================================================="
echo " Compression, by codec and corpus"
echo "=============================================================="
(cd shared && go test ./compress/ -run TestCompressionRatios -count=1 -v 2>&1 | grep -A10 "compressed size")

echo
echo "=============================================================="
echo " Error correction: recovery from random loss"
echo "=============================================================="
(cd shared && go test ./fec/ -run TestRecoveryOverhead -count=1 -v 2>&1 | grep -A8 "recovery from random")

echo
echo "=============================================================="
echo " End to end, both applications over the optical channel"
echo "=============================================================="
(cd e2e && go test . -run 'TestLoopbackDeliversAFileWithNoLoss|TestRepeatedTransfersLoseNothing|TestAcknowledgedChunksAreNotShownAgain' \
    -count=1 -timeout 900s -v 2>&1 | grep -E "loopback_test.go:[0-9]+: (transferred|run |clean)")
