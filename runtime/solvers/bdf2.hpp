#pragma once

#include <Eigen/Dense>
#include "../vm/solver_strategy.hpp"
#include "newton_core.hpp"

// ─────────────────────────────────────────────────────────────────────────────
// BDF2
// 2nd-order Backward Differentiation Formula (Gear's method) with Modified
// Newton-Raphson and adaptive timestepping. L-stable — actively damps
// spurious oscillations after switching events. Uses a Backward Euler primer
// on startup and after any step rejection.
//
// JACOBIAN REUSE POLICY — one fresh Jacobian per accepted step, plus at most
// one extra re-form when a step fails.
//
// Within a step the Jacobian is Modified-Newton by default — held fixed across
// Newton iterations, so each iteration is a single LU solve against an
// already-factorized matrix — but it is NOT held unconditionally. Every eighth
// iteration the weighted-step norm is checked against its value at the previous
// checkpoint, and a Jacobian that has not bought a 100x reduction over that
// window is re-formed at the current iterate (bounded per step). Across steps it
// is not held at all: every accepted step starts by forming a new one.
//
// Both halves exist for the same reason and neither substitutes for the other: a
// circuit's Jacobian IS its matrix of device conductances, and a junction's moves
// many orders of magnitude — across a commutation (the across-step case) and also
// between a step's initial guess and its own solution (the within-step case, an
// LED turning off inside one 50 us step). Either way the iteration still has the
// true solution as its fixed point and so stays CORRECT, but it degrades from
// quadratic to linear, and once the contraction approaches 1 the step is
// abandoned. Cutting dt does not rescue the within-step case — the stiffness
// there is algebraic, not temporal. run() records the measurements.
//
// This deliberately departs from MATLAB's ode15s, which Shampine & Reichelt
// ("The MATLAB ODE Suite", 1997, Section 2.3) describe as forming "a Jacobian
// just once in the whole integration" for problems where it is nearly
// constant. That is the right trade for a smooth ODE and the wrong one for a
// switching circuit. What IS kept from ode15s is the failure rule: "Should
// this happen and the Jacobian not be current, a new Jacobian is formed.
// Otherwise the step size is reduced" — so a step that fails on a
// carried-over Jacobian retries with a fresh one at the same dt, and only a
// step that fails with a fresh Jacobian is blamed on dt. The Jacobian is
// never blamed twice within one step.
//
// The companion half of that rule — early termination on a predicted-bad
// convergence rate — was implemented and removed; run() records why.
//
// Per-iteration voltage clamping is replaced by diagonally-scaled
// trust-region damping (see newton_trust_region.hpp) for the same reasons
// established for TR-BDF2: a fixed absolute clamp tuned for one circuit's
// voltage scale does not generalize.
// ─────────────────────────────────────────────────────────────────────────────

struct BDF2 : SolverStrategy {
    double rtol     = 1e-3;

    // atol is the VOLTAGE tolerance and abstol the CURRENT one, matching
    // SPICE's vntol/abstol. The solution vector holds node voltages in rows
    // 0..num_nodes-1 and branch currents after them, so a single absolute
    // tolerance cannot serve both: 1e-6 is a reasonable microvolt on a node and
    // a very loose microamp on a branch. SPICE's defaults are kept.
    double atol     = 1e-6;    // volts  — .solver argument 2
    double abstol   = 1e-12;   // amps   — .solver argument 5
    int    max_iter = 100;
    double max_dt   = 0.0;  // 0 = use vm->time_step at first run() call

    void run(VM *vm) override;
    bool is_adaptive() const override { return true; }

private:
    Eigen::VectorXd x_prev1, x_prev2;

    // Row index where branch currents start; everything below it is a node
    // voltage. Captured in run() so the convergence test can pick the right
    // absolute tolerance per row (see newton_converged in newton_core.hpp).
    int n_nodes = 0;

    // What the last convergence test measured — the weighted step norm the
    // within-step refresh policy reads, plus the blocking-row diagnostics the
    // failure dump prints. See newton_core.hpp.
    ConvergenceReport conv;
};