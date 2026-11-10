#include "euler_adaptive.hpp"
#include "step_limits.hpp"
#include "newton_core.hpp"
#include "lte.hpp"
#include "newton_trust_region.hpp"
#include "step_trace.hpp"
#include "fail_dump.hpp"
#include "../vm/vm.hpp"
#include "../vm/zcd.hpp"

#include <cstdio>
#include <algorithm>
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
    slots_begin_be(vm->slots, alpha);

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
            if (vm_noncon_ratio(vm, x_new, x_guess, KCL_RTOL, KCL_ATOL) > KCL_VETO)
            { x_guess = x_new; continue; }   // iterate on: the limiters release (KCL VETO, newton_core.hpp)
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
        vm_commit_slots_at(vm, x_guess);
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

    // Accepted points for the truncation-error estimate (lte.hpp).
    BdfHistory lhist;
    lhist.reset(vm->solver->last_solution);
    bool restart_after_event = false;   // the step just accepted ended on a .zcd event
    const std::vector<char> state_rows = lte_state_rows(*vm->solver->sys);

    while (vm->time < vm->end_time) {
        attempt++;
        VMSnapshot checkpoint = vm_save_state(vm);
        double dt = vm->time_step;
        // End this step on the next clock tick if it would cross one. Local
        // to the step: the controller's own preferred step is untouched.
        if (!vm->clocks.empty()) dt = vm_limit_step(vm, dt);

        StepResult step = take_implicit_step(vm, dt);

        // .zcd (vm/zcd.hpp): the step is already committed, so a logic jump
        // inside it rolls it back and re-solves it to end just after the jump.
        if (step.converged && vm->zcd_enabled) {
            const double s_ev = vm_locate_event(vm, checkpoint, vm->solver->last_solution, dt);
            if (s_ev > 0.0 && s_ev < 1.0 - 2.0 * ZCD_FRAC && s_ev * dt > lte_min_step(max_dt)) {
                vm_restore_state(vm, checkpoint);
                vm->time_step = s_ev * dt;
                step_trace("euler_adaptive", attempt, vm->time, dt, step.iters, "zcd-cut");
                continue;
            }
            if (s_ev > 0.0) restart_after_event = true;
        }

        double lte_r = -1.0;
        const LteMethod lm = lte_be();
        if (step.converged) {
            if (restart_after_event) { lhist.restart(); restart_after_event = false; }
            // TRUNCATION ERROR (lte.hpp). take_implicit_step has already
            // committed the step, slots included: the new value is now h0 and
            // the older ones h1, h2. A rejection rolls all of it back.
            const bool has_slots = vm->slots.size() > 0;
            LteStep ls{&lhist, &vm->solver->last_solution, &state_rows, n_nodes, dt,
                       rtol, atol, abstol,
                       has_slots ? &vm->slots.h0 : nullptr,
                       {&vm->slots.h1, &vm->slots.h2, &vm->slots.h2}};
            lte_r = lte_ratio(ls, lm);
            if (lte_r > 1.0 && dt > lte_min_step(max_dt)) {
                vm_restore_state(vm, checkpoint);
                vm->time_step = dt * lte_factor(lte_r, lm);
                step_trace("euler_adaptive", attempt, vm->time, dt, step.iters, "lte-reject");
                continue;
            }
            lhist.accept(vm->solver->last_solution, dt);
        }

        if (step.converged) {
            reject_run.clear();
            logger_log_step(&vm->logger, vm->time, vm->solver->sys, vm->solver->last_solution);
            if (vm->phase_log) vm->phase_log(vm);
            logger_log_signals(&vm->logger, vm->values);

            if (lte_r >= 0.0) {
                // The error estimate picks the next step; the .tran step is a ceiling.
                vm->time_step = std::min(max_dt, dt * lte_factor(lte_r, lm));
                if (step.iters > max_iter / 2) vm->time_step = std::min(vm->time_step, 0.8 * dt);
                step_trace("euler_adaptive", attempt, vm->time, dt, step.iters, "lte");
            } else if (step.iters < 5) {
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
                vm_fail(vm);
                dump_convergence_row(vm, conv.worst_row, conv.worst_step, conv.worst_tol);
                dump_failure(vm, "euler_adaptive", dt, step.alpha, last_x, last_history);
                break;
            }
        }
    }
}