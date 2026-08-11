#include "trapezoidal.hpp"
#include "step_limits.hpp"
#include "newton_trust_region.hpp"
#include "../vm/vm.hpp"
#include "../vm/zcd.hpp"

#include <cstdio>
#include <cmath>
#include <algorithm>

// ─────────────────────────────────────────────────────────────────────────────
// CONVERGENCE CHECK (Zero-Crossing Safe)
// ─────────────────────────────────────────────────────────────────────────────

bool Trapezoidal::has_converged(const Eigen::VectorXd &x_new, const Eigen::VectorXd &x_old) const {
    for (int i = 0; i < x_new.size(); i++) {
        double max_val = std::max(std::abs(x_new(i)), std::abs(x_old(i)));
        double scale = atol + rtol * max_val;
        if (std::abs(x_new(i) - x_old(i)) > scale) return false;
    }
    return true;
}

// ─────────────────────────────────────────────────────────────────────────────
// RUN (Variable-Step SPICE Architecture)
// ─────────────────────────────────────────────────────────────────────────────

void Trapezoidal::run(VM *vm) {
    if (max_dt == 0.0) max_dt = vm->time_step;
    RejectRun reject_run;   // bounds the back-off spiral — see step_limits.hpp
    
    int n = (int)vm->solver->last_solution.size();
    Eigen::VectorXd x_anchor(n), b_old(vm->solver->sys->B_static.size());
    Eigen::VectorXd trap_base(n), x_guess(n), norton_correction(n), total_correction(n);

    Eigen::MatrixXd cached_jacobian(n, n);
    bool force_recompute = true;
    bool force_euler = true; // ALWAYS start the simulation with a Backward Euler step

    while (vm->time < vm->end_time) {
        VMSnapshot checkpoint = vm_save_state(vm);
        double dt = vm->time_step;
        bool zcd_triggered = false;

        // 1. ZCD Breakpoint Detection
        //
        // BUG FIX: vm_run_digital() must NOT be called inside this block.
        // This block's entire purpose is a DISPOSABLE probe — it runs
        // analog forward by a trial dt purely to check for a sign change
        // (vm_detect_discontinuity), then vm_restore_state() unconditionally
        // rolls back every side effect from this probe, on every call,
        // regardless of whether a discontinuity was actually found.
        //
        // The original code called vm_run_digital() here too, which means
        // any digital module's state changes (e.g. a state variable like
        // omega_ref being ramped up by reference_generator) were computed
        // and then immediately discarded by the restore below — and
        // crucially, digital logic was NEVER run again anywhere else in
        // this solver's main loop (only vm_run_analog/vm_run_phase_b run
        // inside the Newton loop further down). The net effect: any
        // digital-only state variable stayed at its initial value for the
        // entire simulation, confirmed directly — omega_ref read back as
        // exactly 0.0 for all 10001 logged timesteps on a real circuit,
        // with and without .zcd enabled, while the identical circuit
        // under euler_fixed (which calls vm_run_digital() once, plainly,
        // with no rollback) ramped correctly.
        if (vm->zcd_enabled) {
            vm->time += dt;
            api_update_solution(vm->api, checkpoint.last_solution);
            vm_run_analog(vm);

            if (vm_detect_discontinuity(vm, checkpoint)) {
                dt = vm_isolate_zero_crossing(vm, checkpoint, dt);
                zcd_triggered = true;
            }
            vm_restore_state(vm, checkpoint); // Rewind for the actual integration step
        }

        // Digital logic runs exactly once per step, AFTER the disposable
        // ZCD probe (if any) has already been rolled back — so its
        // result is computed fresh against the real, restored state and
        // is never discarded. This matches euler_fixed's unconditional,
        // un-rolled-back vm_run_digital() call.
        api_update_solution(vm->api, vm->solver->last_solution);
        vm_run_digital(vm);

        vm->time += dt;
        vm->solver->sys->dt = dt;

        // 2. Determine Integration Method for this step
        // We use Euler if recovering from a failed step, or if ZCD just triggered
        if (zcd_triggered) force_euler = true; 
        
        double alpha;
        if (force_euler) {
            alpha = 1.0 / dt;
            force_recompute = true; // Alpha changed, matrix must be rebuilt
        } else {
            alpha = 2.0 / dt;
        }

        // Prepare Trapezoidal History if not using Euler
        x_anchor = vm->solver->last_solution;
        if (!force_euler) {
            b_old = vm->solver->sys->B_static;
            const Eigen::VectorXd &gx_old = solver_compute_gx(vm->solver, x_anchor);
            trap_base = b_old - gx_old;
        }

        x_guess = x_anchor;
        bool converged = false;
        int iters = 0;

        // 3. Evaluate Initial Jacobian
        if (force_recompute) {
            cached_jacobian = vm_compute_jacobian(vm, x_anchor);
            vm->solver->g_dirty = 1;
            force_recompute = false;
        } else {
            vm->solver->g_dirty = 0;
        }

        TrustRegionState trust; // fresh radius each timestep

        // 4. Inner Newton-Raphson Loop
        for (int iter = 0; iter < max_iter; iter++) {
            iters++;
            api_update_solution(vm->api, x_guess);
            vm_run_analog(vm);
            vm_run_phase_b(vm);

            norton_correction = cached_jacobian * x_guess;
            Eigen::VectorXd x_new;

            // phys_correction is the term that genuinely belongs in the
            // nonlinear residual: trap_base (the old-time-point history
            // contribution) in Trapezoidal mode, nothing in Backward Euler
            // mode. norton_correction (= J*x_guess) is the Newton
            // linearization term — it goes into the linear solve's RHS but
            // must NEVER enter the residual, or the trust region goes blind
            // at stiff junctions (J is huge there).
            const Eigen::VectorXd *phys_correction;

            if (force_euler) {
                phys_correction = nullptr;
                x_new = solver_solve_rhs(vm->solver, 1.0, x_anchor, alpha,
                                         &norton_correction, &cached_jacobian);
            } else {
                total_correction = trap_base + norton_correction;
                phys_correction  = &trap_base;
                x_new = solver_solve_rhs(vm->solver, 1.0, x_anchor, alpha,
                                         &total_correction, &cached_jacobian);
            }

            if (has_converged(x_new, x_guess)) {
                x_guess = x_new;
                converged = true;
                break;
            }

            // Diagonally-scaled trust-region damping (newton_trust_region.hpp).
            // Replaces the old fixed absolute clamp (max_step = 0.05 V), which
            // was a semiconductor-specific assumption and wrong for a general
            // Verilog-AMS-style language. The residual is scored against the
            // TRUE physical equation (phys_correction only), so accept/reject
            // is meaningful even on stiff diode/BJT junctions.
            TrustRegionResult tr = trust_region_step(
                vm, trust, x_guess, x_new, alpha, x_anchor, phys_correction);
            if (tr.collapsed) break;
            if (tr.accepted) x_guess = tr.x_candidate;

            vm->solver->g_dirty = 0; // Reuse LU matrix
        }

        // 5. Adaptive Timestep Control & Recovery
        if (converged) {
            // Success! Log the state.
            solver_advance_time(vm->solver, x_guess);
            logger_log_step(&vm->logger, vm->time, vm->solver->sys, vm->solver->last_solution);
            if (vm->phase_log) vm->phase_log(vm);
            logger_log_signals(&vm->logger, vm->values);

            reject_run.clear();

            // Time Step Adjustment Heuristics
            if (iters <= 4) {
                vm->time_step = std::min(dt * 2.0, max_dt);
            } else if (iters > max_iter / 2) {
                vm->time_step = dt * 0.5;
            } else {
                vm->time_step = dt; // Hold steady
            }

            // Only use Euler on the step immediately following a ZCD event
            force_euler = zcd_triggered; 
            if (force_euler || vm->time_step != dt) force_recompute = true;

        } else {
            // Convergence Failed. Reject step, shrink dt, try again.
            vm_restore_state(vm, checkpoint);
            reject_run.note(dt);
            vm->time_step = dt * 0.25; // Aggressive back-off

            if (reject_run.exhausted(vm->time_step)) {
                fprintf(stderr, "[Trap] FATAL: Failed to converge at t=%.3e\n", vm->time);
                break;
            }
            
            force_euler = true;     // Use Euler to punch through the hard non-linearity
            force_recompute = true; // Matrix must be cleared out
        }
    }
}