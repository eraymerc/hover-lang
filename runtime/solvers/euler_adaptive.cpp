#include "euler_adaptive.hpp"
#include "step_limits.hpp"
#include "newton_trust_region.hpp"
#include "../vm/vm.hpp"
#include "../vm/zcd.hpp"

#include <cstdio>
#include <cmath>

// ─────────────────────────────────────────────────────────────────────────────
// CONVERGENCE CHECK
// ─────────────────────────────────────────────────────────────────────────────

bool EulerAdaptive::has_converged(const Eigen::VectorXd &x_new, const Eigen::VectorXd &x_old) const {
    for (int i = 0; i < x_new.size(); i++) {
        double scale = atol + rtol * std::abs(x_new(i));
        if (std::abs(x_new(i) - x_old(i)) > scale) return false;
    }
    return true;
}

// ─────────────────────────────────────────────────────────────────────────────
// TAKE IMPLICIT STEP
// Newton-Raphson with a Jacobian computed once per step (Modified Newton).
// ─────────────────────────────────────────────────────────────────────────────

std::pair<int, bool> EulerAdaptive::take_implicit_step(VM *vm, double dt) {
    int n = (int)vm->solver->last_solution.size();
    Eigen::VectorXd x_anchor = vm->solver->last_solution;
    Eigen::VectorXd x_guess  = x_anchor;
    Eigen::VectorXd correction(n);

    bool converged = false;
    int  iters     = 0;
    double alpha   = 1.0 / dt;

    api_update_solution(vm->api, x_guess);
    vm_run_digital(vm);
    vm_run_phase_b(vm);

    // Jacobian evaluation moved outside the NR loop (Modified Newton)
    const Eigen::MatrixXd &jacobian = vm_compute_jacobian(vm, x_guess);
    vm->api->g_dirty = 1;
    vm->solver->g_dirty = 1;

    TrustRegionState trust; // default radius=1.0; per-component scaling does the real adaptation

    for (int iter = 0; iter < max_iter; iter++) {
        iters = iter + 1;

        // Re-run Phase A/B to calculate device currents for the current iteration
        api_update_solution(vm->api, x_guess);
        vm_run_analog(vm);
        vm_run_phase_b(vm);

        correction = jacobian * x_guess;

        const Eigen::VectorXd &x_new = solver_solve_rhs(
            vm->solver, 1.0, x_anchor, alpha, &correction, &jacobian);

        if (has_converged(x_new, x_guess)) {
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
    }
    return {iters, converged};
}

// ─────────────────────────────────────────────────────────────────────────────
// RUN
// ─────────────────────────────────────────────────────────────────────────────

void EulerAdaptive::run(VM *vm) {
    if (max_dt == 0.0) max_dt = vm->time_step;
    RejectRun reject_run;   // bounds the back-off spiral — see step_limits.hpp

    vm->solver->sys->dt = vm->time_step;

    while (vm->time < vm->end_time) {
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

        auto [iters, converged] = take_implicit_step(vm, dt);

        if (converged) {
            reject_run.clear();
            logger_log_step(&vm->logger, vm->time, vm->solver->sys, vm->solver->last_solution);
            if (vm->phase_log) vm->phase_log(vm);
            logger_log_signals(&vm->logger, vm->values);

            if (iters < 5) {
                vm->time_step *= 1.5;
                if (vm->time_step > max_dt) vm->time_step = max_dt;
            } else if (iters > max_iter / 2) {
                vm->time_step *= 0.8;
            }
        } else {
            vm_restore_state(vm, checkpoint);
            reject_run.note(dt);
            vm->time_step *= 0.5;
            if (reject_run.exhausted(vm->time_step)) {
                fprintf(stderr, "[Adaptive] FATAL: Failed to converge at t=%.3e\n", vm->time);
                break;
            }
        }
    }
}