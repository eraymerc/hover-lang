#include "trapezoidal_fixed.hpp"
#include "newton_trust_region.hpp"
#include "../vm/vm.hpp"

#include <cstdio>
#include <cmath>
#include <algorithm>

// ─────────────────────────────────────────────────────────────────────────────
// CONVERGENCE CHECK (MATLAB Style)
// ─────────────────────────────────────────────────────────────────────────────

bool TrapezoidalFixed::trap_converged(const Eigen::VectorXd &x_new, const Eigen::VectorXd &x_old) const {
    for (int i = 0; i < x_new.size(); i++) {
        // Safe against zero-crossings: uses the maximum magnitude observed
        double max_val = std::max(std::abs(x_new(i)), std::abs(x_old(i)));
        double scale = atol + rtol * max_val;
        if (std::abs(x_new(i) - x_old(i)) > scale) return false;
    }
    return true;
}

// ─────────────────────────────────────────────────────────────────────────────
// RUN (Modified Newton Fixed-Step)
// ─────────────────────────────────────────────────────────────────────────────

void TrapezoidalFixed::run(VM *vm) {
    vm->solver->sys->dt = vm->time_step;
    int n = (int)vm->solver->last_solution.size();

    Eigen::VectorXd x_anchor(n), b_old(vm->solver->sys->B_static.size());
    Eigen::VectorXd trap_base(n), x_guess(n), norton_correction(n), total_correction(n);

    // Persistent Jacobian across iterations to avoid O(N^3) recalculations
    Eigen::MatrixXd cached_jacobian(n, n);
    bool recompute_jacobian = true;

    double alpha_trap  = 2.0 / vm->time_step;
    double alpha_euler = 1.0 / vm->time_step;

    while (vm->time < vm->end_time) {
        api_update_solution(vm->api, vm->solver->last_solution);
        vm_run_digital(vm);
        vm_run_phase_b(vm);

        x_anchor = vm->solver->last_solution;
        b_old    = vm->solver->sys->B_static;

        const Eigen::VectorXd &gx_old = solver_compute_gx(vm->solver, x_anchor);
        trap_base = b_old - gx_old;

        x_guess = x_anchor;
        bool converged = false;
        TrustRegionState trust;

        // Advance time BEFORE evaluating the analog components so that time-dependent 
        // sources (like AC voltages) correctly evaluate for the t + dt integration point.
        vm->time += vm->time_step;

        // Evaluate the Jacobian exactly once per time step
        if (recompute_jacobian) {
            cached_jacobian = vm_compute_jacobian(vm, x_anchor);
            vm->solver->g_dirty = 1; // Force LU factorization
            recompute_jacobian = false;
        } else {
            vm->solver->g_dirty = 0; // Reuse previous LU factorization
        }

        bool using_euler = false;

        for (int iter = 0; iter < max_iter; iter++) {
            api_update_solution(vm->api, x_guess);
            vm_run_analog(vm);
            vm_run_phase_b(vm);

            // If Trapezoidal rings or stalls, drop to Backward Euler to mathematically damp it.
            if (!using_euler && iter > max_iter / 2) {
                using_euler = true;
                // Because alpha drops from 2/dt to 1/dt, the dynamic conductance matrix 
                // changes. We MUST calculate a fresh Jacobian and force an LU decomposition.
                cached_jacobian = vm_compute_jacobian(vm, x_guess);
                vm->solver->g_dirty = 1; 
            }

            norton_correction = cached_jacobian * x_guess;

            Eigen::VectorXd x_new;
            double alpha_this_iter;
            Eigen::VectorXd correction_this_iter;

            if (using_euler) {
                alpha_this_iter = alpha_euler;
                correction_this_iter = norton_correction;
                x_new = solver_solve_rhs(vm->solver, 1.0, x_anchor, alpha_euler,
                                         &norton_correction, &cached_jacobian);
            } else {
                total_correction = trap_base + norton_correction;
                alpha_this_iter = alpha_trap;
                correction_this_iter = total_correction;
                x_new = solver_solve_rhs(vm->solver, 1.0, x_anchor, alpha_trap,
                                         &total_correction, &cached_jacobian);
            }

            if (trap_converged(x_new, x_guess)) {
                x_guess = x_new;
                converged = true;
                break;
            }

            TrustRegionResult tr = trust_region_step(
                vm, trust, x_guess, x_new, alpha_this_iter, x_anchor, &correction_this_iter);
            
            if (tr.collapsed) break;
            if (tr.accepted) x_guess = tr.x_candidate;

            // Step accepted but not converged yet. Turn off g_dirty to reuse the LU matrix 
            // for the next iteration (unless we trip the Euler fallback next loop).
            vm->solver->g_dirty = 0;
        }

        if (!converged) {
            fprintf(stderr, "[TrapFixed] warning: did not converge at t=%.3e\n", vm->time);
            // The local curvature is highly hostile (failed to converge). 
            // Discard the cached matrix and force a fresh evaluation on the next step.
            recompute_jacobian = true; 
        }

        solver_advance_time(vm->solver, x_guess);
        logger_log_step(&vm->logger, vm->time, vm->solver->sys, vm->solver->last_solution);
        if (vm->phase_log) vm->phase_log(vm);
        logger_log_signals(&vm->logger, vm->values);
    }
}