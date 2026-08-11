#pragma once

#include <cstdio>
#include <cstdlib>
#include <cmath>
#include <string>
#include <vector>
#include <algorithm>
#include <Eigen/Dense>
#include "../vm/vm.hpp"

// ─────────────────────────────────────────────────────────────────────────────
// FAILURE DUMP
//
// Set HOVER_DUMP_FAIL to have a solver print, at the step it gives up on, the
// per-equation nonlinear residual with the node or branch each row belongs to.
//
// The point is attribution. "Failed to converge" and even a full step trace
// only tell you THAT Newton could not land; they cannot tell you WHICH circuit
// equations it could not satisfy. Since the residual is
// B_static(x) - G*x - alpha*C*(x - x_history), evaluating it row by row at the
// abandoned iterate names the offending nodes directly — and in a circuit where
// only one or two devices are commutating, that is usually the whole answer.
//
// Also prints the equilibration exponents. row_scale/col_scale are powers of
// two chosen so each row and column peaks at 1, so -log2 of them IS the decimal
// dynamic range the raw matrix had. That makes conditioning visible as a number
// rather than an inference.
// ─────────────────────────────────────────────────────────────────────────────

inline bool fail_dump_enabled() {
    static const bool on = (getenv("HOVER_DUMP_FAIL") != nullptr);
    return on;
}

// row_label resolves a solution-vector index to its net or branch name.
inline std::vector<std::string> row_labels(System *sys, int n) {
    std::vector<std::string> label(n, "?");
    for (int i = 0; i < (int)sys->node_names.size() && i < n; i++) {
        label[i] = sys->node_names[i];
    }
    for (const auto &kv : sys->branch_map) {
        int row = sys->num_nodes + kv.second;
        if (row >= 0 && row < n) label[row] = "I(" + kv.first + ")";
    }
    return label;
}

// dump_convergence_row reports the row that BLOCKED the convergence test, which
// is a different question from which equations carry residual: this test is on
// the step between successive iterates, not on how well the circuit equations
// are satisfied. Knowing whether the blocker is a node voltage (volts) or a
// branch current (amps) is what decides whether a single shared atol is the
// problem.
inline void dump_convergence_row(VM *vm, int row, double step, double tol) {
    if (!fail_dump_enabled() || row < 0) return;
    System *sys = vm->solver->sys;
    const int n = sys->size;
    std::vector<std::string> label = row_labels(sys, n);
    const char *kind = (row < sys->num_nodes) ? "NODE (volts)" : "BRANCH (amps)";
    fprintf(stderr, "[fail] convergence blocked by row %d  %-24s %s\n",
            row, (row < n ? label[row].c_str() : "?"), kind);
    fprintf(stderr, "[fail]   |step| = %.6e   tol = %.6e   over by %.1fx\n",
            step, tol, (tol > 0.0 ? step / tol : 0.0));
}

inline void dump_failure(VM *vm, const char *solver, double dt, double alpha,
                         const Eigen::VectorXd &x,
                         const Eigen::VectorXd &x_history)
{
    if (!fail_dump_enabled()) return;

    Solver *s   = vm->solver;
    System *sys = s->sys;
    const int n = (int)x.size();

    // Recompute the TRUE nonlinear residual at the abandoned iterate. peek, not
    // update, so nr_prev()'s backing store is not disturbed.
    api_peek_solution(vm->api, x);
    vm_run_analog(vm);
    vm_run_phase_b(vm);

    Eigen::VectorXd r = sys->B_static
                      + alpha * (sys->C * x_history)
                      - sys->G * x
                      - alpha * (sys->C * x);

    std::vector<std::string> label = row_labels(sys, n);

    fprintf(stderr, "\n[fail] ===== %s gave up =====\n", solver);
    fprintf(stderr, "[fail] t=%.9e  dt=%.6e  alpha=%.6e  n=%d\n",
            vm->time, dt, alpha, n);
    fprintf(stderr, "[fail] ||residual||_2 = %.6e   ||residual||_inf = %.6e\n",
            r.norm(), r.cwiseAbs().maxCoeff());

    // NOTE: an aggregate "matrix dynamic range" line used to live here and was
    // removed — it reported a single spread that disagreed with the per-row
    // rowscale column printed below (2^5..2^5 against an obvious 2^1..2^40),
    // and a diagnostic that prints a confidently wrong number is worse than one
    // that prints nothing. The per-row exponent below is measured directly off
    // the factorization and is the trustworthy one.
    //
    // Read that column as the raw magnitude of each row before scaling. A large
    // value means the row is dominated by alpha*C — the companion conductance,
    // which grows without bound as dt shrinks. Rows at 2^40 next to rows at 2^1
    // in the same system is the whole conditioning story, and note that
    // equilibration cannot help WITHIN a row: one factor per row cannot
    // separate an alpha*C term from a GMIN term sharing it.

    // Worst equations first — this is the attribution.
    std::vector<int> idx(n);
    for (int i = 0; i < n; i++) idx[i] = i;
    std::sort(idx.begin(), idx.end(), [&](int a, int b) {
        return std::abs(r(a)) > std::abs(r(b));
    });

    fprintf(stderr, "[fail] worst equations:\n");
    fprintf(stderr, "[fail]   %-28s %14s %14s %8s\n", "row", "x", "residual", "rowscale");
    int shown = (n < 14) ? n : 14;
    for (int k = 0; k < shown; k++) {
        int i = idx[k];
        fprintf(stderr, "[fail]   %-28s %14.6e %14.6e   2^%.0f\n",
                label[i].c_str(), x(i), r(i),
                -std::log2(s->row_scale(i)));
    }
    fprintf(stderr, "[fail] ==========================\n\n");
}
