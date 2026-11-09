#include "trapezoidal.hpp"
#include "step_limits.hpp"
#include "newton_core.hpp"
#include "lte.hpp"
#include "newton_trust_region.hpp"
#include "step_trace.hpp"
#include "fail_dump.hpp"
#include "../vm/vm.hpp"
#include "../vm/zcd.hpp"

#include <cstdio>
#include <cmath>
#include <algorithm>

// The convergence test lives in newton_core.hpp, shared with bdf2. This solver's
// own version was already zero-crossing safe — it is where that form came from —
// but it used a plain `>` comparison, which reads a NaN row as CONVERGED, and a
// single absolute tolerance for node voltages and branch currents alike.
//
// ─────────────────────────────────────────────────────────────────────────────
// RUN (Variable-Step SPICE Architecture)
// ─────────────────────────────────────────────────────────────────────────────

void Trapezoidal::run(VM *vm) {
    if (max_dt == 0.0) max_dt = vm->time_step;
    n_nodes = vm->solver->sys->num_nodes;
    RejectRun reject_run;   // bounds the back-off spiral — see step_limits.hpp
    JacRefresh jac;         // within-step re-form policy — see newton_core.hpp

    int n = (int)vm->solver->last_solution.size();
    Eigen::VectorXd x_anchor(n), b_old(vm->solver->sys->B_static.size());
    Eigen::VectorXd trap_base(n), x_guess(n), norton_correction(n), total_correction(n);

    // jacobian_valid says whether a usable Jacobian is held at all;
    // jacobian_current_this_step says whether it was formed during THIS attempt.
    // The pair implements ode15s's failure rule, as in bdf2: a step that fails on
    // a carried-over Jacobian retries with a fresh one at the same dt, and only a
    // step that fails on a fresh Jacobian is blamed on dt. The Jacobian is never
    // blamed twice within one step. Every ACCEPTED step invalidates it, so no
    // matrix ever survives into a step it was not formed for.
    Eigen::MatrixXd cached_jacobian(n, n);
    bool jacobian_valid = false;
    double jac_alpha = -1.0;      // alpha the held Jacobian was formed under (see the slots note)
    bool force_euler = true; // ALWAYS start the simulation with a Backward Euler step

    // Accepted points for the truncation-error estimate (lte.hpp). This solver
    // keeps no multistep history of its own; this one only feeds the estimate.
    BdfHistory lhist;
    lhist.reset(vm->solver->last_solution);
    const std::vector<char> state_rows = lte_state_rows(*vm->solver->sys);

    long attempt = 0;   // counts ATTEMPTS, not accepted steps — see step_trace.hpp

    while (vm->time < vm->end_time) {
        attempt++;
        VMSnapshot checkpoint = vm_save_state(vm);
        double dt = vm->time_step;
        // End this step on the next clock tick if it would cross one. Local
        // to the step: the controller's own preferred step is untouched.
        if (!vm->clocks.empty()) {
            dt = vm_limit_step(vm, dt);
            // A tick changes the circuit's inputs: take this step as Backward
            // Euler, as after a zero crossing, so stiff modes do not ring.
            if (vm_tick_due(vm)) { force_euler = true; lhist.restart(); }
        }
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
        jac.begin_step();

        // 2. Determine Integration Method for this step
        // We use Euler if recovering from a failed step, or if ZCD just triggered
        if (zcd_triggered) { force_euler = true; lhist.restart(); }

        // No "alpha changed, rebuild the matrix" flag is needed here any more:
        // solver_solve_rhs refactorizes whenever g_dirty is set OR alpha differs
        // from the last solve (mna/engine.cpp), and the Jacobian is re-formed at
        // the top of every step regardless.
        double alpha = force_euler ? (1.0 / dt) : (2.0 / dt);

        // ddt/idt: the same companion as the matrix (state_slots.hpp), with the
        // derivative at the last point for the trapezoidal rule. alpha*dq/dV is
        // part of the Jacobian, so one formed under another alpha is re-formed
        // (only when the deck has slots — otherwise nothing changes).
        static const double W_LAST[3] = {1.0, 0.0, 0.0};
        if (force_euler) slots_begin_be(vm->slots, alpha);
        else             slots_begin(vm->slots, alpha, W_LAST, 1.0);
        if (vm->slots.size() > 0 && alpha != jac_alpha) jacobian_valid = false;

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
        bool jacobian_current_this_step = false;

        // 3. Evaluate Initial Jacobian
        if (!jacobian_valid) {
            cached_jacobian = vm_compute_jacobian(vm, x_anchor);
            vm->solver->g_dirty = 1;
            jacobian_valid = true;
            jacobian_current_this_step = true;
            jac_alpha = alpha;
        } else {
            vm->solver->g_dirty = 0;
        }

        TrustRegionState trust; // fresh radius each timestep

        // 4. Inner Newton-Raphson Loop
        for (int iter = 0; iter < max_iter; iter++) {
            iters++;

            // Within-step Jacobian refresh — policy and measurements in
            // newton_core.hpp. A Jacobian formed at x_anchor is the wrong
            // linearization for a junction that changes state inside the step,
            // and shrinking dt does not address it.
            if (jac.due(iter, conv.worst_ratio)) {
                cached_jacobian = vm_compute_jacobian(vm, x_guess);
                vm->solver->g_dirty = 1;
                jacobian_current_this_step = true;
            }

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

            // Opens the refresh window; as in bdf2 this reads the ratio left by
            // the previous convergence test, since it runs before this
            // iteration's own.
            if (iter == 0) jac.seed(conv.worst_ratio);

            if (newton_converged(x_new, x_guess, n_nodes, rtol, atol, abstol, conv)) {
                if (vm_kcl_ratio(vm, x_new, alpha, x_anchor, phys_correction, KCL_RTOL, KCL_ATOL, rtol, atol, abstol) > KCL_VETO) {
                    step_trace("trap", attempt, vm->time, dt, iters, "kcl-veto");
                    x_guess = x_new;
                    continue;   // iterate on: the limiters release next time (KCL VETO, newton_core.hpp)
                }
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
            // TRUNCATION ERROR (lte.hpp), before anything is committed.
            vm_eval_slots_at(vm, x_guess);
            LteMethod lm = force_euler ? lte_be() : lte_trap();
            LteStep ls{&lhist, &x_guess, &state_rows, n_nodes, dt, rtol, atol, abstol,
                       vm->slots.size() > 0 ? &vm->slots.s_now : nullptr,
                       {&vm->slots.h0, &vm->slots.h1, &vm->slots.h2}};
            double lte_r = lte_ratio(ls, lm);
            if (lte_r < 0.0 && lm.p > 1) { lm = lte_be(); lte_r = lte_ratio(ls, lm); }
            if (lte_r > 1.0 && dt > lte_min_step(max_dt)) {
                vm_restore_state(vm, checkpoint);
                vm->time_step = dt * lte_factor(lte_r, lm);
                step_trace("trap", attempt, vm->time, dt, iters, "lte-reject");
                continue;
            }
            // Success! Log the state.
            slots_commit(vm->slots);
            lhist.accept(x_guess, dt);
            solver_advance_time(vm->solver, x_guess);
            logger_log_step(&vm->logger, vm->time, vm->solver->sys, vm->solver->last_solution);
            if (vm->phase_log) vm->phase_log(vm);
            logger_log_signals(&vm->logger, vm->values);

            // THE JACOBIAN IS RE-FORMED ONCE PER ACCEPTED STEP.
            //
            // This used to be conditional — the matrix was rebuilt only when
            // alpha changed, i.e. on a dt change or a switch to Backward Euler —
            // so a run of accepted steps at a steady dt kept solving against one
            // matrix. A circuit's Jacobian IS its matrix of device conductances,
            // and a diode's moves from 1e-12 S to ~1 S across a commutation, so
            // the linearization silently stops describing the circuit while the
            // iteration still converges (to the right fixed point, just linearly
            // and ever more slowly). Measured on examples/Diode/rectifier.hvr the
            // run died at t = 11.6 ms; the same defect and the same fix are
            // recorded at bdf2.cpp's accept site.
            jacobian_valid = false;

            reject_run.clear();

            // Time Step Adjustment Heuristics
            if (lte_r >= 0.0) {
                // The error estimate picks the next step; the .tran step is a ceiling.
                vm->time_step = std::min(max_dt, dt * lte_factor(lte_r, lm));
                if (iters > max_iter / 2) vm->time_step = std::min(vm->time_step, 0.5 * dt);
                step_trace("trap", attempt, vm->time, dt, iters, "lte");
            } else if (iters <= 4) {
                vm->time_step = std::min(dt * 2.0, max_dt);
                step_trace("trap", attempt, vm->time, dt, iters, "grow");
            } else if (iters > max_iter / 2) {
                vm->time_step = dt * 0.5;
                step_trace("trap", attempt, vm->time, dt, iters, "shrink");
            } else {
                vm->time_step = dt; // Hold steady
                step_trace("trap", attempt, vm->time, dt, iters, "accept");
            }

            // Only use Euler on the step immediately following a ZCD event
            force_euler = zcd_triggered;

        } else {
            // Convergence Failed.
            vm_restore_state(vm, checkpoint);

            // Blame the Jacobian before blaming dt: if this attempt inherited its
            // matrix rather than forming one, retry at the SAME dt with a fresh
            // one. force_euler is left alone, so the retry solves the same
            // problem this attempt did, with a better linearization of it.
            if (!jacobian_current_this_step) {
                jacobian_valid = false;
                step_trace("trap", attempt, vm->time, dt, iters, "jac-refresh");
                continue;
            }

            // A fresh Jacobian still failed — now it is genuinely about dt.
            reject_run.note(dt);
            vm->time_step = dt * 0.25; // Aggressive back-off
            step_trace("trap", attempt, vm->time, dt, iters, "reject");

            if (reject_run.exhausted(vm->time_step)) {
                fprintf(stderr, "[Trap] FATAL: Failed to converge at t=%.3e\n", vm->time);
                vm_fail(vm);
                dump_convergence_row(vm, conv.worst_row, conv.worst_step, conv.worst_tol);
                dump_failure(vm, "trapezoidal", dt, alpha, x_guess, x_anchor);
                break;
            }

            force_euler = true;      // Use Euler to punch through the hard non-linearity
            jacobian_valid = false;  // Matrix must be cleared out
        }
    }
}