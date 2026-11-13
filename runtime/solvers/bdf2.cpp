#include "bdf2.hpp"
#include "newton_core.hpp"
#include "newton_trust_region.hpp"
#include "step_trace.hpp"
#include "lte.hpp"
#include "fail_dump.hpp"
#include "step_limits.hpp"
#include "../vm/vm.hpp"
#include "../vm/zcd.hpp"

#include <cstdio>
#include <cstdlib>
#include <cmath>
#include <algorithm>

// The convergence test and the within-step Jacobian refresh policy now live in
// newton_core.hpp, shared with ndf2, trapezoidal and euler_adaptive. Both were
// written and measured here; the comments moved with the code.

// ─────────────────────────────────────────────────────────────────────────────
// RUN
//
// Mirrors Go: func (s *BDF2) Run(vm *VM)
// 2nd order BDF on the actual (variable) step grid; BDF1 when there is no
// usable history. Coefficients: bdf_history.hpp.
// ─────────────────────────────────────────────────────────────────────────────

void BDF2::run(VM *vm) {
    if (max_dt == 0.0) max_dt = vm->time_step;
    n_nodes = vm->solver->sys->num_nodes;

    int n = (int)vm->solver->last_solution.size();
    hist.reset(vm->solver->last_solution);
    const LteRows state_rows = lte_state_rows(*vm->solver->sys);

    Eigen::VectorXd correction(n);
    Eigen::VectorXd x_blend(n);

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
    double jac_alpha = -1.0;   // alpha the held Jacobian was formed under

    long attempt = 0;   // counts ATTEMPTS, not accepted steps — see step_trace.hpp
    bool plain_newton = false;          // this attempt is the plain-Newton retry
    bool restart_after_event = false;   // the last step ended on a .zcd event
    RejectRun reject_run;    // bounds the halving spiral — see step_limits.hpp
    LteFloorRun lte_floor;   // ends a run stuck at the step floor — see lte.hpp
    JacRefresh jac;          // within-step re-form policy — see newton_core.hpp
    EventStepCap event_cap;  // first step after an event — see step_limits.hpp

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

        vm->analog_dt = dt;   // analog code sees t_n + dt (vm.hpp)
        jac.begin_step();
        Eigen::VectorXd x_guess = hist.y[0];

        // The step's logic pass comes first: whether this step must restart
        // the history depends on what it did. A clock tick restarts it only if
        // it changed what the logic drives (vm->tick_changed, item 13); a step
        // that follows a .zcd event always does.
        api_update_solution(vm->api, x_guess);
        vm_run_digital(vm);
        vm_run_phase_b(vm);
        if (vm->tick_changed || restart_after_event) {
            hist.restart();
            dt = event_cap.apply(dt, vm->time_step, 2.0 * lte_min_step(max_dt), vm->time);
            vm->solver->sys->dt = dt;
            vm->analog_dt = dt;   // analog code sees t_n + dt (vm.hpp)
        }

        bool converged   = false;
        double alpha;
        double w[3];
        hist.bdf_recipe(hist.usable_order(dt, 2), dt, alpha, w);
        hist.blend_of(w, x_blend);
        // ddt/idt slots get the same recipe (vm/state_slots.hpp). alpha*dq/dV
        // is part of the Jacobian, so a Jacobian formed under another alpha is
        // re-formed — only when the deck has slots, so nothing else changes.
        slots_begin(vm->slots, alpha, w, 0.0);
        if (vm->slots.size() > 0 && alpha != jac_alpha) jacobian_valid = false;

        int iters = 0;
        bool jacobian_current_this_step = false;

        if (!jacobian_valid) {
            jacobian = vm_compute_jacobian(vm, x_guess);
            vm->solver->g_dirty = 1;
            jacobian_valid = true;
            jac_alpha = alpha;
            jacobian_current_this_step = true;
        }

        TrustRegionState trust; // default radius=1.0; per-component scaling does the real adaptation

        // NOT INSTALLING A REJECTED ITERATE — HISTORY.
        //
        // api_update_solution shifts prev_solution := last_solution, and
        // prev_solution is what nr_prev() reports. Installing x_guess again after
        // a rejection therefore made nr_prev() answer with the CURRENT point.
        // Skipping that install was tried once before and reverted: with the
        // junction limiters then anchored on nr_prev(V(a) - V(b)) it broke
        // examples/BJT/npn_amp.hvr (FATAL at t = 0) — those models relied on the
        // limiter releasing during a rejection sequence. The standard library
        // now anchors every limiter on its own output, nr_prev(vl), and with
        // that the skip is right: npn_amp, pnp_amp, optocoupler and rectifier
        // are bit-identical with it, and the diode bridges lose a sixth of
        // their Newton iterations (the anchor no longer walks while Newton
        // stands still). A deck that still limits from nr_prev(V(...)) keeps a
        // LIMITED junction through a rejection sequence where it used to be
        // released.
        // A trust-region rejection leaves x_guess where it was. That is not a new
        // Newton iterate, so it is not installed again: installing it made the
        // previous iterate equal the current one, and everything read through
        // nr_prev() then moved although Newton had not — a junction limiter
        // anchored on its own output took one more step per rejection.
        bool iterate_moved = true;
        for (int iter = 0; iter < max_iter; iter++) {
            iters++;

            // Within-step Jacobian refresh — the policy and the measurements
            // behind it are in newton_core.hpp. Modified Newton holds J fixed at
            // x_prev1 for the whole step, which is wrong for a device whose
            // conductance moves orders of magnitude between the guess and the
            // solution, and dt is not the knob that fixes it.
            if (!plain_newton && jac.due(iter, conv.worst_ratio)) {
                jacobian = vm_compute_jacobian(vm, x_guess);
                vm->solver->g_dirty = 1;
                jacobian_current_this_step = true;
            }

            if (iterate_moved) api_update_solution(vm->api, x_guess);
            iterate_moved = true;   // every path but a rejection moves it
            // Plain Newton forms J AFTER the update, so the Jacobian and the residual
            // see the same nr_prev and the limiters linearise both at one point
            // (newton_trust_region.hpp, PLAIN-NEWTON RETRY).
            if (plain_newton) {
                jacobian = vm_compute_jacobian(vm, x_guess);
                vm->solver->g_dirty = 1;
                jacobian_current_this_step = true;
            }
            vm_run_analog(vm);
            vm_run_phase_b(vm);

            correction = jacobian * x_guess;

            const Eigen::VectorXd &x_new = solver_solve_rhs(
                vm->solver, 1.0, x_blend, alpha, &correction, &jacobian);

            // Opens the refresh window. Note this reads the ratio left by the
            // PREVIOUS convergence test — the last one of the previous attempt,
            // since this runs before this iteration's test. That is what was
            // measured and tuned against; it makes the first window's baseline
            // a carried-over number rather than this step's own opening ratio.
            if (iter == 0) jac.seed(conv.worst_ratio);

            if (newton_converged(x_new, x_guess, n_nodes, rtol, atol, abstol, conv)) {
                if (vm_noncon_ratio(vm, x_new, x_guess, KCL_RTOL, KCL_ATOL) > KCL_VETO) {
                    step_trace("bdf2", attempt, vm->time, dt, iters, "kcl-veto");
                    x_guess = x_new;
                    continue;   // iterate on: the limiters release next time (KCL VETO, newton_core.hpp)
                }
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
            if (plain_newton) { x_guess = x_new; continue; }   // retry mode: undamped (newton_trust_region.hpp)
            TrustRegionResult tr = trust_region_step(
                vm, trust, x_guess, x_new, alpha, x_blend, nullptr);
            iterate_moved = tr.accepted;
            if (tr.collapsed) {
                break;
            }
            if (tr.accepted) {
                x_guess = tr.x_candidate;
            }
        }

        if (converged) plain_newton = false;

        if (!converged) {
            // Documented rule: "Should this happen and the Jacobian not be
            // current, a new Jacobian is formed. Otherwise the step size
            // is reduced." The Jacobian is never blamed twice in the same
            // step — if it was already (re)formed this step and STILL
            // failed, only dt is reduced.
            // NOTE — ALPHA CONTINUATION WAS TRIED HERE AND DOES NOT WORK.
            //
            // The idea: alpha is the integration formula's coefficient in the
            // companion model ((G + alpha*C)x = B_static + alpha*C*x_hist), it
            // scales as 1/dt, and so it — not dt directly — is what makes a
            // stiff step stiff. Rather than shrink dt, solve a ladder of easier
            // problems at the same instant and carry each solution forward as
            // the next initial guess, accepting only the final rung at the true
            // alpha. Pseudo-transient continuation, same shape as vm_solve_op's
            // source stepping.
            //
            // Both directions were implemented and measured on the full-bridge
            // rectifier, and both fail on their FIRST rung:
            //
            //   alpha DOWNWARD (1e4x -> 1x): the first rung is heavily damped,
            //     so x should be pinned at x_history and nearly free to solve.
            //     But a large alpha is exactly the alpha*C domination that
            //     breaks this circuit in the first place — it reproduces the
            //     tiny-dt failure rather than avoiding it.
            //
            //   alpha UPWARD (1e-4x -> 1x): a small alpha does not give an
            //     easier nearby problem, it gives a DIFFERENT one. With the
            //     capacitors barely coupled, dc_out is no longer held near
            //     19.67 V, so the operating point moves wholesale and every
            //     diode changes state.
            //
            // There is no good starting rung between those two walls, so the
            // technique cannot get traction on this failure. Do not re-add it
            // without a starting point that is both well-conditioned AND near
            // the true solution.
            vm_restore_state(vm, checkpoint);

            // A STAGNATION RESPONSE WAS TRIED HERE AND REVERTED.
            //
            // The trust region reports `stagnated` when it gave up having
            // shrunk the step to a no-op rather than because candidates were
            // getting worse. The reasoning was that this points at a stale
            // search direction, not at dt, so the right answer is to re-form
            // the Jacobian (bounded to two extra re-forms) instead of halving.
            //
            // Measured: it made examples/BJT/npn_amp.hvr, which runs in 3197
            // steps, fail to finish at all within 200 s. Each extra re-form is
            // a full O(nodes) perturbation sweep (the Jacobian was all finite
            // differences then — before 0.9's AD), and stagnation turns out to
            // be common enough on a healthy BJT step that the cost compounds.
            // Reverted; the tolerant accept test in trust_region_step is kept,
            // since a no-op step being read as failure is wrong regardless.
            if (!jacobian_current_this_step) {
                // Attribute the failure to the Jacobian, not the step
                // size: invalidate it so the next attempt re-forms it,
                // and retry at the SAME dt and the SAME history (hist
                // untouched) — this is a Jacobian retry,
                // not a step rejection, so BDF2's multistep history should
                // not be discarded.
                jacobian_valid = false;
                step_trace("bdf2", attempt, vm->time, dt, iters, "jac-refresh");
                continue;
            }
            if (!plain_newton) {
                // One retry at this dt as plain Newton (newton_trust_region.hpp).
                plain_newton = true;
                step_trace("bdf2", attempt, vm->time, dt, iters, "plain-newton");
                continue;
            }
            plain_newton = false;
            // Jacobian was already current and still failed — the failure is
            // genuinely about the step size now.
            //
            // A RESIDUAL-BASED ACCEPTANCE WAS TRIED HERE AND REVERTED.
            //
            // The motivation is sound and the measurement behind it stands: the
            // step test asks whether the iterate stopped moving, which is not
            // the question of whether the circuit is solved, and the two come
            // apart wherever the Jacobian barely constrains a direction. At a
            // bridge rectifier's commutation the source-side nodes are held
            // only by a barely-conducting diode (~3e-7 S) and a 1 MOhm
            // reference; measured there, EVERY equation was satisfied to
            // ||r||_inf = 3.0e-6 while the blocking node's Newton step was
            // 1.776 V against a 1.97 mV tolerance. residual_converged() in
            // newton_trust_region.hpp implements that check and is kept for
            // diagnostics.
            //
            // Accepting on it is what does not work, at either placement:
            //
            //   inside the Newton loop (from iteration 8): fired 62 times,
            //     accepted non-solutions and walked the run off course into
            //     ||r||_inf = 22 A, failing EARLIER than before.
            //
            //   here, only where the step would otherwise be abandoned: the
            //     rectifier improved a lot — correct waveform, 12.10..22.53 V,
            //     surviving to t = 46.8 ms instead of 16.95 ms — but it broke
            //     examples/BJT/npn_amp.hvr outright (FATAL at t = 1e-6 after 4
            //     steps, previously 3197 steps) and shifted pnp_amp's gain from
            //     62.70 to 61.76.
            //
            // So the criterion accepts points that are not solutions, and the
            // tolerance that would separate the two is knife-edge: at
            // 1e-3*scale it is too loose, and at 1e-5*scale it rejects the very
            // rectifier step it was written to rescue. That narrow a margin
            // means this needs a better-founded test, not a tuned constant.
            reject_run.note(dt);
            // Back off from the step that FAILED, not from vm->time_step: the
            // restore above put back the checkpoint's preferred step, which is
            // larger whenever this step was shortened (a clock tick, the first
            // step after an event — step_limits.hpp).
            vm->time_step = 0.5 * dt;
            // Restart at BDF1. With the variable-step coefficients
            // (bdf_history.hpp) a halved step would still be a valid BDF2 step,
            // and dropping the restart was tried — it broke
            // examples/DCMotor/h_bridge.hvr (FATAL at t = 202 ms). There a
            // rejection marks a discontinuity the solver was not told about
            // (LoadTorque's friction jump as omega crosses zero, sorunlar
            // item 14), and BDF2 extrapolating history from across that jump
            // keeps failing where BDF1 recovers. ndf2 does not restart here;
            // see its note for the cost on smooth circuits.
            hist.restart();
            step_trace("bdf2", attempt, vm->time, dt, iters, "reject");
            if (reject_run.exhausted(vm->time_step)) {
                fprintf(stderr, "[BDF2] FATAL: Failed to converge at t=%.3e\n", vm->time);
                vm_fail(vm);
                dump_convergence_row(vm, conv.worst_row, conv.worst_step, conv.worst_tol);
                dump_failure(vm, "bdf2", dt, alpha, x_guess, x_blend);
                break;
            }
            continue;
        }

        // THE JACOBIAN IS RE-FORMED ONCE PER ACCEPTED STEP.
        //
        // Without this line `jacobian_valid` is cleared only when a step FAILS,
        // so one Jacobian stays alive for as long as steps keep succeeding.
        // Measured on examples/Diode/rectifier.hvr before the fix: the Jacobian
        // was formed 6 times before t = 207 us and then not again until
        // t = 1.182e-2 — 311 consecutive accepted steps, over half a mains
        // cycle including a full commutation of all four bridge diodes, all
        // solved against a matrix built when the circuit was in an entirely
        // different state. A diode's conductance moves from 1e-12 S to ~1 S
        // across a commutation and the Jacobian is precisely the matrix of
        // those conductances.
        //
        // Why that hid for so long: the answers stayed CORRECT. The iteration
        // (G + alpha*C + J)x_new = B(x_guess) + J*x_guess is a fixed point
        // whose fixed point is the true solution for ANY invertible J — only
        // the RATE depends on J being right. So the symptom was 15-30
        // iterations per step where 3-8 should do, with the residual falling by
        // a constant ~0.75 per iteration instead of quadratically. At a hard
        // commutation that factor crosses 1, the step diverges, and halving dt
        // cannot help because dt was never what was wrong: the trace showed 28
        // successive halvings from 1e-4 down to 1.5e-12, every one of them
        // hitting the iteration ceiling.
        //
        // Re-forming per step costs one Jacobian — an O(nodes) perturbation
        // sweep when this was measured, one AD evaluation now — and pays
        // for itself several times over. Measured, Newton iterations for a
        // whole run: npn_amp 16561 -> 6344, pnp_amp 18642 -> 5443, and the
        // rectifier goes from failing at t = 1.182e-2 to completing all 100 ms
        // with dc_out at 12.09 .. 22.53 V, matching the 12.02 .. 22.53 V that
        // the deck documents as correct.
        //
        // A CONVERGENCE-RATE MONITOR WAS TRIED HERE FIRST AND DID NOT WORK.
        // The header quotes ode15s's rule — terminate the iteration when the
        // observed contraction rho = eta_k/eta_{k-1} predicts convergence will
        // not arrive within a few iterations, and re-form the Jacobian when
        // that happens. Implemented faithfully (with ode15s's own
        // max(0.9*rate, rho) smoothing), it bailed at iteration 2 on npn_amp's
        // very first step and never let the run start. The reason is structural
        // rather than a bad threshold: Newton's opening iterations are
        // superlinear, eta falls by many orders at once, and a geometric
        // extrapolation from two such samples always predicts hopelessness. The
        // rate only becomes meaningful after the iteration has settled — by
        // which point, with a per-step Jacobian, there is nothing left to
        // detect. Do not re-add it without solving the warm-up problem first.
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
                event_cap.note_cut(dt);
                vm->time_step = s_ev * dt;
                step_trace("bdf2", attempt, vm->time, dt, iters, "zcd-cut");
                continue;
            }
            event_here = s_ev > 0.0;
        }

        // TRUNCATION ERROR (lte.hpp). The slots are evaluated at the new
        // point first: their error counts, and their commit waits for the verdict.
        vm_eval_slots_at(vm, x_guess);
        LteMethod lm = hist.usable_order(dt, 2) >= 2 ? lte_bdf2() : lte_be();
        const bool has_slots = vm->slots.size() > 0;
        LteStep ls{&hist, &x_guess, &state_rows, n_nodes, dt, rtol, atol, abstol,
                   has_slots ? &vm->slots.s_now : nullptr,
                   {&vm->slots.h0, &vm->slots.h1, &vm->slots.h2}};
        double lte_r = lte_ratio(ls, lm);
        if (lte_r < 0.0 && lm.p > 1) { lm = lte_be(); lte_r = lte_ratio(ls, lm); }
        if (lte_r > 1.0 && dt > lte_min_step(max_dt)) {
            vm_restore_state(vm, checkpoint);
            vm->time_step = lte_next_step(dt, lte_r, lm, max_dt);
            step_trace("bdf2", attempt, vm->time, dt, iters, "lte-reject");
            continue;
        }
        slots_commit(vm->slots);
        hist.accept(x_guess, dt);
        restart_after_event = event_here;

        solver_advance_time(vm->solver, x_guess);
        vm->time += dt;
        vm->analog_dt = 0.0;
        logger_log_step(&vm->logger, vm->time, vm->solver->sys, vm->solver->last_solution);
        if (vm->phase_log) vm->phase_log(vm);
        logger_log_signals(&vm->logger, vm->values);

        if (lte_r >= 0.0) {
            if (lte_floor.exhausted(dt, lte_r, max_dt)) {
                fprintf(stderr, "[BDF2] FATAL: timestep too small at t=%.3e: the truncation error "
                                "stayed over tolerance at the minimum step (%.1e s)\n", vm->time, lte_min_step(max_dt));
                vm_fail(vm);
                break;
            }
            // The error estimate picks the next step; the .tran step is a ceiling,
            // lte_min_step the floor.
            vm->time_step = lte_next_step(dt, lte_r, lm, max_dt);
            if (iters > max_iter / 2) vm->time_step = std::min(vm->time_step, 0.8 * dt);
            step_trace("bdf2", attempt, vm->time, dt, iters, "lte");
        } else if (iters <= 5) {
            // Too few accepted points for an estimate yet (start, restart).
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