#pragma once

// ─────────────────────────────────────────────────────────────────────────────
// INTERNAL STEP-SIZE GUARDS
//
// .tran gives a deck exactly one step-size number, t_step, and it bounds dt from
// ABOVE: the fixed step on a fixed-step solver, max_dt on a variable-step one.
// Everything else about how dt moves is the solver's own business. This header
// holds the two guards that used to be user-visible knobs and are not any more,
// with the measurements that set them.
//
// Both are deliberately loose. They exist to stop pathological behaviour, not to
// tune accuracy, and the two directions of error are wildly asymmetric: a guard
// that is too tight silently breaks a circuit that was about to work, while a
// guard that is too loose costs a few seconds in front of an error message that
// was going to be printed anyway. Tune toward loose.
// ─────────────────────────────────────────────────────────────────────────────

// ── COLD START — SPICE'S FIRST-TIMESTEP RULE ─────────────────────────────────
//
// An adaptive solver does not start at max_dt. It starts where SPICE starts:
//
//     dt_0 = min(t_step, t_stop / TSTOP_DIVISOR) / FIRST_STEP_DIVISOR
//
// Two clauses, doing two different jobs.
//
// The min() against t_stop/50 is the one that is not obvious. It says the
// opening step must be small relative to the WHOLE RUN, not merely relative to
// the ceiling the deck asked for — so a deck that sets a max step coarse enough
// to cross its own run in a handful of steps still gets a resolved start. A rule
// written only as a fraction of max_dt (which is what stood here first) cannot
// express that, because it has no idea how long the run is. No deck in this repo
// currently trips the clause — every one of them sets t_step well under
// t_stop/50, so the min() picks t_step and the rule reduces to t_step/10 — and it
// is kept precisely because the case it guards is the one nobody writes on
// purpose.
//
// The /10 is the actual cold-start damping, and this codebase needs it for the
// same reason SPICE does. Started AT its 100 us ceiling,
// examples/Diode/rectifier.hvr settles to dc_out = 17.07..22.55 V instead of the
// 12.06..22.53 V the deck documents as correct: the first mains half-cycle is
// where the reservoir cap charges and all four bridge diodes first commutate,
// and resolving it with full-size steps puts the run on a different trajectory
// that nothing later recovers. The answer does not diverge, it is just wrong.
//
// The rule reproduces what the decks already said by hand. Before .tran's
// initial-dt argument was removed, rectifier.hvr asked for 10 us out of a 100 us
// ceiling and npn_amp.hvr for 1 us out of 10 us — exactly t_step/10 in both
// cases. The decks had SPICE's rule of thumb written into them one deck at a
// time; this moves it into the solver where it cannot be forgotten.
//
// WHY NOT START SMALLER STILL — e.g. from the smallest representable dt, ramping
// up. Measured, that is worse, not safer. Rectifier dc_out against its correct
// 12.06..22.53 V, as a fraction of max_dt, all other decks unchanged:
//
//   fraction  1.0      1e-1     1e-2     1e-4     1e-6      1e-8     1e-10
//   dc_out    17.07 ✗  12.06 ✓  12.06 ✓  12.09 ✓  -1.52 ✗✗  20.54 ✗  12.04 ✓
//
// Note the shape: not a curve that improves and flattens, but an erratic one.
// 1e-6 produces a NEGATIVE dc_out on a rectifier, 1e-8 is wrong the other way,
// 1e-10 is right again by luck. Below ~1e-4 the starting dt stops being a smaller
// version of the same question, for the reason recorded at the alpha-continuation
// note in bdf2.cpp: alpha = 1/dt scales the companion model's capacitor term, so
// a tiny dt lets alpha*C swamp G, pins every node to its history value, and hands
// the opening trajectory to conditioning rather than to physics. "Start from the
// minimum and ramp" reproduces the tiny-dt failure rather than avoiding it — and
// with min_dt gone there is no minimum to start from in any case. npn_amp's
// collector swing drifts over that range too (8.130 -> 8.064, 0.8%), and every
// deck pays steps it never gets back: the rectifier runs 1045 steps from 1e-2 and
// 1206 from 1e-10, all of them spent climbing.
//
// So SPICE's /10 is not a conservative choice on a "smaller is safer" line. It is
// close to the largest damping that clears the full-ceiling failure, which is
// where this belongs.
inline constexpr double COLD_START_TSTOP_DIVISOR = 50.0;
inline constexpr double COLD_START_FIRST_DIVISOR = 10.0;

