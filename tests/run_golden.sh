#!/bin/bash
# ─────────────────────────────────────────────────────────────────────────────
# HOVER GOLDEN TESTS
#
# End-to-end regression tests: compile and run each test circuit, then
# compare the output CSV against a checked-in reference waveform within
# tolerance (see compare_csv.py for why tolerance, not exact match).
#
# Usage:
#   tests/run_golden.sh            run all golden tests
#   tests/run_golden.sh --update   regenerate the reference CSVs
#
# Run from the repository root (the hover binary resolves standard_library/
# relative to its own location, so it must be built at the root).
# ─────────────────────────────────────────────────────────────────────────────
set -u
cd "$(dirname "$0")/.."

TESTS=(
    "examples/BJT/npn_amp.hvr"
    "examples/Diode/rectifier.hvr"
    "examples/DCMotor/h_bridge.hvr"
    "tests/circuits/types_exercise.hvr"
)

UPDATE=0
[ "${1:-}" = "--update" ] && UPDATE=1

rm -f sim.cpp  # a leftover sim.cpp in the root breaks "go build ." (C++ file in Go package dir)
echo "[golden] building hover..."
go build -o ./hover . || exit 1

FAILED=0
for hvr in "${TESTS[@]}"; do
    name=$(basename "$hvr" .hvr)
    ref="tests/golden/${name}.csv"
    echo ""
    echo "[golden] $name"

    if ! ./hover "$hvr" > "/tmp/hover_${name}.log" 2>&1; then
        echo "  FAIL: hover exited non-zero (see /tmp/hover_${name}.log)"
        FAILED=1
        continue
    fi
    if [ ! -f simulation_output.csv ]; then
        echo "  FAIL: no simulation_output.csv produced"
        FAILED=1
        continue
    fi

    if [ "$UPDATE" = 1 ]; then
        mkdir -p tests/golden
        cp simulation_output.csv "$ref"
        echo "  updated $ref"
    elif [ ! -f "$ref" ]; then
        echo "  FAIL: no reference at $ref (run with --update to create)"
        FAILED=1
    else
        python3 tests/compare_csv.py "$ref" simulation_output.csv || FAILED=1
    fi
    rm -f simulation_output.csv sim sim.cpp
done

echo ""
if [ "$FAILED" = 1 ]; then
    echo "[golden] FAILURES — see above"
    exit 1
fi
echo "[golden] all tests passed"
