#pragma once

#include <Eigen/Dense>
#include "../vm/solver_strategy.hpp"
#include "newton_core.hpp"

// ─────────────────────────────────────────────────────────────────────────────
// TRAPEZOIDAL
// True variable-step Trapezoidal solver with iteration-count adaptation.
// Uses Backward Euler exclusively for post-ZCD ringing suppression and 
// recovery from failed convergence steps.
//
// Per-iteration damping uses diagonally-scaled trust-region (see
// newton_trust_region.hpp) instead of the original fixed absolute voltage
// clamp (max_step = 0.05 V). The fixed clamp was a semiconductor-specific
// assumption (it mirrors SPICE's pnjlim junction limit) and does not
// generalize to a Verilog-AMS-style language where a node may be at any
// scale. The trust region scores each candidate against the TRUE nonlinear
// residual — and crucially, the residual is built from the physical
// correction only (trap_base in Trapezoidal mode, none in Euler mode), never
// the Newton linearization term J*x_guess, which would otherwise blind the
// accept/reject decision at stiff junctions.
//
// JACOBIAN REUSE POLICY — BDF2's, with this solver's own recovery behaviour
// kept. One fresh Jacobian per ACCEPTED step; a progress-triggered re-form
// within a step (newton_core.hpp); and on a failed step the Jacobian is blamed
// before dt — if it was not formed during this step, the SAME dt is retried
// with a fresh one, and only a failure on a fresh Jacobian cuts dt.
//
// The previous policy re-formed only when alpha changed — a dt change or a
// switch to Backward Euler — so on a run of accepted steps at a steady dt one
// matrix was reused indefinitely, which is the same defect bdf2 had and the
// same one that made ndf2 return wrong DC answers. Measured here on
// examples/Diode/rectifier.hvr, this solver gave up entirely at t = 11.6 ms;
// with the policy below it completes the full 100 ms run.
//
// What is NOT taken from bdf2: the 0.25 back-off with a forced Backward Euler
// step after a failure (bdf2 halves and re-primes), and the iters <= 4 growth
// threshold. Those are this solver's own and are left alone.
// ─────────────────────────────────────────────────────────────────────────────

struct Trapezoidal : SolverStrategy {
    double rtol     = 1e-3;

    // atol is the VOLTAGE tolerance and abstol the CURRENT one — SPICE's
    // vntol/abstol split, see newton_core.hpp.
    double atol     = 1e-6;    // volts  — .solver argument 2
    double abstol   = 1e-12;   // amps   — .solver argument 5
    int    max_iter = 50;   // SPICE typically limits NR loops to ~20-50 max
    double max_dt   = 0.0;

    void run(VM *vm) override;
    bool is_adaptive() const override { return true; }

private:
    // Row index where branch currents start — everything below it is a node
    // voltage. Captured in run() so the convergence test can pick the right
    // absolute tolerance per row.
    int n_nodes = 0;

    // What the last convergence test measured — see newton_core.hpp.
    ConvergenceReport conv;
};