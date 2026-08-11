#pragma once

#include "step_trace.hpp"

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

    // Set when the radius collapsed because the step had shrunk to a NO-OP —
    // residual_after equal to residual_before to full precision — rather than
    // because candidates were genuinely making things worse. The two demand
    // opposite responses from the caller and were previously indistinguishable.
    bool            stagnated = false;
    double          residual  = 0.0;   // residual at x_current when it gave up
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

    result.residual = residual_before;

    // NOTE — A TOLERANT ACCEPT TEST WAS TRIED HERE AND REVERTED.
    //
    // The strict `residual_after < residual_before` below has a real and
    // demonstrated failure mode: once the radius shrinks far enough that
    // x_candidate is numerically indistinguishable from x_current, the two
    // residuals are EQUAL to full precision, so the test can never pass again
    // and the radius decays to collapse. Measured on the bridge rectifier:
    // twenty collapses per run, each ending with before and after identical to
    // all seven printed digits at a radius of 7e-11. The trust region reads its
    // own no-op step as evidence of failure.
    //
    // Accepting on "not worse" (<= before * (1 + 1e-12)) fixes that reasoning,
    // and it is canary-clean on npn_amp (3197 steps, gain 65.51, unchanged).
    // But it moved pnp_amp's gain from 62.70 to 61.58 — 1.8%, past the 1%
    // threshold set in advance for "this is perturbing working circuits rather
    // than fixing anything" — while changing nothing about the rectifier it was
    // written for. Reverted on that rule rather than on judgement.
    //
    // Worth knowing before retrying: pnp_amp's measured gain has ranged over
    // 61.58..63.13 across solver-internal changes in this work, so roughly a
    // 2.5% band. An amplifier's gain should not be that sensitive to solver
    // internals, and understanding WHY it is may matter more than this test.
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
            // Set when the collapse came from the step becoming a no-op rather
            // than from candidates getting worse. Nothing acts on it today —
            // see the reverted stagnation response in bdf2.cpp — but it is what
            // distinguishes the two cases if this is picked up again.
            result.stagnated = (residual_after <= residual_before * (1.0 + 1e-9));
        }
    }

    // Diagnostic: the accept/reject decision is made purely on whether the
    // residual improved, with no notion of whether it is already small enough.
    // Logging both residuals against the radius is what distinguishes "Newton
    // is genuinely lost" from "Newton is sitting on the answer and cannot tell".
    if (step_trace_enabled()) {
        fprintf(stderr, "[tr] radius=%.3e before=%.6e after=%.6e %s\n",
                trust.radius, residual_before, residual_after,
                result.accepted ? "accept" : (result.collapsed ? "COLLAPSED" : "reject"));
    }

    return result;
}
// ─────────────────────────────────────────────────────────────────────────────
// RESIDUAL-BASED CONVERGENCE
//
// A second, independent convergence criterion: the iterate is acceptable if the
// true nonlinear residual is small, EVEN WHEN the step test rejects it.
//
// Why this is needed. A step-only test asks "did the iterate stop moving?",
// which is not the same question as "are the circuit equations satisfied?".
// They come apart whenever the Jacobian barely constrains some direction. At a
// bridge rectifier's commutation instant the source-side nodes are held only by
// a barely-conducting diode (g = Is/nvt*exp(v/nvt), around 3e-7 S) and a 1 MOhm
// reference, while other rows carry 1e-3 to 1e6. Those nodes sit in a
// near-null-space: the linear solve throws a large, meaningless excursion into
// them and it never settles. Measured on exactly that circuit: every equation
// satisfied to ||r||_inf = 3.0e-6, the blocking node's own residual 6.4e-7, and
// a Newton step of 1.776 V against a 1.97 mV tolerance — rejected forever, at
// a point that was already the answer.
//
// The test mirrors SPICE's KCL check rather than inventing one: a row passes if
// its residual is within reltol of the magnitude of the currents actually
// flowing in that row, plus an absolute floor. Scoring against the row's own
// traffic is what makes one tolerance work for a node carrying 20 mA and a node
// carrying 1 uA.
//
// NOTE this evaluates the analog blocks, so call it only when the step test has
// already failed — never on every iteration.
inline bool residual_converged(
    VM                     *vm,
    const Eigen::VectorXd  &x,
    double                  alpha,
    const Eigen::VectorXd  &x_history,
    double                  rtol,
    double                  atol)
{
    api_peek_solution(vm->api, x);
    vm_run_analog(vm);
    vm_run_phase_b(vm);

    Solver *s = vm->solver;
    Eigen::VectorXd gx   = s->sys->G * x;
    Eigen::VectorXd cdot = s->sys->C * (x_history - x);   // capacitive current / alpha

    for (int i = 0; i < x.size(); i++) {
        double r = s->sys->B_static(i) + alpha * cdot(i) - gx(i);

        // Row traffic: how much current is actually flowing through this
        // equation, used as the yardstick for reltol.
        //
        // The capacitive term MUST be formed as alpha*C*(x_history - x), the
        // net current, and not as the magnitudes of alpha*C*x and
        // alpha*C*x_history added separately. Those two are each enormous and
        // nearly cancel — at dt = 1.6e-6 a 10 uF capacitor holding 19.67 V
        // gives each of them about 185 A, while the real current through it is
        // milliamps. Summing them inflated the yardstick by four orders, which
        // made this test accept almost anything: the rectifier then failed at
        // t = 6.6e-4 instead of 1.7e-2, having been walked off course by
        // accepted non-solutions. Physical cancellation here is real and must
        // be respected.
        double scale = std::abs(gx(i)) + alpha * std::abs(cdot(i))
                     + std::abs(s->sys->B_static(i));

        if (std::abs(r) > rtol * scale + atol) return false;
    }
    return true;
}
