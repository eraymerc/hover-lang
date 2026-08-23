#pragma once

#include <Eigen/Dense>
#include <utility>
#include "../vm/solver_strategy.hpp"
#include "newton_core.hpp"

// ─────────────────────────────────────────────────────────────────────────────
// EULER ADAPTIVE
// Backward Euler with Modified Newton-Raphson and convergence-driven
// adaptive timestepping. Expands dt by 1.5x when the inner loop converges
// quickly, contracts by 0.5x on failure.
//
// Per-iteration damping uses diagonally-scaled trust-region (see
// newton_trust_region.hpp) instead of the original fixed absolute voltage
// clamp (max_step=0.05) — kept consistent with bdf2/ndf2/trapezoidal_fixed,
// which all made the same change earlier: a fixed clamp tuned for one
// circuit's scale does not generalize to circuits at a different
// voltage/current scale.
//
// JACOBIAN REUSE POLICY — this solver has never had an ACROSS-step staleness
// problem: take_implicit_step forms a fresh Jacobian at the top of every step
// attempt, unconditionally, which is what bdf2 and ndf2 had to be changed to do.
// So there is no jacobian_valid/jacobian_current_this_step pair here and no
// "retry the same dt with a fresh Jacobian" path — there is never a carried-over
// Jacobian to blame. What it did lack, and now has, is the WITHIN-step refresh
// (newton_core.hpp): a matrix formed at the step's anchor is the wrong
// linearization for a junction that changes state during the step, and that is a
// separate failure from a stale one, unreachable by cutting dt.
// ─────────────────────────────────────────────────────────────────────────────

struct EulerAdaptive : SolverStrategy {
    double rtol     = 1e-3;

    // atol is the VOLTAGE tolerance and abstol the CURRENT one — SPICE's
    // vntol/abstol split, see newton_core.hpp.
    double atol     = 1e-6;    // volts  — .solver argument 2
    double abstol   = 1e-12;   // amps   — .solver argument 5
    int    max_iter = 100;
    double max_dt   = 0.0;  // 0 = use vm->time_step at first run() call

    void run(VM *vm) override;
    bool is_adaptive() const override { return true; }

private:
    // What one step attempt did. alpha rides along so run() can hand it to the
    // failure dump, which needs the companion-model coefficient to rebuild the
    // residual at the abandoned iterate.
    struct StepResult {
        int    iters     = 0;
        bool   converged = false;
        double alpha     = 0.0;
    };
    StepResult take_implicit_step(VM *vm, double dt);

    // Row index where branch currents start — everything below it is a node
    // voltage. Captured in run() so the convergence test can pick the right
    // absolute tolerance per row.
    int n_nodes = 0;

    // What the last convergence test measured — see newton_core.hpp.
    ConvergenceReport conv;

    // The iterate the last attempt gave up on, and the history vector it was
    // solved against. Kept so run() can dump them after a FATAL;
    // take_implicit_step owns both while it runs.
    Eigen::VectorXd last_x, last_history;
};