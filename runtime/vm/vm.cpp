#include "vm.hpp"
#include "zcd.hpp"
#include "../solvers/newton_trust_region.hpp"
#include "../solvers/step_limits.hpp"
#include <cmath>
#include <cstdio>

// ─────────────────────────────────────────────────────────────────────────────
// LIFECYCLE
// ─────────────────────────────────────────────────────────────────────────────

void vm_init(VM *vm, API *api, Solver *solver) {
    vm->api              = api;
    vm->solver           = solver;
    vm->time             = 0.0;
    vm->time_step        = 1e-6;
    vm->end_time         = 0.01;
    vm->zcd_enabled      = 0;
    vm->op_enabled       = 0;
    vm->strategy         = nullptr;
    vm->phase_structural = nullptr;
    vm->phase_digital    = nullptr;
    vm->phase_analog     = nullptr;
    vm->phase_b          = nullptr;
    vm->phase_log        = nullptr;
    vm->save_state_vars    = nullptr;
    vm->restore_state_vars = nullptr;
    int n = solver->sys->size;
    vm->jacobian_scratch = Eigen::MatrixXd::Zero(n, n);
    vm->base_b_scratch   = Eigen::VectorXd::Zero(n);
}

// ─────────────────────────────────────────────────────────────────────────────
// PHASE EXECUTION
// Called by solvers each timestep. Function pointers set by generated sim.cpp.
// ─────────────────────────────────────────────────────────────────────────────

void vm_run_digital(VM *vm) {
    if (vm->phase_structural) vm->phase_structural(vm);
    if (vm->phase_digital)    vm->phase_digital(vm);
}

void vm_run_analog(VM *vm) {
    if (vm->phase_analog) vm->phase_analog(vm);
}

void vm_run_phase_b(VM *vm) {
    if (vm->phase_b) vm->phase_b(vm);
}

// ─────────────────────────────────────────────────────────────────────────────
// JACOBIAN
// Numerical dI/dV estimate via 1µV node perturbation.
// Used by implicit solvers (BDF2, TR-BDF2, etc.)
// ─────────────────────────────────────────────────────────────────────────────

