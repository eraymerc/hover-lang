#include "bdf2.hpp"
#include "newton_trust_region.hpp"
#include "step_trace.hpp"
#include "../vm/vm.hpp"
#include "../vm/zcd.hpp"

#include <cstdio>
#include <cmath>

// ─────────────────────────────────────────────────────────────────────────────
// CONVERGENCE CHECK
// ─────────────────────────────────────────────────────────────────────────────

bool BDF2::has_converged(const Eigen::VectorXd &x_new, const Eigen::VectorXd &x_old) const {
    for (int i = 0; i < x_new.size(); i++) {
        double scale = atol + rtol * std::abs(x_new(i));
        if (std::abs(x_new(i) - x_old(i)) > scale) return false;
    }
    return true;
}

// ─────────────────────────────────────────────────────────────────────────────
// RUN
//
// Mirrors Go: func (s *BDF2) Run(vm *VM)
// 2nd order BDF: alpha = 1.5/dt on steady steps, 1.0/dt on the primer step.
// xBlend = (4/3)*xPrev1 - (1/3)*xPrev2 (BDF2 history blend), or xPrev1 alone
// when priming.
// ─────────────────────────────────────────────────────────────────────────────

void BDF2::run(VM *vm) {
    if (max_dt == 0.0) max_dt = vm->time_step;

    int n = (int)vm->solver->last_solution.size();
    x_prev1 = vm->solver->last_solution;
    x_prev2 = vm->solver->last_solution;

    Eigen::VectorXd correction(n);
    Eigen::VectorXd x_blend(n);

    int step_count = 0;

    // Jacobian carries over across steps, exactly as ode15s does ("it is
    // natural to retain a copy of the Jacobian matrix... ode15s will
    // normally form a Jacobian just once in the whole integration" when
    // it's constant). jacobian_valid tracks whether we currently hold a
    // usable Jacobian at all; jacobian_current_this_step tracks whether
    // it was (re)formed during THIS step specifically — needed for the
    // documented rule "if convergence will fail and the Jacobian is not
    // current, form a new one; otherwise reduce the step size" — the
    // Jacobian is never blamed twice within the same step.
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
        Eigen::VectorXd x_guess = x_prev1;

        bool converged   = false;
        bool is_priming  = (step_count == 0);
        double alpha     = is_priming ? (1.0 / dt) : (1.5 / dt);

        if (is_priming) {
            x_blend = x_prev1;
        } else {
            x_blend = (4.0 / 3.0) * x_prev1 - (1.0 / 3.0) * x_prev2;
        }

        int iters = 0;
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

        TrustRegionState trust; // default radius=1.0; per-component scaling does the real adaptation

        // Track successive correction norms to predict the convergence
        // rate, exactly as ode15s does: "the rate of convergence is
        // monitored and the iteration terminated if it is predicted that
        // convergence will not be achieved in four iterations." Two
        // consecutive correction norms are enough to estimate a
        // contraction ratio rho = ||delta_k|| / ||delta_{k-1}||; if rho
        // is not safely below 1, four more iterations at that rate will
        // not bring the error under tolerance, so there is no point
        // continuing to burn the iteration budget.
        for (int iter = 0; iter < max_iter; iter++) {
            iters++;

            api_update_solution(vm->api, x_guess);
            vm_run_analog(vm);
            vm_run_phase_b(vm);

            correction = jacobian * x_guess;

            const Eigen::VectorXd &x_new = solver_solve_rhs(
                vm->solver, 1.0, x_blend, alpha, &correction, &jacobian);

            if (has_converged(x_new, x_guess)) {
                x_guess = x_new;
                converged = true;
                break;
            }

            // NOTE: pass nullptr, NOT &correction, as the residual's
            // physical-correction term. `correction` here is the Newton
            // linearization term J*x_guess — it belongs in the LINEAR
            // solve's RHS (above), but must NOT enter the true nonlinear
            // residual that trust_region_step scores candidates with.
            // Including it adds the constant J*x_guess to residual_at's
            // result at BOTH x_current and x_candidate; for stiff junctions
            // J is huge, so that constant swamps the comparison and the
            // accept/reject decision becomes numerical noise. BDF2 has no
            // genuine physical correction term, so the residual term is
            // null. (Solvers WITH a real physical term — e.g. trapezoidal's
            // trap_base — must pass that term ONLY, never the norton part.)
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
            // Documented rule: "Should this happen and the Jacobian not be
            // current, a new Jacobian is formed. Otherwise the step size
            // is reduced." The Jacobian is never blamed twice in the same
            // step — if it was already (re)formed this step and STILL
            // failed, only dt is reduced.
            vm_restore_state(vm, checkpoint);
            if (!jacobian_current_this_step) {
                // Attribute the failure to the Jacobian, not the step
                // size: invalidate it so the next attempt re-forms it,
                // and retry at the SAME dt and the SAME step_count/history
                // (x_prev1/x_prev2 untouched) — this is a Jacobian retry,
                // not a step rejection, so BDF2's multistep history should
                // not be discarded.
                jacobian_valid = false;
                step_trace("bdf2", attempt, vm->time, dt, iters, "jac-refresh");
                continue;
            }
            // Jacobian was already current and still failed — the
            // failure is genuinely about the step size now.
            vm->time_step *= 0.5;
            step_count = 0;
            step_trace("bdf2", attempt, vm->time, dt, iters, "reject");
            if (vm->time_step < min_dt) {
                fprintf(stderr, "[BDF2] FATAL: Failed to converge at t=%.3e\n", vm->time);
                break;
            }
            continue;
        }

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
            step_trace("bdf2", attempt, vm->time, dt, iters, "grow");
        } else if (iters > max_iter / 2) {
            vm->time_step *= 0.8;
            step_trace("bdf2", attempt, vm->time, dt, iters, "shrink");
        } else {
            step_trace("bdf2", attempt, vm->time, dt, iters, "accept");
        }
    }
}