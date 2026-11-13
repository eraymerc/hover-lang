#include "vm.hpp"
#include "zcd.hpp"
#include "../solvers/newton_trust_region.hpp"
#include "../solvers/step_limits.hpp"
#include "../hover_sim.hpp"   // hvr::dual_overflow (the AD Jacobian)
#include <algorithm>
#include <cmath>
#include <cstdlib>
#include <cstdio>
#include <cstring>

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
    if (!vm->clocks.empty())  vm_run_clocks(vm);  // clocked digital modules — vm/clock.cpp
}

void vm_run_analog(VM *vm) {
    vm->in_analog = true;
    if (vm->phase_analog) vm->phase_analog(vm);
    vm->in_analog = false;
}

void vm_eval_slots_at(VM *vm, const Eigen::VectorXd &x) {
    if (vm->slots.size() == 0) return;
    api_peek_solution(vm->api, x);
    vm_run_analog(vm);
}

void vm_commit_slots_at(VM *vm, const Eigen::VectorXd &x) {
    if (vm->slots.size() == 0) return;
    vm_eval_slots_at(vm, x);
    slots_commit(vm->slots);
}

double vm_noncon_ratio(VM *vm, const Eigen::VectorXd &x, const Eigen::VectorXd &x_iter,
                       double rtol, double atol, int *worst_row) {
    // A PROBE: it must leave the solver bit-for-bit where it found it. While
    // current sources were stamped into B_static incrementally (B += new -
    // last, until 0.9 — System::restamp_current_row), an extra drive pass
    // re-rounded B even when it wrote the same values, and that alone turned
    // examples/Diode/rectifier.hvr into a FATAL at 15.5 ms. Node rows are now
    // rebuilt from the source values, but the save/restore stays: it costs
    // nothing next to the two analog passes. B_static and every source's last
    // value are put back exactly, and the analog outputs are recomputed at the
    // caller's own read-view and Newton history (a deterministic re-run).
    API    *api = vm->api;
    System *sys = vm->solver->sys;
    const Eigen::VectorXd last_saved = api->last_solution;
    const Eigen::VectorXd prev_saved = api->prev_solution;
    const Eigen::VectorXd b_saved    = sys->B_static;
    std::vector<double> cs_saved;
    cs_saved.reserve(sys->current_sources.size());
    for (const auto &kv : sys->current_sources) cs_saved.push_back(kv.second.last_value);
    // The current each AD-linearised source carried in the solve that produced
    // x (the caller evaluated the devices at x_iter just before solving).
    std::vector<double> i_used;
    if (vm->ad_lin_valid) {
        i_used.reserve(vm->ad_lin.size());
        for (const auto &l : vm->ad_lin) i_used.push_back(l.rec->last_value);
    }

    // The devices at x as the NEXT Newton iteration would see them: limited
    // against the iterate x was solved from...
    api->last_solution = x;
    api->prev_solution = x_iter;
    vm_run_analog(vm);
    vm_run_phase_b(vm);
    const Eigen::VectorXd b_lim = sys->B_static;
    // ...and with every limiter released (nr_prev(v) == v).
    api->prev_solution = x;
    vm_run_analog(vm);
    vm_run_phase_b(vm);
    const Eigen::VectorXd &b_true = sys->B_static;

    // SPICE's per-device test (DIOconvTest and kin): each source's true
    // current at x against the solve's own linear model of it, the current at
    // x_iter plus dI/dx * (x - x_iter). Scaled by the source's own current,
    // not a node row's, so it does not trip on balanced rows the way a KCL
    // residual check did (vm.hpp). The step test alone accepted two junctions
    // 1.24 V forward with 2.3e7 A of KCL error on a 310 V H-bridge: a 0.026 V
    // move — one nVt, a factor e in current — passes the 0.31 V tolerance of
    // a 311 V node, and a junction is the difference of two such nodes.
    double dev_worst = 0.0;
    if (vm->ad_lin_valid) {
        for (size_t k = 0; k < vm->ad_lin.size(); k++) {
            const auto &l = vm->ad_lin[k];
            double pred = i_used[k];
            for (int j = 0; j < l.n; j++) pred += l.d[j] * (x(l.i[j]) - x_iter(l.i[j]));
            const double now   = l.rec->last_value;
            const double ratio = std::abs(now - pred)
                               / (rtol * std::max(std::abs(now), std::abs(pred)) + atol);
            if (!(ratio <= dev_worst)) dev_worst = ratio;
        }
    }

    double worst = dev_worst;
    int    wrow  = -1;
    for (int i = 0; i < x.size(); i++) {
        const double d     = std::abs(b_true(i) - b_lim(i));
        const double scale = std::max(std::abs(b_true(i)), std::abs(b_lim(i)));
        const double ratio = d / (rtol * scale + atol);
        if (!(ratio <= worst)) { worst = ratio; wrow = i; }   // NaN counts as worst
    }
    static const bool trace = getenv("HOVER_TRACE_KCL") != nullptr;
    if (trace && worst > 10.0 && wrow >= 0) {
        std::string name = wrow < sys->num_nodes ? sys->node_names[wrow] : std::string("branch?");
        if (wrow >= sys->num_nodes)
            for (const auto &kv : sys->branch_map) if (kv.second == wrow) { name = "I(" + kv.first + ")"; break; }
        fprintf(stderr, "[noncon] t=%.12e ratio=%.3e row=%d %s limited=%.6e true=%.6e\n",
                vm->time, worst, wrow, name.c_str(), b_lim(wrow), b_true(wrow));
    }

    // ...and the evaluation the solve that produced x actually used: the
    // devices at x_iter, limited against the iterate BEFORE it, against
    // released. A limiter active there means x balances limited currents, and
    // SPICE refuses convergence on exactly that (CKTnoncon). The check above
    // cannot see it once x == x_iter: a 1.5 mA step into an LED stopped at
    // 2.45 V because its solve had limited against an iterate at 0.93 MV, and
    // both evaluations above ran unlimited — 3.5 kA of KCL error, accepted
    // (docs/hoveR_sorunlar.txt item 41). The library limiters (pnjlim, fetlim,
    // limvds) never act on a branch that moved less than 2 nVt, so when no node
    // moved 10 mV in that iteration there is nothing to find and the two extra
    // passes are skipped.
    //
    // That shortcut holds for limiters anchored on nr_prev(V(...)), not for
    // those anchored on their own output, nr_prev(x) (vm/nr_locals.hpp): those
    // can still be holding a junction after the nodes have stopped — SPICE's
    // icheck. With an AD Jacobian the per-device test above sees it (the
    // solve's tangent at the held voltage misses the true current at x);
    // without one only these passes can, so they always run then.
    const int nn = sys->num_nodes;
    if (nn > 0 && ((x_iter.head(nn) - prev_saved.head(nn)).cwiseAbs().maxCoeff() > 1e-2 ||
                   (vm->nr.size() > 0 && !vm->ad_lin_valid))) {
        api->last_solution = x_iter;
        api->prev_solution = prev_saved;
        vm_run_analog(vm);
        vm_run_phase_b(vm);
        const Eigen::VectorXd b_used = sys->B_static;
        api->prev_solution = x_iter;
        vm_run_analog(vm);
        vm_run_phase_b(vm);
        for (int i = 0; i < x.size(); i++) {
            const double d     = std::abs(sys->B_static(i) - b_used(i));
            const double scale = std::max(std::abs(sys->B_static(i)), std::abs(b_used(i)));
            const double ratio = d / (rtol * scale + atol);
            if (!(ratio <= worst)) { worst = ratio; wrow = i; }
        }
    }

    api->last_solution = last_saved;
    api->prev_solution = prev_saved;
    // Leave the analog outputs as the caller had them, evaluated at its own
    // read-view: this is a probe and must not change what the solver does next.
    vm_run_analog(vm);
    sys->B_static = b_saved;
    size_t k = 0;
    for (auto &kv : sys->current_sources) kv.second.last_value = cs_saved[k++];
    if (worst_row) *worst_row = wrow;
    return worst;
}

