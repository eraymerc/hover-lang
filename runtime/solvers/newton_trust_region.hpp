#pragma once

#include <Eigen/Dense>
#include <cmath>
#include "../mna/engine.hpp"
#include "../vm/vm.hpp"

// ─────────────────────────────────────────────────────────────────────────────
// DIAGONALLY-SCALED TRUST-REGION NEWTON DAMPING
//
// Shared by every Newton-based solver (tr_bdf2, bdf2, trapezoidal,
// trapezoidal_adaptive, euler_adaptive). Replaces the old fixed
// "clamp every node's update to +-max_step volts" approach.
//
// TWO separate correctness issues had to be fixed to get this right,
// both caught by actually instrumenting and running real circuits rather
// than reasoning about the algorithm in the abstract:
//
// 1. SCALE MISMATCH: a single global trust radius measured in raw step
//    norm does not work when a circuit has nodes at wildly different
//    natural scales (a 310V motor-drive bus alongside a 10mV BJT base
//    junction). Fixed with per-component (diagonal) scaling: each
//    variable's step is measured RELATIVE to its own current magnitude
//    (step[i] / max(|x[i]|, floor)), not in raw volts/amps.
//
// 2. RESIDUAL TAUTOLOGY: the first working version scored candidate
//    steps by checking ||B_dynamic - G_eff*x|| against the SAME
//    B_dynamic/G_eff that solver_solve_rhs had just used to compute the
//    candidate itself. Since the candidate IS (or is a damped step
//    toward) the exact algebraic solution of that linear system, the
//    residual against it is ALWAYS smaller for points closer to the
//    candidate -- this is true by construction, completely independent
//    of whether the underlying NONLINEAR circuit equations are actually
//    better satisfied. The result was every iteration showing
//    "accepted=true" unconditionally (confirmed by direct instrumentation:
//    radius grew 1.5 -> 3.7e10 over 59 iterations without a single
//    rejection), making the whole accept/reject mechanism a no-op and
//    explaining the BJT-circuit slowdown -- the loop was burning through
//    iterations with zero actual quality control.
//
//    Fixed by evaluating the residual using a FRESHLY RE-RUN analog phase
//    at the candidate point (residual_at — recomputes the true nonlinear
//    device currents at x, not the linearized approximation the solve
//    just assumed), giving an honest answer to "did this step actually
//    get closer to satisfying the real circuit equations."
// ─────────────────────────────────────────────────────────────────────────────

// compute_scale_vector returns D, where D[i] = max(|x[i]|, floor).
inline Eigen::VectorXd compute_scale_vector(const Eigen::VectorXd &x, double floor_val = 1e-6) {
    Eigen::VectorXd d(x.size());
    for (int i = 0; i < x.size(); i++) {
        double mag = std::abs(x(i));
        d(i) = (mag > floor_val) ? mag : floor_val;
    }
    return d;
}

// residual_at computes the TRUE nonlinear residual at candidate point x:
//   1. Push x into the API so V()/I() reads inside analog logic see it.
//   2. Re-run the analog phase — this recomputes nonlinear device
//      currents (diode/BJT exponentials, etc.) AT x, not at whatever
//      point the Jacobian was last linearized around.
//   3. Re-run phase_b to restamp those currents into B_static.
//   4. residual = B_static(x) - G*x - alpha*C*x_history_term, matching
//      the same equation form solver_solve_rhs builds (b_scale always 1.0
//      in every call site in this codebase, so hardcoded here too).
//
// alpha and x_history must be the SAME values the caller's Newton loop is
// using for its solver_solve_rhs calls this iteration, so the residual is
// evaluated against the identical implicit equation the loop is trying
// to solve — not some other formulation.
//
// correction, if non-null, is added the same way solver_solve_rhs adds it
// (e.g. trap_base for TR-BDF2's Stage 1) — omit only if the caller's RHS
// truly has no such term.
inline double residual_at(
    VM                     *vm,
    const Eigen::VectorXd  &x,
    double                  alpha,
    const Eigen::VectorXd  &x_history,
    const Eigen::VectorXd  *correction)
{
    // PROBE, not an iteration: push x into the read-view only. Using
    // api_peek_solution (not api_update_solution) keeps prev_solution —
    // nr_prev()'s backing store — pointing at the real previous Newton
    // iterate instead of this trial point. trust_region_step restores the
    // read-view to the current iterate after scoring.
    api_peek_solution(vm->api, x);
    vm_run_analog(vm);
    vm_run_phase_b(vm);

    Solver *s = vm->solver;
    Eigen::VectorXd gx = s->sys->G * x;
    Eigen::VectorXd target = s->sys->B_static + alpha * (s->sys->C * x_history);
    if (correction != nullptr) {
        target += *correction;
    }
    Eigen::VectorXd r = target - gx - alpha * (s->sys->C * x);
    return r.norm();
}

