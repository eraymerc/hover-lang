#include "ndf2.hpp"
#include "step_limits.hpp"
#include "newton_core.hpp"
#include "newton_trust_region.hpp"
#include "step_trace.hpp"
#include "fail_dump.hpp"
#include "../vm/vm.hpp"
#include "../vm/zcd.hpp"

#include <cstdio>
#include <cmath>

// ─────────────────────────────────────────────────────────────────────────────
// SHAMPINE'S NDF KAPPA COEFFICIENTS
// Shampine & Reichelt, "The MATLAB ODE Suite," SIAM J. Sci. Comput. 18 (1997).
// Index 0 = order 1, index 1 = order 2 (the only two implemented here).
// ─────────────────────────────────────────────────────────────────────────────

static const double NDF_KAPPA[2] = { -0.1850, -1.0 / 9.0 };

// The convergence test lives in newton_core.hpp, shared with bdf2. NDF2 used to
// carry its own, which scaled against x_new alone (unsafe across a zero
// crossing) and used a plain `>` comparison that reads a NaN row as converged.
//
// ─────────────────────────────────────────────────────────────────────────────
// RUN
//
// order 1 (NDF1): alpha = (1-k1)/h,
//                 x_blend = [y_n + k1*(2y_n - y_{n-1})] / (1-k1)
// order 2 (NDF2): alpha = (1.5-k2)/h,
//                 x_blend = [(4/3)y_n - (1/3)y_{n-1} + k2*(2/3)*(2y_n - y_{n-1})]
//                           / (1 - (2/3)*k2)
//
// Both reduce exactly to plain BDF1/BDF2 when kappa=0, confirming the
// derivation against the known special case before trusting the kappa-
// weighted general form.
// ─────────────────────────────────────────────────────────────────────────────