// cold_start_dt returns the dt an adaptive solver should take its first step at.
// t_stop <= 0 (a degenerate or unset run length) simply skips the whole-run
// clause rather than producing a zero or negative dt.
inline double cold_start_dt(double max_dt, double t_stop) {
    double dt = max_dt;
    if (t_stop > 0.0) {
        const double whole_run = t_stop / COLD_START_TSTOP_DIVISOR;
        if (whole_run < dt) dt = whole_run;
    }
    return dt / COLD_START_FIRST_DIVISOR;
}

// ── REJECTION BACK-OFF LIMIT ─────────────────────────────────────────────────
//
// Every adaptive solver answers a non-converged step by cutting dt and retrying,
// and that loop needs a stopping condition or it runs until dt denormalizes to
// zero and alpha = 1/dt becomes infinite. That condition used to be `min_dt`, a
// hardcoded 1e-12 absolute floor on every adaptive solver struct.
//
// An absolute floor is the wrong shape for the job. It means something different
// on a 400 ms motor deck than on a 5 us logic deck, and it cannot be exposed to
// decks either: wiring .tran's t_step through as a floor was tried (2026-08-10)
// and broke npn_amp.hvr and pnp_amp.hvr at t = 0 even at a 1 ns setting, because
// a BJT amplifier's cold start legitimately needs a few picosecond-scale steps
// before it settles. Any number small enough to permit that is too small to
// bound anything.
//
// So the limit is RELATIVE to where the back-off started. A solver records dt at
// the first rejection of a run and gives up once dt has fallen by
// REJECT_DT_RATIO from it; an accepted step clears the record. That is
// scale-free, needs no user input, and cannot forbid a cold start, because a
// cold start that keeps succeeding never begins a rejection run at all.
//
// THE RATIO IS 1e-12 BECAUSE A TIGHT ONE WAS MEASURED AND IT BROKE A DECK.
// This started at 1e-6, on the reasoning that the optocoupler investigation had
// just shown twenty successive halvings producing the identical iteration count
// and identical residual at every dt — so six orders of fruitless back-off ought
// to be plenty. That reasoning is right about the optocoupler and wrong as a
// general rule. examples/DCMotor/h_bridge.hvr needs a genuine deep back-off once,
// at t = 0.202:
//
//   ratio    1e-6      1e-7      1e-8      1e-9      1e-12     1e-15
//   result   FATAL     20269 steps, completes, identical at every ratio below 1e-7
//
// The need sits one order from where the tight guard was set. 1e-12 leaves five
// orders of margin over the only deck that has ever exercised it, still
// terminates (about 40 halvings from any starting dt), and costs nothing when it
// is not needed — a doomed step reaches the ceiling either way, this only
// decides how many attempts it wastes first.
//
// Neither guard is a per-solver field or a .solver() argument, and neither
// should become one. A deck that wants to bound dt bounds it from above with
// .tran's t_step, which is the only step-size number a user should reason about.
inline constexpr double REJECT_DT_RATIO = 1e-12;

// RejectRun tracks one run of consecutive step rejections. Declare one per
// solver run(), call note() on each rejection and clear() on each accepted
// step; exhausted() is the "give up" verdict.
struct RejectRun {
    double dt_at_start = 0.0;   // dt when this rejection run began; 0 = no run

    // note records the dt that was just rejected. The FIRST rejection of a run
    // sets the baseline — deliberately the dt that failed, not the reduced one,
    // so the ratio measures the full distance backed off.
    void note(double rejected_dt) {
        if (dt_at_start == 0.0) dt_at_start = rejected_dt;
    }

    void clear() { dt_at_start = 0.0; }

    // exhausted answers whether the retry dt has fallen REJECT_DT_RATIO below
    // where the back-off started. Written with a negated comparison so a NaN dt
    // — which every ordinary comparison would answer "false, keep going" to —
    // ends the run instead of spinning forever.
    bool exhausted(double next_dt) const {
        if (dt_at_start == 0.0) return false;
        return !(next_dt >= dt_at_start * REJECT_DT_RATIO);
    }
};
