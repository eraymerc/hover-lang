#include "trapezoidal_fixed.hpp"
#include "newton_trust_region.hpp"
#include "newton_core.hpp"
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
    double jac_alpha = -1.0;   // alpha the cached Jacobian was formed under

    double alpha_trap  = 2.0 / vm->time_step;
    double alpha_euler = 1.0 / vm->time_step;

    // A step that starts on a clock tick that changed the logic's outputs is a
    // Backward Euler step — SPICE's rule at breakpoints. The trapezoidal rule is A-stable but not L-stable:
    // restarted from inputs a tick has just changed, its stiff modes ring at
    // full amplitude, sign flipping every step, and never decay. One BE step
    // damps them. (At t = 0 the bias point already is a BE step — see vm_boot.)
    while (vm->time < vm->end_time) {
        api_update_solution(vm->api, vm->solver->last_solution);
        vm_run_digital(vm);
        vm_run_phase_b(vm);
        // Backward Euler on a step whose tick changed what the logic drives
        // (item 13): a tick that changes nothing leaves the rule alone.
        const bool tick = vm->tick_changed;

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

        bool using_euler = tick;
        // ddt/idt follow the same formula as the matrix, switched in the same
        // place (state_slots.hpp). The trapezoidal companion needs the
        // derivative at the last point, which B_old carries for the matrix.
        // Published BEFORE the Jacobian: alpha*dq/dV is part of it, so a
        // Jacobian formed under another alpha has the wrong slope for every
        // ddt() — re-form it when alpha changes (only when there are slots, so
        // a deck without ddt/idt is solved exactly as before).
        static const double W_LAST[3] = {1.0, 0.0, 0.0};
        const double alpha_step = using_euler ? alpha_euler : alpha_trap;
        if (using_euler) slots_begin_be(vm->slots, alpha_euler);
        else             slots_begin(vm->slots, alpha_trap, W_LAST, 1.0);
        if (vm->slots.size() > 0 && alpha_step != jac_alpha) recompute_jacobian = true;

        // Evaluate the Jacobian exactly once per time step
        if (recompute_jacobian) {
            cached_jacobian = vm_compute_jacobian(vm, x_anchor);
            vm->solver->g_dirty = 1; // Force LU factorization
            recompute_jacobian = false;
            jac_alpha = alpha_step;
        } else {
            vm->solver->g_dirty = 0; // Reuse previous LU factorization
        }


        for (int iter = 0; iter < max_iter; iter++) {
            api_update_solution(vm->api, x_guess);
            vm_run_analog(vm);
            vm_run_phase_b(vm);

            // If Trapezoidal rings or stalls, drop to Backward Euler to mathematically damp it.
            if (!using_euler && iter > max_iter / 2) {
                using_euler = true;
                slots_begin_be(vm->slots, alpha_euler);
                jac_alpha = alpha_euler;
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
                // Iterate on: the limiters release next time. A fixed-step
                // solver cannot reject the step, so if max_iter runs out the
                // point is still taken — reported by the not-converged warning
                // below instead of passing silently (KCL VETO, newton_core.hpp).
                if (vm_noncon_ratio(vm, x_new, x_guess, KCL_RTOL, KCL_ATOL) > KCL_VETO) {
                    x_guess = x_new;
                    continue;
                }
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

        vm_commit_slots_at(vm, x_guess);
        solver_advance_time(vm->solver, x_guess);
        logger_log_step(&vm->logger, vm->time, vm->solver->sys, vm->solver->last_solution);
        if (vm->phase_log) vm->phase_log(vm);
        logger_log_signals(&vm->logger, vm->values);
    }
}