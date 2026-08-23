#include "bdf2.hpp"
#include "newton_core.hpp"
#include "newton_trust_region.hpp"
#include "step_trace.hpp"
#include "fail_dump.hpp"
#include "step_limits.hpp"
#include "../vm/vm.hpp"
#include "../vm/zcd.hpp"

#include <cstdio>
#include <cmath>

// The convergence test and the within-step Jacobian refresh policy now live in
// newton_core.hpp, shared with ndf2, trapezoidal and euler_adaptive. Both were
// written and measured here; the comments moved with the code.

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
    n_nodes = vm->solver->sys->num_nodes;

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
    RejectRun reject_run;    // bounds the halving spiral — see step_limits.hpp
    JacRefresh jac;          // within-step re-form policy — see newton_core.hpp

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
        jac.begin_step();
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
        bool stagnated = false;   // trust region gave up with a no-op step

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

        // A "PEEK INSTEAD OF UPDATE" FIX WAS TRIED HERE AND REVERTED.
        //
        // api_update_solution shifts prev_solution := last_solution, and
        // prev_solution is what nr_prev() reports. When the trust region
        // rejects a candidate x_guess does not move — and trust_region_step has
        // already restored the read-view to x_guess — so this update sets
        // prev_solution == last_solution == x_guess: nr_prev() answers with the
        // CURRENT point. Every junction limiter in the standard library is fed
        // from nr_prev() (pnjlim in the diode, both BJTs and the LED), and
        // handed v_old == v_new they take their pass-through path and limit
        // nothing. So junction limiting switches itself off for as long as the
        // trust region keeps rejecting — the hardest part of the solve, and the
        // whole reason the limiter exists.
        //
        // The reasoning survives; the fix does not. Tracking whether the
        // iterate moved and calling api_peek_solution instead of
        // api_update_solution when it did not — which leaves prev_solution
        // pointing at the last genuinely different iterate, the correct meaning
        // of "previous Newton iterate" — broke examples/BJT/npn_amp.hvr
        // outright (FATAL at t = 0, previously 3083 steps) and did not rescue
        // examples/Optoelectronics/phototransistor/optocoupler.hvr, which was
        // the deck it was written for. Evidently some models depend on the
        // limiter releasing during a rejection sequence. Do not re-apply
        // without understanding which, and why.
        for (int iter = 0; iter < max_iter; iter++) {
            iters++;

            // Within-step Jacobian refresh — the policy and the measurements
            // behind it are in newton_core.hpp. Modified Newton holds J fixed at
            // x_prev1 for the whole step, which is wrong for a device whose
            // conductance moves orders of magnitude between the guess and the
            // solution, and dt is not the knob that fixes it.
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

            // Opens the refresh window. Note this reads the ratio left by the
            // PREVIOUS convergence test — the last one of the previous attempt,
            // since this runs before this iteration's test. That is what was
            // measured and tuned against; it makes the first window's baseline
            // a carried-over number rather than this step's own opening ratio.
            if (iter == 0) jac.seed(conv.worst_ratio);

            if (newton_converged(x_new, x_guess, n_nodes, rtol, atol, abstol, conv)) {
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
                stagnated = tr.stagnated;
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
            // a full O(nodes) perturbation sweep, and stagnation turns out to
            // be common enough on a healthy BJT step that the cost compounds.
            // Reverted; the tolerant accept test in trust_region_step is kept,
            // since a no-op step being read as failure is wrong regardless.
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
            vm->time_step *= 0.5;
            step_count = 0;
            step_trace("bdf2", attempt, vm->time, dt, iters, "reject");
            if (reject_run.exhausted(vm->time_step)) {
                fprintf(stderr, "[BDF2] FATAL: Failed to converge at t=%.3e\n", vm->time);
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
        // Re-forming per step costs one O(nodes) perturbation sweep and pays
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