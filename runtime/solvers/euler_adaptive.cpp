#include "euler_adaptive.hpp"
#include "step_limits.hpp"
#include "newton_core.hpp"
#include "newton_trust_region.hpp"
#include "step_trace.hpp"
#include "fail_dump.hpp"
#include "../vm/vm.hpp"
#include "../vm/zcd.hpp"

#include <cstdio>
#include <cmath>

// The convergence test lives in newton_core.hpp, shared with bdf2. This solver
// used to carry its own, which scaled against x_new alone (unsafe across a zero
// crossing) and used a plain `>` comparison that reads a NaN row as converged.
//
// ─────────────────────────────────────────────────────────────────────────────
// TAKE IMPLICIT STEP
// Newton-Raphson with a Jacobian formed once per step (Modified Newton), plus
// the progress-triggered within-step re-form from newton_core.hpp.
// ─────────────────────────────────────────────────────────────────────────────

EulerAdaptive::StepResult EulerAdaptive::take_implicit_step(VM *vm, double dt) {
    int n = (int)vm->solver->last_solution.size();
    Eigen::VectorXd x_anchor = vm->solver->last_solution;
    Eigen::VectorXd x_guess  = x_anchor;
    Eigen::VectorXd correction(n);

    bool converged = false;
    int  iters     = 0;
    double alpha   = 1.0 / dt;

    JacRefresh jac;   // within-step re-form policy — see newton_core.hpp

    api_update_solution(vm->api, x_guess);
    vm_run_digital(vm);
    vm_run_phase_b(vm);

    // Jacobian evaluation outside the NR loop (Modified Newton). Held BY VALUE,
    // not as a reference to vm_compute_jacobian's internal buffer: the
    // within-step refresh below reassigns it, and self-assignment through a
    // reference to that same buffer is not a well-defined way to express "form a
    // new one".
    Eigen::MatrixXd jacobian = vm_compute_jacobian(vm, x_guess);
    vm->api->g_dirty = 1;
    vm->solver->g_dirty = 1;

    TrustRegionState trust; // default radius=1.0; per-component scaling does the real adaptation

    for (int iter = 0; iter < max_iter; iter++) {
        iters = iter + 1;

        // Within-step Jacobian refresh — policy and measurements in
        // newton_core.hpp.
        if (jac.due(iter, conv.worst_ratio)) {
            jacobian = vm_compute_jacobian(vm, x_guess);
            vm->solver->g_dirty = 1;
        }

        // Re-run Phase A/B to calculate device currents for the current iteration
        api_update_solution(vm->api, x_guess);
        vm_run_analog(vm);
        vm_run_phase_b(vm);

        correction = jacobian * x_guess;

        const Eigen::VectorXd &x_new = solver_solve_rhs(
            vm->solver, 1.0, x_anchor, alpha, &correction, &jacobian);

        // Opens the refresh window; as in bdf2 this reads the ratio left by the
        // previous convergence test, since it runs before this iteration's own.
        if (iter == 0) jac.seed(conv.worst_ratio);

        if (newton_converged(x_new, x_guess, n_nodes, rtol, atol, abstol, conv)) {
            x_guess = x_new;
            converged = true;
            break;
        }

        // nullptr, NOT &correction: `correction` is the Newton
        // linearization term J*x_guess and belongs only in the linear
        // solve's RHS above. Feeding it into the residual adds the same
        // large constant to both endpoints of the accept/reject comparison
        // (J is huge at stiff junctions), blinding the trust region.
        // Backward Euler here has no physical correction term.
        TrustRegionResult tr = trust_region_step(
            vm, trust, x_guess, x_new, alpha, x_anchor, nullptr);
        if (tr.collapsed) {
            break;
        }
        if (tr.accepted) {
            x_guess = tr.x_candidate;
        }
    }

    if (converged) {
        solver_advance_time(vm->solver, x_guess);
        vm->time += dt;
    } else {
        // Kept for the failure dump in run(), which needs the abandoned iterate
        // and the history it was solved against to rebuild the residual.
        last_x       = x_guess;
        last_history = x_anchor;
    }
    return {iters, converged, alpha};
}

// ─────────────────────────────────────────────────────────────────────────────
// RUN
// ─────────────────────────────────────────────────────────────────────────────

void EulerAdaptive::run(VM *vm) {
    if (max_dt == 0.0) max_dt = vm->time_step;
    n_nodes = vm->solver->sys->num_nodes;
    RejectRun reject_run;   // bounds the back-off spiral — see step_limits.hpp

    vm->solver->sys->dt = vm->time_step;

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
            } else {
                vm_restore_state(vm, checkpoint);
            }
        }

        StepResult step = take_implicit_step(vm, dt);

        if (step.converged) {
            reject_run.clear();
            logger_log_step(&vm->logger, vm->time, vm->solver->sys, vm->solver->last_solution);
            if (vm->phase_log) vm->phase_log(vm);
            logger_log_signals(&vm->logger, vm->values);

            if (step.iters < 5) {
                vm->time_step *= 1.5;
                if (vm->time_step > max_dt) vm->time_step = max_dt;
                step_trace("euler_adaptive", attempt, vm->time, dt, step.iters, "grow");
            } else if (step.iters > max_iter / 2) {
                vm->time_step *= 0.8;
                step_trace("euler_adaptive", attempt, vm->time, dt, step.iters, "shrink");
            } else {
                step_trace("euler_adaptive", attempt, vm->time, dt, step.iters, "accept");
            }
        } else {
            vm_restore_state(vm, checkpoint);
            reject_run.note(dt);
            vm->time_step *= 0.5;
            step_trace("euler_adaptive", attempt, vm->time, dt, step.iters, "reject");
            if (reject_run.exhausted(vm->time_step)) {
                fprintf(stderr, "[Adaptive] FATAL: Failed to converge at t=%.3e\n", vm->time);
                dump_convergence_row(vm, conv.worst_row, conv.worst_step, conv.worst_tol);
                dump_failure(vm, "euler_adaptive", dt, step.alpha, last_x, last_history);
                break;
            }
        }
    }
}