const Eigen::MatrixXd& vm_compute_jacobian(VM *vm, Eigen::VectorXd &x_guess) {
    int n = vm->solver->sys->size;

    // MATLAB's ode15s/ode23tb use a RELATIVE perturbation step for their
    // numerical Jacobians by default: delta_i = sqrt(eps) * max(|x_i|, 1.0)
    // per variable, not one fixed absolute step for the entire vector.
    //
    // The previous fixed delta_v = 1e-6 (an absolute volt/amp/radian
    // value applied uniformly to every node) breaks down badly for any
    // node whose natural scale is far from "around 1e-6" — e.g.
    // theta_e_node in a PMSM model, which integrates an electrical angle
    // and can reach hundreds of radians within milliseconds. Perturbing
    // a 440-radian value by 1e-6 is a relative change of about 2e-9 —
    // nine orders of magnitude smaller than the value itself — which is
    // exactly the regime where floating-point cancellation in
    // (B_static_perturbed - B_static_baseline) produces a numerically
    // garbage derivative estimate, even though cos()/sin() themselves
    // remain accurate at that magnitude (verified separately — this is
    // NOT a trig-precision problem, it's a perturbation-step-size
    // problem).
    //
    // This fix matches MATLAB's documented, standard practice and is a
    // genuine correctness improvement on its own — but tested directly
    // against a real PMSM/FOC motor-control circuit that was failing to
    // converge under TR-BDF2, it did NOT resolve that specific failure
    // (the stall point was identical with and without this fix). That
    // means the PMSM stall has a different root cause than a poorly
    // scaled numerical Jacobian perturbation — this fix is kept because
    // it is correct and matches documented best practice, not because it
    // was confirmed to fix that particular circuit's instability.
    //
    // sqrt(eps) ≈ 1.49e-8 for IEEE double precision (eps ≈ 2.22e-16) is
    // the standard, textbook-derived optimal relative step size for
    // central-difference-free (one-sided) numerical differentiation —
    // small enough to approximate the true derivative well, large
    // enough to avoid being swamped by floating-point rounding noise.
    // The max(|x_i|, 1.0) floor matches MATLAB's own convention: for a
    // node sitting at or near zero, fall back to a step of sqrt(eps)
    // absolute rather than letting the step collapse to zero.
    const double sqrt_eps = 1.4901161193847656e-8; // sqrt(2.22e-16), IEEE double

    vm->jacobian_scratch.setZero();

    // Baseline. Peek (read-view only): the Jacobian is a probe, so none of
    // these perturbation evaluations may advance prev_solution / nr_prev().
    api_peek_solution(vm->api, x_guess);
    vm_run_analog(vm);
    vm_run_phase_b(vm);
    vm->base_b_scratch = vm->solver->sys->B_static;

    for (int i = 0; i < n; i++) {
        double orig = x_guess(i);

        // Per-node relative perturbation — this is the actual fix.
        double scale = std::abs(orig);
        if (scale < 1.0) scale = 1.0;
        double delta_v = sqrt_eps * scale;

        x_guess(i) += delta_v;

        api_peek_solution(vm->api, x_guess);
        vm_run_analog(vm);
        vm_run_phase_b(vm);

        for (int j = 0; j < n; j++) {
            double delta_i = vm->solver->sys->B_static(j) - vm->base_b_scratch(j);
            if (delta_i != 0.0) {
                vm->jacobian_scratch(j, i) = -(delta_i / delta_v);
            }
        }

        x_guess(i) = orig;
    }

    // Restore baseline. Peek again so last_solution is left pointing at the
    // real current iterate (x_guess) with prev_solution untouched — the
    // caller's next api_update_solution then shifts the correct value into
    // prev_solution for nr_prev().
    api_peek_solution(vm->api, x_guess);
    vm_run_analog(vm);
    vm_run_phase_b(vm);

    return vm->jacobian_scratch;
}

// ─────────────────────────────────────────────────────────────────────────────
// DC OPERATING POINT
// ─────────────────────────────────────────────────────────────────────────────

static int op_converged(const Eigen::VectorXd &x_new,
                        const Eigen::VectorXd &x_old,
                        double rtol, double atol)
{
    for (int i = 0; i < x_new.size(); i++) {
        double scale = atol + rtol * std::abs(x_new(i));
        // NEGATED, deliberately — see the identical note in BDF2::has_converged.
        // `> scale` is false for NaN, so the plain form REPORTS A NaN ITERATE AS
        // CONVERGED: every alpha rung would "complete" on its first iteration
        // and hand a NaN operating point to the transient, which is exactly what
        // examples/BJT/opamp.hvr did.
        if (!(std::abs(x_new(i) - x_old(i)) <= scale)) return 0;
    }
    return 1;
}