struct TrustRegionState {
    double radius;
    double grow_factor   = 1.5;
    double shrink_factor = 0.5;
    double min_radius    = 1e-10;
    double initial_radius;

    explicit TrustRegionState(double initial = 1.0)
        : radius(initial), initial_radius(initial) {}

    void reset() { radius = initial_radius; }
};

struct TrustRegionResult {
    Eigen::VectorXd x_candidate;
    bool            accepted;
    bool            collapsed;
};

// trust_region_step computes the diagonally-scaled candidate and scores
// it using residual_at — a genuine re-evaluation of the nonlinear circuit
// equations at the candidate point, not a comparison against the linear
// solve's own already-known exact solution.
//
// vm, alpha, x_history, and correction are forwarded to residual_at so
// the quality check matches exactly the implicit equation the caller's
// Newton loop is solving this iteration.
inline TrustRegionResult trust_region_step(
    VM                     *vm,
    TrustRegionState       &trust,
    const Eigen::VectorXd  &x_current,
    const Eigen::VectorXd  &x_full_step,
    double                  alpha,
    const Eigen::VectorXd  &x_history,
    const Eigen::VectorXd  *correction)
{
    TrustRegionResult result;
    result.accepted = false;
    result.collapsed = false;

    Eigen::VectorXd step = x_full_step - x_current;

    Eigen::VectorXd d = compute_scale_vector(x_current);
    Eigen::VectorXd scaled_step = step.cwiseQuotient(d);
    double scaled_step_norm = scaled_step.norm();

    Eigen::VectorXd x_candidate;
    if (scaled_step_norm > trust.radius && scaled_step_norm > 0.0) {
        double factor = trust.radius / scaled_step_norm;
        x_candidate = x_current + step * factor;
    } else {
        x_candidate = x_full_step;
    }

    // Genuine nonlinear residual comparison — both evaluated by actually
    // re-running the analog phase at each point, not by checking distance
    // to a linear solve's own exact solution.
    //
    // residual_at peeks (perturbs the read-view) but never advances
    // prev_solution. We snapshot the read-view first and restore it after,
    // so when control returns to the caller's Newton loop the API reflects
    // the real current iterate (x_current) — its next api_update_solution
    // then shifts the correct value into prev_solution and nr_prev() stays
    // honest.
    Eigen::VectorXd saved_reads = vm->api->last_solution;

    double residual_before = residual_at(vm, x_current,   alpha, x_history, correction);
    double residual_after  = residual_at(vm, x_candidate, alpha, x_history, correction);

    api_peek_solution(vm->api, saved_reads);

    if (residual_after < residual_before) {
        trust.radius *= trust.grow_factor;
        result.x_candidate = x_candidate;
        result.accepted = true;
    } else {
        trust.radius *= trust.shrink_factor;
        result.x_candidate = x_current;
        result.accepted = false;
        if (trust.radius < trust.min_radius) {
            result.collapsed = true;
        }
    }

    return result;
}