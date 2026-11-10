#include "ndf2.hpp"
#include "step_limits.hpp"
#include "newton_core.hpp"
#include "newton_trust_region.hpp"
#include "step_trace.hpp"
#include "lte.hpp"
#include "fail_dump.hpp"
#include "../vm/vm.hpp"
#include "../vm/zcd.hpp"

#include <cstdio>
#include <cstdlib>
#include <cmath>
#include <algorithm>

// The convergence test lives in newton_core.hpp, shared with bdf2. NDF2 used to
// carry its own, which scaled against x_new alone (unsafe across a zero
// crossing) and used a plain `>` comparison that reads a NaN row as converged.
//
// ─────────────────────────────────────────────────────────────────────────────
// RUN
//
// BDF1 with one history point, NDF1 with two, NDF2 with three, all on the
// actual (variable) step grid. Coefficients and the derivation, including why
// NDF2's correction needs the QUADRATIC predictor: bdf_history.hpp.
// ─────────────────────────────────────────────────────────────────────────────

void NDF2::run(VM *vm) {
    if (max_dt == 0.0) max_dt = vm->time_step;
    n_nodes = vm->solver->sys->num_nodes;
    RejectRun reject_run;   // bounds the back-off spiral — see step_limits.hpp
    JacRefresh jac;         // within-step re-form policy — see newton_core.hpp

    int n = (int)vm->solver->last_solution.size();
    hist.reset(vm->solver->last_solution);
    const std::vector<char> state_rows = lte_state_rows(*vm->solver->sys);

    Eigen::VectorXd correction(n);
    Eigen::VectorXd x_blend(n);


    Eigen::MatrixXd jacobian;
    bool jacobian_valid = false;
    double jac_alpha = -1.0;   // alpha the held Jacobian was formed under

    long attempt = 0;   // counts ATTEMPTS, not accepted steps — see step_trace.hpp
    bool restart_after_event = false;   // the last step ended on a .zcd event

    while (vm->time < vm->end_time) {
        attempt++;
        VMSnapshot checkpoint = vm_save_state(vm);
        double dt = vm->time_step;
        // End this step on the next clock tick if it would cross one. Local
        // to the step: the controller's own preferred step is untouched.
        if (!vm->clocks.empty()) {
            dt = vm_limit_step(vm, dt);
        }

        vm->solver->sys->dt = dt;
        jac.begin_step();

        // Order by available history (bdf_history.hpp). THE KAPPA SIGN NOTE
        // that lived here — the correction is SUBTRACTED; with a plus, a
        // capacitor at rest read as discharging 14-31 % per step and the
        // rectifier and npn_amp settled at wrong DC points — still holds, and
        // the steady-state test it recommends (y_n = y_{n-1} = y must give
        // x_blend == y) is satisfied by the new formulas identically in kappa.
        double alpha;
        double w[3];
        hist.ndf_recipe(hist.usable_order(dt, 3), dt, alpha, w);
        hist.blend_of(w, x_blend);
        // ddt/idt slots get the same recipe (vm/state_slots.hpp). alpha*dq/dV
        // is part of the Jacobian, so a Jacobian formed under another alpha is
        // re-formed — only when the deck has slots, so nothing else changes.
        slots_begin(vm->slots, alpha, w, 0.0);
        if (vm->slots.size() > 0 && alpha != jac_alpha) jacobian_valid = false;

        Eigen::VectorXd x_guess = hist.y[0];

        // The step's logic pass comes first: whether this step must restart
        // the history depends on what it did. A clock tick restarts it only if
        // it changed what the logic drives (vm->tick_changed, item 13); a step
        // that follows a .zcd event always does.
        api_update_solution(vm->api, x_guess);
        vm_run_digital(vm);
        vm_run_phase_b(vm);
        if (vm->tick_changed || restart_after_event) hist.restart();
        bool converged = false;
        int  iters = 0;
        bool jacobian_current_this_step = false;


        if (!jacobian_valid) {
            jacobian = vm_compute_jacobian(vm, x_guess);
            vm->solver->g_dirty = 1;
            jacobian_valid = true;
            jac_alpha = alpha;
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
                if (vm_noncon_ratio(vm, x_new, x_guess, KCL_RTOL, KCL_ATOL) > KCL_VETO) {
                    step_trace("ndf2", attempt, vm->time, dt, iters, "kcl-veto");
                    x_guess = x_new;
                    continue;   // iterate on: the limiters release next time (KCL VETO, newton_core.hpp)
                }
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
            // NOTE: the history is deliberately NOT restarted here. An earlier
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
            // wrong," and the history remains perfectly valid
            // regardless of how this particular attempt's dt choice
            // turned out. Order falls back to 1 only when the history is
            // genuinely reset elsewhere (ZCD re-priming after a detected
            // discontinuity), not as a side effect of ordinary step
            // rejection.
            if (reject_run.exhausted(vm->time_step)) {
                fprintf(stderr, "[NDF2] FATAL: Failed to converge at t=%.3e\n", vm->time);
                vm_fail(vm);
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
        // .zcd: a logic output that jumps inside this step (vm/zcd.hpp). The step
        // is re-solved to end just after the jump; the one that does is
        // accepted and the next step restarts the history.
        bool event_here = false;
        if (vm->zcd_enabled) {
            const double s_ev = vm_locate_event(vm, checkpoint, x_guess, dt);
            if (s_ev > 0.0 && s_ev < 1.0 - 2.0 * ZCD_FRAC && s_ev * dt > lte_min_step(max_dt)) {
                vm_restore_state(vm, checkpoint);
                vm->time_step = s_ev * dt;
                step_trace("ndf2", attempt, vm->time, dt, iters, "zcd-cut");
                continue;
            }
            event_here = s_ev > 0.0;
        }

        // TRUNCATION ERROR (lte.hpp). The slots are evaluated at the new
        // point first: their error counts, and their commit waits for the verdict.
        vm_eval_slots_at(vm, x_guess);
        LteMethod lm = [&] { int pu = hist.usable_order(dt, 3); return pu >= 3 ? lte_ndf2() : (pu == 2 ? lte_ndf1() : lte_be()); }();
        const bool has_slots = vm->slots.size() > 0;
        LteStep ls{&hist, &x_guess, &state_rows, n_nodes, dt, rtol, atol, abstol,
                   has_slots ? &vm->slots.s_now : nullptr,
                   {&vm->slots.h0, &vm->slots.h1, &vm->slots.h2}};
        double lte_r = lte_ratio(ls, lm);
        if (lte_r < 0.0 && lm.p > 1) { lm = lte_be(); lte_r = lte_ratio(ls, lm); }
        if (lte_r > 1.0 && dt > lte_min_step(max_dt)) {
            vm_restore_state(vm, checkpoint);
            vm->time_step = dt * lte_factor(lte_r, lm);
            step_trace("ndf2", attempt, vm->time, dt, iters, "lte-reject");
            continue;
        }
        slots_commit(vm->slots);
        hist.accept(x_guess, dt);
        restart_after_event = event_here;

        solver_advance_time(vm->solver, x_guess);
        vm->time += dt;
        logger_log_step(&vm->logger, vm->time, vm->solver->sys, vm->solver->last_solution);
        if (vm->phase_log) vm->phase_log(vm);
        logger_log_signals(&vm->logger, vm->values);

        if (lte_r >= 0.0) {
            // The error estimate picks the next step; the .tran step is a ceiling.
            vm->time_step = std::min(max_dt, dt * lte_factor(lte_r, lm));
            if (iters > max_iter / 2) vm->time_step = std::min(vm->time_step, 0.8 * dt);
            step_trace("ndf2", attempt, vm->time, dt, iters, "lte");
        } else if (iters <= 5) {
            // Too few accepted points for an estimate yet (start, restart).
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