// vm_solve_dc — the DC operating point, used by BOTH boot paths (with and
// without the .op directive). Newton with a fresh numerical Jacobian per
// iteration, damped by the same diagonally-scaled trust region BDF2 uses,
// walking a source-stepping ladder in alpha.
//
// THE JACOBIAN IS REQUIRED HERE, not an optimization. In Hover a junction
// contributes nothing to G: a diode is `R<Rs>` from anode to `ai` and then a
// `current_source` from `ai` to cathode (stdlib/semiconductors/diode.hvr),
// and current sources stamp only the RHS. Even GMIN is inside that
// current-source expression — `i_d = ... + 1.0e-12 * vd` — so the model's
// own header is literal when it says "GMIN closes the loop on the Jacobian".
// Without a Jacobian every junction in the circuit is an OPEN CIRCUIT during
// .op, and any node whose only tie to the netlist runs through junctions is
// structurally floating. Measured on examples/BJT/opamp.hvr: matrix exactly
// singular, null space on `opamp.d_mid` + `opamp.d2.d.ai` at 0.7071 each.
//
// THE ALPHA LADDER IS APPLIED AT STAMP TIME, not by rewriting B_static rows.
// The previous arrangement captured b_full once at x=0 and re-wrote branch
// rows after each phase_b — but every Jacobian and residual probe re-runs
// phase_b, and api_set_voltage_source is an ABSOLUTE overwrite, so the probe
// machinery silently undid the scaling: driven sources (sine, LED light bus)
// sat at full strength from alpha=0.05 while static sources ramped. Now
// stamp_voltage_source, api_set_voltage_source and the static-row registry
// all multiply by sys->source_alpha, so alpha is consistent on every
// evaluation path. Device-internal driven voltage sources (LED light bus,
// BJT rbx, PMSM back-EMF) ramp too — harmless: the alpha=1.0 rung is exact
// and intermediate rungs are only a homotopy path.
//
// THE DAMPER IS THE ±0.05 PER-COMPONENT ALIAS CAP, KEPT BLIND.
//
// Two smarter dampers were tried here first and both were reverted with the
// decks as witnesses:
//
//   trust_region_step's diagonally-scaled radius (as BDF2 uses): its norm is
//   measured per-row against max(|x_i|, 1e-6), so a node near 0 V may jump
//   ~1e6 V at radius 1.0 — and a from-zero DC solve STARTS with nodes at
//   0 V. Measured on examples/BJT/opamp.hvr: rung 0.10's first step flew to
//   1.8e5 V, past the stdlib jexp guard (x>40) where a junction's Jacobian
//   column is EXACTLY zero, making G+J singular; later rungs LU-solved to
//   NaN and collapsed. BDF2's guess is the previous trajectory point, so it
//   never sees this; a from-zero DC solve does.
//
//   residual-scored capped steps (strict decrease, then a 5% uphill band):
//   the raw residual mixes units and scales wildly (h_bridge rows span
//   equilibration scales 2^1..2^39), so scoring rejected steps that would
//   have converged, and on h_bridge the shrunken cap FROZE x_guess into a
//   non-solution that every later rung then "converged" from (op_converged
//   tests step size, not residual) — a worse failure than the cap's.
//
// The reason the blind cap is now sufficient: its known failure, period-2
// limit cycles on the LED decks (constant worst step 2.097e-01 forever),
// needed kilovolt steps to truncate, and those existed only because driven
// sources sat at FULL strength from alpha=0.05 (the stamp-restamping bug
// fixed above). With the ladder reaching driven sources, redled/
// multiple_leds/opamp/npn/pnp/rectifier all converge every rung under the
// blind cap. If cycling ever reappears, the fix is a scored cap on a
// WELL-SCALED residual, not the raw one.
//
// Solver alpha = 1/time_step with a ZERO history anchor, exactly as the old
// loop did. A purist would use alpha=0 — a DC solve has no capacitance term,
// so G_eff = G + J solves the literal DC system Gx = B. That was tried here
// and reverted: decks whose models stamp hidden ddt companion inductances
// (h_bridge's BJTs) get a physically-wrong DC bias from the literal system
// (at alpha=0 an inductor branch row degenerates to V(p)=V(n), a short,
// which is not the same bias the transient met), and h_bridge's BDF2 then
// failed 17us in instead of running 198ms. The 1/dt alpha*C diagonal is
// damping the old loop implicitly relied on; keep it, keep zero history.
static bool vm_solve_dc_once(VM *vm) {
    int n = vm->solver->sys->size;
    Eigen::VectorXd zero_history = Eigen::VectorXd::Zero(n);
    Eigen::VectorXd x_guess      = Eigen::VectorXd::Zero(n);
    Eigen::MatrixXd jacobian;
    Eigen::VectorXd correction(n);
    int    op_iters = 0, op_worst_row = -1, op_failed = 0;
    double op_worst = 0.0;

    // Decks with no voltage sources at all have nothing to ramp — skip the
    // ladder and solve the full-strength problem directly.
    static const double ladder[] = {
        0.05, 0.1, 0.15, 0.2, 0.3, 0.4, 0.5, 0.6, 0.7, 0.8, 0.9, 1.0
    };
    static const double single[] = { 1.0 };
    const bool   rampable = !vm->solver->sys->voltage_source_stamps.empty();
    const double *alphas  = rampable ? ladder : single;
    const int    n_alphas = rampable ? 12 : 1;

    vm->time = 0.0;
    api_update_solution(vm->api, x_guess);
    vm_run_digital(vm);
    vm_run_analog(vm);
    vm_run_phase_b(vm);

    for (int k = 0; k < n_alphas; k++) {
        double alpha = alphas[k];
        system_set_source_alpha(vm->solver->sys, alpha);

        op_iters = 0;
        bool rung_converged = false;
        for (int iter = 0; iter < 500; iter++) {
            op_iters = iter + 1;
            // update, NOT peek: pnjlim/nr_prev semantics require that a
            // rejected candidate leaves prev_solution == current iterate —
            // the "limiter releases during rejection" behaviour some stdlib
            // models depend on (tried-and-reverted record: bdf2.cpp:171-194).
            api_update_solution(vm->api, x_guess);
            vm_run_analog(vm);
            vm_run_phase_b(vm);

            jacobian = vm_compute_jacobian(vm, x_guess);
            // J folds into G_eff only inside solver_factorize
            // (engine.cpp:44-48), and last_alpha does not change within a
            // rung — without g_dirty the LU would reuse a stale Jacobian.
            vm->solver->g_dirty = 1;
            correction = jacobian * x_guess;

            const Eigen::VectorXd &x_new = solver_solve_rhs(
                vm->solver, 1.0, zero_history, /*alpha*/1.0 / vm->time_step,
                &correction, &jacobian);

            if (op_converged(x_new, x_guess, 1e-3, 1e-6)) {
                x_guess = x_new;
                rung_converged = true;
                break;
            }
            op_worst = 0.0; op_worst_row = -1;
            for (int i = 0; i < n; i++) {
                double d2 = std::abs(x_new(i) - x_guess(i));
                if (!(d2 <= op_worst)) { op_worst = d2; op_worst_row = i; }
            }

            // The ±0.05 per-component alias cap, accepted BLINDLY as before.
            //
            // Residual-scored variants (strict decrease, then a 5% uphill
            // band) were tried here and rejected: the raw residual mixes
            // units and scales wildly (h_bridge's rows span equilibration
            // scales 2^1..2^39), so scoring failed decks the blind cap
            // passes (h_bridge worst-rung walked INTO a frozen non-solution
            // state — op_converged checks step size, not residual, and a
            // shrunken cap makes every later rung "converge" falsely — while
            // protecting nothing: the period-2 limit cycles this scoring was
            // meant to break needed kilovolt steps to truncate, and those
            // only existed because driven sources sat at FULL strength from
            // alpha=0.05. With the alpha ladder applied at stamp time, the
            // cycling precondition is gone — redled/multiple_leds converge
            // their OP cleanly under the blind cap. limitcycle.
            for (int i = 0; i < n; i++) {
                double delta = x_new(i) - x_guess(i);
                if (delta >  0.05) delta =  0.05;
                if (delta < -0.05) delta = -0.05;
                x_guess(i) += delta;
            }
        }
        // Report the truth. This used to print "complete" unconditionally,
        // whether the rung had converged in three iterations or exhausted all
        // 500 without ever getting close — so a failed operating point was
        // indistinguishable from a good one, and the transient inherited it
        // either way with nothing on stderr to say so. A trust-region
        // collapse is reported the same way: it IS a non-converged rung.
        if (!rung_converged) {
            fprintf(stderr, "[VM] OP alpha=%.2f DID NOT CONVERGE after %d iterations "
                            "(worst step %.3e on row %d)\n",
                    alpha, op_iters, op_worst, op_worst_row);
            op_failed = 1;
        }
        printf("[VM] OP alpha=%.2f complete (%d iters)\n", alpha, op_iters);
    }

    // Unconditional, and before the commit restamps: the transient's per-step
    // api_set_voltage_source calls must stamp at full strength. Nothing in
    // the loop above returns early or throws, so this cannot leak.
    system_set_source_alpha(vm->solver->sys, 1.0);

    api_update_solution(vm->api, x_guess);
    vm_run_analog(vm);
    vm_run_phase_b(vm);
    solver_advance_time(vm->solver, x_guess);
    return !op_failed;
}