void vm_run_phase_b(VM *vm) {
    if (!vm->phase_b) return;
    // Source rows are summed once at the end of the pass (System::cs_defer).
    System *sys = vm->solver->sys;
    sys->cs_defer = true;
    vm->phase_b(vm);
    sys->flush_current_rows();
}

// ─────────────────────────────────────────────────────────────────────────────
// JACOBIAN — -dB/dx of the analog blocks, for the implicit solvers
// Exact when the compiler emitted phase_jacobian (automatic differentiation,
// compiler/codegen/autodiff.go); otherwise, and for any formation the AD copy
// cannot express, by finite differences: one per-unknown perturbation each.
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

    // ── The exact Jacobian, when the compiler could differentiate the deck ──
    // One evaluation of phase_jacobian (the analog blocks in hvr::Dual — see
    // hover_sim.hpp) at the same read-view writes -dB/dx straight into
    // jacobian_scratch. It touches no simulation state, so the baseline just
    // computed stays the state the caller sees, exactly as after the
    // numerical loop below. A gradient that outgrew hvr::Dual::K, or a
    // derivative that is not finite (sqrt at 0, a pow at a singular base),
    // falls back to the numerical Jacobian for this formation.
    // HOVER_JACOBIAN=fd forces the numerical one (for comparison).
    static const bool force_fd = [] {
        const char *e = getenv("HOVER_JACOBIAN");
        return e != nullptr && strcmp(e, "fd") == 0;
    }();
    if (vm->phase_jacobian && !force_fd) {
        hvr::dual_overflow() = false;
        vm->ad_lin.clear();
        vm->in_analog = true;
        vm->phase_jacobian(vm);
        vm->in_analog = false;
        if (!hvr::dual_overflow() && vm->jacobian_scratch.allFinite()) {
            vm->ad_lin_valid = true;
            return vm->jacobian_scratch;
        }
        vm->ad_lin_valid = false;
        vm->jacobian_ad_fallbacks++;
        vm->jacobian_scratch.setZero();
    }

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
// without the .op directive). Newton with a fresh Jacobian per iteration
// (vm_compute_jacobian: automatic differentiation, or finite differences), damped by the same diagonally-scaled trust region BDF2 uses,
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
// op_newton — one damped Newton solve of
//     G x + alpha_c C (x - history) = B(x)
// starting from x_guess, which holds the answer on success. Shared by the
// source-stepping ladder (history = 0) and the .op pseudo-transient
// continuation below (history = the previous rung's answer).
static bool op_newton(VM *vm, Eigen::VectorXd &x_guess,
                      const Eigen::VectorXd &history, double alpha_c,
                      int *iters, double *worst, int *worst_row,
                      bool capped = true) {
    const int n = vm->solver->sys->size;
    Eigen::MatrixXd jacobian;
    Eigen::VectorXd correction(n);
    *iters = 0; *worst = 0.0; *worst_row = -1;

    for (int iter = 0; iter < 500; iter++) {
        *iters = iter + 1;
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
            vm->solver, 1.0, history, alpha_c, &correction, &jacobian);

        if (op_converged(x_new, x_guess, 1e-3, 1e-6)) {
            x_guess = x_new;
            return true;
        }
        *worst = 0.0; *worst_row = -1;
        for (int i = 0; i < n; i++) {
            double d2 = std::abs(x_new(i) - x_guess(i));
            if (!(d2 <= *worst)) { *worst = d2; *worst_row = i; }
        }

        if (!capped) {                       // op_solve's retry: plain Newton
            x_guess = x_new;
            if (!x_guess.allFinite()) return false;
            continue;
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
    return false;
}

// op_solve — op_newton, and when the capped solve fails, ONE retry without
// the cap: plain Newton from where the capped one stopped, as the transient
// solvers retry a step the trust region could not converge
// (solvers/newton_trust_region.hpp, PLAIN-NEWTON RETRY).
//
// The cap moves every unknown at most 0.05 per iteration, so 500 iterations
// reach 25 V: a 1 kV divider started its transient at 300 V instead of 500 V,
// a 48 V bus behind 0.1 Ohm onto 1 mF at 0.3 V, an ideal source across a
// capacitor never converged at all — all linear circuits that plain Newton
// solves in one iteration. The cap itself stays: the notes above record why
// every smarter damper failed on the decks that need it. A rung the cap DOES
// converge never reaches the retry, so those decks are unchanged; a retry that
// fails too leaves the capped answer, as before.
static bool op_solve(VM *vm, Eigen::VectorXd &x_guess,
                     const Eigen::VectorXd &history, double alpha_c,
                     int *iters, double *worst, int *worst_row) {
    if (op_newton(vm, x_guess, history, alpha_c, iters, worst, worst_row)) return true;
    Eigen::VectorXd x_plain = x_guess;
    int plain_iters = 0, plain_row = -1;
    double plain_worst = 0.0;
    if (op_newton(vm, x_plain, history, alpha_c, &plain_iters, &plain_worst, &plain_row, false)) {
        x_guess = x_plain;
        *iters += plain_iters;
        return true;
    }
    return false;
}

// op_relax_to_dc — the .op pseudo-transient continuation.
//
// The source ladder above solves with a ZERO history anchor at alpha = 1/dt,
// so every capacitor is a dt/C resistor to 0 V and every inductor a L/dt
// resistor: what it returns is the state one tiny backward-Euler step after
// an all-zero start, not a DC operating point. Measured: 2.5 V - 0.1 ohm -
// 1 uF gave V(cap) = 5 mV and -25 A of charging current at t = 0.1 ns, with
// "OP complete" printed.
//
// Re-anchoring the history at the current answer changes that: the C term
// is then alpha_c C (x - x_prev), which vanishes at a steady state, so the
// fixed point of these rungs is the true DC solution whatever alpha_c is.
// alpha_c only decides how far one rung moves, and dt grows geometrically to
// DT_END so the last rung is DC for any realistic time constant (1 uF on a
// lone GMIN of 1e-12 is tau = 1e6 s). Starting from the damped ladder's
// answer and walking dt up keeps the 1/dt damping that decks with hidden
// companion inductances (h_bridge's BJTs) rely on for the first solve.
//
// A rung that does not converge is usually a big move hitting the ±0.05
// per-iteration cap (500 iterations = 25 V), so it is retried with a smaller
// dt growth before giving up.
static bool op_relax_to_dc(VM *vm, Eigen::VectorXd &x_guess) {
    const double DT_END = 1e12;
    double dt   = vm->time_step;
    double grow = 10.0;
    int    rungs = 0, iters = 0, worst_row = -1;
    double worst = 0.0;
    Eigen::VectorXd x_try;

    while (dt < DT_END) {
        const double dt_try = std::min(dt * grow, DT_END);
        const Eigen::VectorXd history = x_guess;
        x_try = x_guess;
        // ddt/idt slots take the same history as the matrix: the last
        // converged rung, held in h0 by slots_anchor and untouched by a rung
        // that fails.
        slots_begin_be(vm->slots, 1.0 / dt_try);
        if (op_solve(vm, x_try, history, 1.0 / dt_try, &iters, &worst, &worst_row)) {
            slots_anchor(vm->slots);
            x_guess = x_try;
            dt      = dt_try;
            grow    = std::min(grow * 2.0, 10.0);
            rungs++;
        } else {
            grow = std::sqrt(grow);
            if (grow < 1.05) {
                fprintf(stderr, "[VM] OP: capacitors/inductors did not reach DC steady "
                                "state (stalled at dt=%.3e, worst step %.3e on row %d)\n",
                        dt, worst, worst_row);
                return false;
            }
        }
    }
    printf("[VM] OP relaxed to DC steady state (%d rungs)\n", rungs);
    return true;
}

static bool vm_solve_dc_once(VM *vm, bool relax) {
    int n = vm->solver->sys->size;
    Eigen::VectorXd zero_history = Eigen::VectorXd::Zero(n);
    Eigen::VectorXd x_guess      = Eigen::VectorXd::Zero(n);
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
    // The ladder solves from an all-zero history (zero_history below), and so
    // do the ddt/idt slots: uncharged, at the same alpha.
    slots_reset(vm->slots);
    slots_begin_zero(vm->slots, 1.0 / vm->time_step);
    api_update_solution(vm->api, x_guess);
    vm_run_digital(vm);
    vm_run_analog(vm);
    vm_run_phase_b(vm);

    for (int k = 0; k < n_alphas; k++) {
        double alpha = alphas[k];
        system_set_source_alpha(vm->solver->sys, alpha);

        bool rung_converged = op_solve(vm, x_guess, zero_history,
                                        1.0 / vm->time_step,
                                        &op_iters, &op_worst, &op_worst_row);
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
        } else {
            // Only a converged rung is "complete" — stderr already says when
            // one is not, and stdout used to contradict it.
            printf("[VM] OP alpha=%.2f complete (%d iters)\n", alpha, op_iters);
        }
    }

    // Unconditional, and before the commit restamps: the transient's per-step
    // api_set_voltage_source calls must stamp at full strength. Nothing in
    // the loop above returns early or throws, so this cannot leak.
    system_set_source_alpha(vm->solver->sys, 1.0);

    // Only with .op (or for .ac, which needs the true DC point): without it the
    // run deliberately starts from uncharged reactive elements (the bias point
    // above), which is what decks without the directive have always got. That
    // bias point is one backward-Euler step of vm->time_step from an all-zero
    // history — i.e. the state at t = time_step, not at t = 0 — and vm_boot
    // accounts for it as such (see the t = 0 row there).
    if (relax && !op_failed) {
        slots_anchor(vm->slots);   // the relaxation starts from the last rung
        if (!op_relax_to_dc(vm, x_guess)) op_failed = 1;
    }

    api_update_solution(vm->api, x_guess);
    vm_run_analog(vm);
    vm_run_phase_b(vm);
    // The bias point is the transient's history: its charges, and (without
    // .op) the derivative its backward-Euler step produced — which a
    // trapezoidal first step needs, exactly as the matrix needs B_old.
    slots_anchor(vm->slots);
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

// step_used (optional) receives the time_step the final attempt solved with:
// without .op that is the length of the backward-Euler step the bias point
// represents, which vm_boot books as elapsed time.
static bool vm_solve_dc(VM *vm, bool relax, double *step_used = nullptr) {
    const double dt_restore = vm->time_step;

    for (int attempt = 0; attempt <= OP_DAMPING_RETRIES; attempt++) {
        if (vm_solve_dc_once(vm, relax)) {
            if (attempt > 0) {
                printf("[VM] OP converged after %d damping retr%s (dt %.3e -> %.3e)\n",
                       attempt, attempt == 1 ? "y" : "ies", dt_restore, vm->time_step);
            }
            if (step_used) *step_used = vm->time_step;
            vm->time_step = dt_restore;
            printf("[VM] OP complete\n");
            return true;
        }
        if (attempt < OP_DAMPING_RETRIES) {
            vm->time_step *= 0.5;
            fprintf(stderr, "[VM] OP did not converge — retrying with more damping "
                            "(dt %.3e, alpha %.3e)\n", vm->time_step, 1.0 / vm->time_step);
        }
    }

    if (step_used) *step_used = vm->time_step;
    vm->time_step = dt_restore;
    fprintf(stderr, "[VM] WARNING: DC operating point did not converge after %d "
                    "damping retries; the transient is starting from an "
                    "unconverged bias.\n", OP_DAMPING_RETRIES);
    // Not "OP complete": that line used to be printed here too, so a log read
    // top to bottom (or grepped for it) said the operating point had been
    // found when it had not. stdout now says what stderr says.
    printf("[VM] OP FAILED — continuing from an unconverged bias\n");
    return false;
}

void vm_solve_op(VM *vm) {
    printf("[VM] Solving DC operating point...\n");
    vm->op_converged = vm_solve_dc(vm, vm->op_enabled);
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
    vm->failed = false;
    vm->failed_at = 0.0;
    vm_reset_clocks(vm); // a (re)boot starts every clock from tick 0
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
        printf("[VM] No .op directive — solving bias point; capacitors and inductors start uncharged (add .op; for the DC operating point)\n");
    }
    // NON-CONSTANT STATE INITIALISERS (`state double x = V(node);`) take their
    // value at t = 0, as a declaration does. Until 0.9 codegen dropped them and
    // wrote 0. Without .op the t = 0 state is the circuit at rest (the t = 0
    // row below: every node and branch 0), so they are evaluated there, before
    // the bias point; with .op, t = 0 IS the operating point, so after it.
    if (vm->state_init && !vm->op_enabled) {
        vm->time = 0.0;
        api_peek_solution(vm->api, Eigen::VectorXd::Zero(vm->solver->sys->size));
        vm->state_init(vm);
    }
    double bias_step = 0.0;
    vm_solve_dc(vm, vm->op_enabled, &bias_step);
    if (vm->state_init && vm->op_enabled) {
        api_peek_solution(vm->api, vm->solver->last_solution);
        vm->state_init(vm);
    }

    // THE FIRST ROWS. Every solver logs a step only after advancing time, so
    // the state the run starts from has to be logged here or it appears
    // nowhere — the CSV used to open with the end of a step labelled t = 0.
    //
    // With .op the bias point IS the t = 0 state (a DC solution consumes no
    // time): it is the t = 0 row and the transient starts from it at t = 0.
    //
    // Without .op the bias point is not a t = 0 state. It is one backward-
    // Euler step of bias_step from an all-zero history — the sources switched
    // on at t = 0, the capacitors and inductors starting uncharged — so it is
    // the state at t = bias_step, and booking it as t = 0 put the whole run
    // one step late. It is logged at t = bias_step and the transient continues
    // from there. The t = 0 row before it is the circuit at rest (t = 0-,
    // every node and branch at 0), as SPICE prints for a run started without
    // an operating point. For that row the stateless passes (module, analog)
    // are re-evaluated against the zero solution so I() columns and signals
    // derived from V()/I() agree with it; digital signals keep the values
    // their first pass computed, which is what holds over the first step.
    // Both passes are then re-run against the bias point, so the rest of the
    // run sees exactly what it saw before.
    //
    // (Solving the bias with a vanishingly small step instead, to get a true
    // t = 0+ state, works for linear capacitors but breaks every ddt()-built
    // junction charge: their hidden 1 H inductor would be pinned at 0 A
    // against a q(V) that is not 0, and npn_amp diverged within 31 ns.)
    //
    // A library reset (HVR_reset_sim) reboots, and gets its own first rows.
    auto log_row = [vm](const Eigen::VectorXd &x) {
        logger_log_step(&vm->logger, vm->time, vm->solver->sys, x);
        if (vm->phase_log) vm->phase_log(vm);
        logger_log_signals(&vm->logger, vm->values);
    };
    if (vm->op_enabled) {
        log_row(vm->solver->last_solution);
    } else {
        auto stateless_passes = [vm] {
            if (vm->phase_structural) vm->phase_structural(vm);
            if (vm->phase_analog)     vm->phase_analog(vm);
        };
        const Eigen::VectorXd bias = vm->api->last_solution;
        vm->api->last_solution.setZero();
        stateless_passes();
        vm->time = 0.0;
        log_row(vm->api->last_solution);
        vm->api->last_solution = bias;
        stateless_passes();
        vm->time = bias_step;
        log_row(vm->solver->last_solution);
    }
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

    if (vm->failed) {
        printf("[VM] Simulation STOPPED at t=%.6e of %.6e — the solver did not converge; "
               "the CSV holds the run up to there.\n", vm->failed_at, vm->end_time);
    } else {
        printf("[VM] Simulation Complete.\n");
    }
    if (vm->jacobian_ad_fallbacks > 0)
        printf("[VM] Note: %ld Jacobian(s) fell back from automatic differentiation to finite "
               "differences (a gradient wider than hvr::Dual::K, or a non-finite derivative).\n",
               vm->jacobian_ad_fallbacks);
    logger_export_csv(&vm->logger, "simulation_output.csv");
}