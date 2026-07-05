#!/usr/bin/env python3
"""Compare a Hover simulation CSV against a golden reference, with tolerance.

Exact comparison is deliberately NOT used: adaptive solvers (bdf2, ndf2,
trapezoidal) accept/reject steps based on floating-point convergence checks,
so a different compiler (zig-clang vs g++), libm, or CPU can legally produce
a slightly different time grid and trajectory. Instead, both signals are
interpolated onto the union of their time grids (within the overlapping time
range) and compared pointwise against a mixed absolute+relative tolerance
scaled by each column's full range in the reference.

Usage: compare_csv.py <reference.csv> <actual.csv> [rel_tol]
Exit 0 = match, 1 = mismatch, 2 = structural problem (missing column, etc).
"""
import csv
import sys


def load(path):
    with open(path, newline="") as f:
        rows = list(csv.reader(f))
    header, data = rows[0], rows[1:]
    cols = {name: [] for name in header}
    for r in data:
        for name, val in zip(header, r):
            cols[name].append(float(val))
    return header, cols


def interp(t_grid, t, y):
    """Linear interpolation of (t, y) onto t_grid. t must be ascending."""
    out, j = [], 0
    for tg in t_grid:
        while j + 1 < len(t) and t[j + 1] < tg:
            j += 1
        if tg <= t[0]:
            out.append(y[0])
        elif tg >= t[-1]:
            out.append(y[-1])
        else:
            t0, t1, y0, y1 = t[j], t[j + 1], y[j], y[j + 1]
            out.append(y0 if t1 == t0 else y0 + (y1 - y0) * (tg - t0) / (t1 - t0))
    return out


def main():
    ref_path, act_path = sys.argv[1], sys.argv[2]
    rel_tol = float(sys.argv[3]) if len(sys.argv) > 3 else 0.02  # 2% of range

    ref_hdr, ref = load(ref_path)
    act_hdr, act = load(act_path)

    if ref_hdr != act_hdr:
        print(f"MISMATCH: columns differ\n  ref: {ref_hdr}\n  act: {act_hdr}")
        return 2

    t_ref, t_act = ref["Time"], act["Time"]
    lo, hi = max(t_ref[0], t_act[0]), min(t_ref[-1], t_act[-1])
    if hi <= lo:
        print("MISMATCH: no overlapping time range")
        return 2
    # Skip the initial 5% of the run: DC settling transients are the most
    # compiler-sensitive part of an adaptive solve and not what we're testing.
    lo = lo + 0.05 * (hi - lo)
    grid = sorted({t for t in t_ref + t_act if lo <= t <= hi})

    failed = False
    for col in ref_hdr:
        if col == "Time":
            continue
        r = interp(grid, t_ref, ref[col])
        a = interp(grid, t_act, act[col])
        rng = max(ref[col]) - min(ref[col])
        tol = rel_tol * rng + 1e-9
        worst, worst_t = 0.0, 0.0
        for tg, rv, av in zip(grid, r, a):
            d = abs(rv - av)
            if d > worst:
                worst, worst_t = d, tg
        status = "ok" if worst <= tol else "FAIL"
        if worst > tol:
            failed = True
        print(f"  {col}: max|diff|={worst:.4g} at t={worst_t:.4g} (tol={tol:.4g}) {status}")

    return 1 if failed else 0


if __name__ == "__main__":
    sys.exit(main())