// ─────────────────────────────────────────────────────────────────────────────
// DC OPERATING POINT — RETRY LADDER
//
// vm_solve_dc_once solves at alpha = 1/vm->time_step. That alpha is not a
// formality: the note above vm_solve_dc_once records that dropping the alpha*C
// diagonal entirely (the "purist" alpha = 0 DC system) gave h_bridge a
// physically wrong bias and made its transient fail 17 us in instead of running
// 198 ms. A larger alpha is more of that same damping.
//
// So a rung that will not converge has an obvious second thing to try before
// giving up: halve the step, doubling alpha, and solve again. This is the exact
// counterpart of what the transient loop already does on a rejected step, and
// until now the OP was the one place in the runtime that failed on its first
// attempt with no back-off at all — it printed a warning and handed an
// unconverged bias to the transient, which then had to discover the problem for
// itself several steps later and much less legibly.
//
// Four extra tries is 16x the starting alpha. The ceiling exists because this
// is genuinely a different question from the transient's back-off: there, a
// smaller dt is also a physically finer answer, so grinding down is defensible.
// Here every rung solves the SAME DC problem and dt is nothing but a damping
// parameter, so a ladder that keeps going is not converging on anything — it is
// just making the companion capacitance dominate until the answer is "x equals
// its own history", which is vacuous. If 16x has not helped, the bias point is
// wrong for a reason damping cannot fix.
//
// vm->time_step is restored afterwards no matter what. The transient's own
// controller owns it from here, and leaving a rung's damping value behind would
// silently rescale the first step of the run.
// ─────────────────────────────────────────────────────────────────────────────
static const int OP_DAMPING_RETRIES = 4;