void NDF2::run(VM *vm) {
    if (max_dt == 0.0) max_dt = vm->time_step;
    n_nodes = vm->solver->sys->num_nodes;
    RejectRun reject_run;   // bounds the back-off spiral — see step_limits.hpp
    JacRefresh jac;         // within-step re-form policy — see newton_core.hpp

    int n = (int)vm->solver->last_solution.size();
    x_prev1 = vm->solver->last_solution;
    x_prev2 = vm->solver->last_solution;

    Eigen::VectorXd correction(n);
    Eigen::VectorXd x_blend(n);

    int step_count = 0;
    int order = 1; // starts at order 1, promotes to order 2 once history exists and convergence is fast

    Eigen::MatrixXd jacobian;
    bool jacobian_valid = false;

    long attempt = 0;   // counts ATTEMPTS, not accepted steps — see step_trace.hpp

    while (vm->time < vm->end_time) {
        attempt++;
        VMSnapshot checkpoint = vm_save_state(vm);
        double dt = vm->time_step;

        if (vm->zcd_enabled) {
            vm->time += dt;

            api_update_solution(vm->api, checkpoint.last_solution);
            vm_run_digital(vm);
            vm_run_analog(vm);

            if (vm_detect_discontinuity(vm, checkpoint)) {
                dt = vm_isolate_zero_crossing(vm, checkpoint, dt);
                vm_restore_state(vm, checkpoint);
                vm->time_step = dt;
                step_count = 0;
            } else {
                vm_restore_state(vm, checkpoint);
            }
        }

        vm->solver->sys->dt = dt;
        jac.begin_step();

        // Order selection: order 1 until at least one full step of real
        // history exists (step_count >= 1, so x_prev2 is a genuine
        // previous-previous point, not just a duplicate of x_prev1 from
        // initialization), then promote to order 2. This is a simplified
        // stand-in for MATLAB's actual error-estimator-driven order
        // selection — see ndf2.hpp for why the full scheme isn't
        // implemented here.
        bool use_order2 = (step_count >= 1);
        order = use_order2 ? 2 : 1;

        // THE KAPPA TERM IS SUBTRACTED, NOT ADDED — this sign was wrong here for
        // as long as the solver has existed, and it is what made ndf2 produce
        // confidently wrong DC answers on circuits bdf2 solves correctly.
        //
        // Derivation, order 2. The formula is
        //
        //   y_{n+1} - (4/3)y_n + (1/3)y_{n-1}
        //     - k*(2/3)*(y_{n+1} - 2y_n + y_{n-1}) = (2/3)*h*f
        //
        // Multiply by 1.5/h and collect the y_{n+1} terms into alpha:
        //
        //   alpha = 1.5*(1 - (2/3)k)/h = (1.5 - k)/h
        //   f = alpha*y_{n+1} - (1.5/h)*[ (4/3)y_n - (1/3)y_{n-1}
        //                                 - (2/3)k*(2y_n - y_{n-1}) ]
        //
        // The companion model this engine solves is f = alpha*(y - x_blend), so
        // x_blend is that bracket divided by (1 - (2/3)k) — with the kappa term
        // carrying a MINUS, inherited from the minus on the left-hand side.
        // Order 1 is the same derivation with alpha = (1-k)/h.
        //
        // THE TEST THAT CATCHES THIS is steady state, not kappa = 0. Setting
        // kappa = 0 recovers BDF1/BDF2 whichever sign is written, which is why
        // the check recorded in the comment above ("both reduce exactly to plain
        // BDF1/BDF2 when kappa=0") passed while the formula was wrong. Instead
        // put the circuit at rest, y_n = y_{n-1} = y: every capacitor current
        // must be zero, so x_blend MUST equal y exactly. With the minus it does,
        // identically in kappa. With the plus, order 2 gives
        // y*(1 + (2/3)k)/(1 - (2/3)k) = 0.862*y at k = -1/9, and order 1 gives
        // 0.688*y at k = -0.185 — the solver believes a capacitor at rest is
        // discharging by 14% (or 31%) of its voltage every single step.
        //
        // Measured, that is exactly what it looked like: examples/Diode/
        // rectifier.hvr returned 0.50 .. 22.18 V, an output with no filtering
        // left in it, and examples/BJT/npn_amp.hvr sat at a collector of 0.21 V
        // with its base at 0.91 V instead of 1.59 V — a DC bias error, not a
        // transient one, which is the signature of a history blend that is
        // systematically short.
        double kappa = NDF_KAPPA[order - 1];
        double alpha;
        if (order == 1) {
            alpha   = (1.0 - kappa) / dt;
            x_blend = (x_prev1 - kappa * (2.0 * x_prev1 - x_prev2)) / (1.0 - kappa);
        } else {
            alpha   = (1.5 - kappa) / dt;
            x_blend = ((4.0 / 3.0) * x_prev1 - (1.0 / 3.0) * x_prev2
                        - kappa * (2.0 / 3.0) * (2.0 * x_prev1 - x_prev2))
                      / (1.0 - (2.0 / 3.0) * kappa);
        }

        Eigen::VectorXd x_guess = x_prev1;
        bool converged = false;
        int  iters = 0;
        bool jacobian_current_this_step = false;

        api_update_solution(vm->api, x_guess);
        vm_run_digital(vm);
        vm_run_phase_b(vm);

        if (!jacobian_valid) {
            jacobian = vm_compute_jacobian(vm, x_guess);
            vm->solver->g_dirty = 1;
            jacobian_valid = true;
            jacobian_current_this_step = true;
        }

        TrustRegionState trust;

        for (int iter = 0; iter < max_iter; iter++) {
            iters++;

            // Within-step Jacobian refresh — policy and measurements in
            // newton_core.hpp. Holding J at the step's opening guess for all
            // max_iter iterations is wrong for a device whose conductance moves
            // orders of magnitude inside one step, and dt is not the knob for it.
            if (jac.due(iter, conv.worst_ratio)) {
                jacobian = vm_compute_jacobian(vm, x_guess);
                vm->solver->g_dirty = 1;
                jacobian_current_this_step = true;
            }

            api_update_solution(vm->api, x_guess);
            vm_run_analog(vm);
            vm_run_phase_b(vm);

            correction = jacobian * x_guess;

            const Eigen::VectorXd &x_new = solver_solve_rhs(
                vm->solver, 1.0, x_blend, alpha, &correction, &jacobian);

            // Opens the refresh window; as in bdf2 this reads the ratio left by
            // the previous convergence test, since it runs before this
            // iteration's own.
            if (iter == 0) jac.seed(conv.worst_ratio);

            if (newton_converged(x_new, x_guess, n_nodes, rtol, atol, abstol, conv)) {
                x_guess = x_new;
                converged = true;
                break;
            }

            // nullptr, NOT &correction: `correction` is the Newton
            // linearization term J*x_guess, which belongs only in the
            // linear solve's RHS (above). The true nonlinear residual that
            // trust_region_step scores must not include it — adding the
            // same large constant (J is huge at stiff junctions) to the
            // residual at both x_current and x_candidate swamps the
            // accept/reject comparison. NDF2 has no physical correction
            // term, so the residual term is null.
            TrustRegionResult tr = trust_region_step(
                vm, trust, x_guess, x_new, alpha, x_blend, nullptr);
            if (tr.collapsed) {
                break;
            }
            if (tr.accepted) {
                x_guess = tr.x_candidate;
            }
        }

        if (!converged) {
            vm_restore_state(vm, checkpoint);
            if (!jacobian_current_this_step) {
                jacobian_valid = false;
                step_trace("ndf2", attempt, vm->time, dt, iters, "jac-refresh");
                continue;
            }
            reject_run.note(dt);
            vm->time_step *= 0.5;
            step_trace("ndf2", attempt, vm->time, dt, iters, "reject");
            // NOTE: step_count is deliberately NOT reset here. An earlier
            // version reset it to 0 on every convergence failure, which
            // forced an order-2 -> order-1 demotion alongside the dt cut
            // every single time — confirmed by direct instrumentation to
            // cause a repeating "grow dt -> fail -> demote to order 1 ->
            // shrink dt -> climb back through 3-4 steps -> grow dt back
            // to the same danger zone -> fail again" cycle on a real BJT
            // test circuit, where each ~4-step cycle produced only ~1
            // step's worth of net forward progress. The step-size cut
            // alone already addresses "this dt was too large for the
            // current Jacobian/nonlinearity" — there's no separate reason
            // a failure should also imply "the order-2 formula itself was
            // wrong," and x_prev1/x_prev2 remain perfectly valid history
            // regardless of how this particular attempt's dt choice
            // turned out. Order falls back to 1 only when step_count is
            // genuinely reset elsewhere (ZCD re-priming after a detected
            // discontinuity), not as a side effect of ordinary step
            // rejection.
            if (reject_run.exhausted(vm->time_step)) {
                fprintf(stderr, "[NDF2] FATAL: Failed to converge at t=%.3e\n", vm->time);
                dump_convergence_row(vm, conv.worst_row, conv.worst_step, conv.worst_tol);
                dump_failure(vm, "ndf2", dt, alpha, x_guess, x_blend);
                break;
            }
            continue;
        }

        // THE JACOBIAN IS RE-FORMED ONCE PER ACCEPTED STEP.
        //
        // Without this line jacobian_valid is cleared only when a step FAILS, so
        // one Jacobian stays alive for as long as steps keep succeeding — across
        // a diode commutation, where the very conductances the matrix is made of
        // move from 1e-12 S to ~1 S. That is what this solver did until now, and
        // unlike bdf2 (where the same defect showed up as a run that stalled and
        // then failed) it showed up here as WRONG ANSWERS that completed
        // cleanly: an unfiltered rectifier output and a BJT amplifier biased at
        // the wrong operating point. See the policy note in ndf2.hpp for the
        // measurements, and bdf2.cpp's accept site for why a stale Jacobian
        // stays plausible while being wrong.
        jacobian_valid = false;

        reject_run.clear();
        x_prev2 = x_prev1;
        x_prev1 = x_guess;

        solver_advance_time(vm->solver, x_guess);
        vm->time += dt;
        logger_log_step(&vm->logger, vm->time, vm->solver->sys, vm->solver->last_solution);
        if (vm->phase_log) vm->phase_log(vm);
        logger_log_signals(&vm->logger, vm->values);
        step_count++;

        if (iters <= 5) {
            vm->time_step *= 1.2;
            if (vm->time_step > max_dt) vm->time_step = max_dt;
            step_trace("ndf2", attempt, vm->time, dt, iters, "grow");
        } else if (iters > max_iter / 2) {
            vm->time_step *= 0.8;
            step_trace("ndf2", attempt, vm->time, dt, iters, "shrink");
        } else {
            step_trace("ndf2", attempt, vm->time, dt, iters, "accept");
        }
    }
}