static void vm_solve_dc(VM *vm) {
    const double dt_restore = vm->time_step;

    for (int attempt = 0; attempt <= OP_DAMPING_RETRIES; attempt++) {
        if (vm_solve_dc_once(vm)) {
            if (attempt > 0) {
                printf("[VM] OP converged after %d damping retr%s (dt %.3e -> %.3e)\n",
                       attempt, attempt == 1 ? "y" : "ies", dt_restore, vm->time_step);
            }
            vm->time_step = dt_restore;
            printf("[VM] OP complete\n");
            return;
        }
        if (attempt < OP_DAMPING_RETRIES) {
            vm->time_step *= 0.5;
            fprintf(stderr, "[VM] OP did not converge — retrying with more damping "
                            "(dt %.3e, alpha %.3e)\n", vm->time_step, 1.0 / vm->time_step);
        }
    }

    vm->time_step = dt_restore;
    fprintf(stderr, "[VM] WARNING: DC operating point did not converge after %d "
                    "damping retries; the transient is starting from an "
                    "unconverged bias.\n", OP_DAMPING_RETRIES);
    printf("[VM] OP complete\n");
}

void vm_solve_op(VM *vm) {
    printf("[VM] Solving DC operating point...\n");
    vm_solve_dc(vm);
}

// ─────────────────────────────────────────────────────────────────────────────
// MAIN RUN LOOP
// ─────────────────────────────────────────────────────────────────────────────

// vm_boot / vm_run_until are the incremental split of what used to be the
// entire body of vm_run: vm_boot is the one-time numerical init (DC
// operating point, or the first Backward-Euler step + factorization),
// vm_run_until advances the transient loop up to a target time. Splitting
// them out lets the --hovercraft library API (runtime/hvr_runtime.hpp,
// runtime/vm/hvr_runtime.cpp) drive a simulation incrementally via
// HVR_step/HVR_run, while vm_run below is preserved byte-for-byte in
// behavior for the standalone binary — it's now just boot() + run_until(end
// _time) + the same CSV export it always did.
void vm_boot(VM *vm) {
    // COLD START — applied HERE, before the operating point, not inside the
    // solver's run().
    //
    // .tran's t_step is a CEILING on a variable-step solver, and a run should
    // not open at its ceiling: examples/Diode/rectifier.hvr started at its
    // 100 us max settles to a dc_out of 17.07..22.55 V instead of the correct
    // 12.06..22.53 V, because the first mains half-cycle is where the reservoir
    // cap charges and all four bridge diodes first commutate. cold_start_dt
    // applies SPICE's first-timestep rule to pick the opening dt instead.
    //
    // The placement is the part worth explaining. Doing this at the top of
    // run() looks equivalent and is not, because vm_solve_dc below solves at
    // alpha = 1/vm->time_step — so a cold start applied after the OP leaves the
    // OP damped by the CEILING while the transient is damped by the opening
    // step, ten times apart. That is not hypothetical: it is what this codebase
    // did for the length of one commit, and it silently weakened every migrated
    // deck's operating point by 10x, on the same alpha*C diagonal whose removal
    // is recorded above vm_solve_dc_once as having broken h_bridge outright.
    // Setting it here makes the OP and the first transient step agree by
    // construction, and lands the OP back on the alpha the decks used to get
    // from .tran's old initial-dt argument.
    //
    // Guarded on is_adaptive() because a fixed-step solver's vm->time_step is
    // not a ceiling — it is every step the run will take, and dividing it here
    // would silently change the deck's sample rate.
    if (vm->strategy && vm->strategy->is_adaptive()) {
        vm->time_step = cold_start_dt(vm->time_step, vm->end_time);
    }

    // Both branches now run the same trust-region DC Newton. The old no-.op
    // branch was ONE LU solve at G + (1/dt)C with no Jacobian and no Newton
    // — and in Hover a junction stamps nothing into G, so that solve treated
    // every junction as an open circuit and handed the transient the answer
    // to a structurally different netlist. Junction decks effectively
    // required `.op;` purely because of this shortcut.
    if (vm->op_enabled) {
        printf("[VM] Solving DC operating point...\n");
    } else {
        printf("[VM] No .op directive — solving DC bias point (same Newton as .op)\n");
    }
    vm_solve_dc(vm);
}

void vm_run_until(VM *vm, double target_time) {
    vm->end_time = target_time;
    if (vm->time >= vm->end_time) {
        return;
    }

    // g_dirty is set unconditionally here (matching the original vm_run,
    // which set it after the boot if/else regardless of which branch ran)
    // so every subsequent HVR_step/HVR_run call re-marks the solver dirty,
    // not just the very first one.
    vm->solver->g_dirty = 1;

    if (vm->strategy) {
        vm->strategy->run(vm);
    }
}

void vm_run(VM *vm) {
    printf("[VM] Starting Co-Simulation... T_stop: %.3e, dt: %.3e\n",
           vm->end_time, vm->time_step);

    vm_boot(vm);
    vm_run_until(vm, vm->end_time);

    printf("[VM] Simulation Complete.\n");
    logger_export_csv(&vm->logger, "simulation_output.csv");
}