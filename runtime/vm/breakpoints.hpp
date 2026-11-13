#pragma once

#include <Eigen/Dense>
#include <limits>

// ─────────────────────────────────────────────────────────────────────────────
// BREAKPOINTS — steps that end exactly where the deck's code compares `time`
//
// Logic is sample-and-hold: it runs at the start of each step and holds for
// the step. A pulse written as
//
//     if (time >= t0 && time < t0 + w) { i = 1.0; }
//
// that starts and ends inside one step is never sampled, and .zcd cannot see
// it either: it compares the drive at the step's two ends, and they agree.
// SPICE avoids this for its PULSE/PWL sources with breakpoints — the source
// tells the transient where its corners are and every step ends on them.
// Hover's sources are code, so the compiler finds the corners instead
// (compiler/codegen/breakpoints.go): every comparison of a time expression
// against a threshold that does not change during a step becomes a call of
// vm_bp_compare, which evaluates the comparison exactly as written AND works
// out the first time after the step's start at which its result flips. The
// recognised time expressions are
//
//     time,  time + c,  c + time,  time - c                      (monotone)
//     fmod(u, T),  u % T,  u - floor(u / T) * T,  u one of those (periodic)
//
// with c, T and the threshold built from literals, parameters, `state`
// variables of logic, inputs and extern (math) calls — nothing that moves
// within a step. See the compiler file for what is and is not recognised.
//
// What happens with the reported times:
//
//   * Logic (structural and digital modules, and the functions they call).
//     At the start of every step attempt vm_breakpoint_step runs the step's
//     logic pass itself — before choosing dt rather than after, which logic
//     does not notice: it reads t_n and the solution at t_n either way — and
//     collects the first flip of every comparison the logic actually
//     evaluates. (The solver's own call of the logic then skips the generated
//     phases once; clock ticks still run there.) As long as none of those flips, the logic
//     takes the same path and the same thresholds (a nested `if (time < t0 +
//     w)` is only evaluated once `time >= t0` holds — and is found at the step
//     that starts there). The step is shortened to end exactly on the earliest
//     one, so the next step starts there and its logic pass sees the new value.
//
//   * Analog code runs at the solved point t_n + dt, during the step. Its
//     comparisons report their next flips as they run; vm_breakpoint_step uses
//     what the last attempt reported, and if a solved step turns out to have
//     crossed a flip anyway (a comparison that only came into play inside it),
//     vm_breakpoint_cut has the solver re-solve it to end on the flip.
//
// EXACT LANDING. `time >= t0` must be true at the next step's start, not
// false by one ulp. Every reported time is therefore the first DOUBLE at
// which the comparison, evaluated exactly as the generated code evaluates it,
// gives the new result (found by bisection between neighbouring doubles), and
// the step is sized so that t_n + dt == that double in floating point —
// vm->time lands on it bit for bit, no snapping. A strict `time > t0` flips
// at the double after t0, so it is landed there; `time >= t0` at t0 itself.
// Clock ticks are not exact this way and need tick_tol (clock.cpp);
// comparisons in user code cannot be given a tolerance, so they are hit
// exactly instead.
//
// LEFT LIMIT FOR ANALOG CODE. The step that ends on a breakpoint E is solved
// AT E, and analog code would read the new value there — `time >= t0`
// switches the source on for the whole step that merely reaches t0. On that
// step analog code is shown the double just below E (Sim::time), the last
// instant of the old value; the step that starts at E then shows the new one.
// Logic needs no such help: it samples at the start of a step.
//
// PROGRESS. A flip less than h_min = lte_min_step(max_dt) after a step's
// start is not landed (it is seen by the next sample, at most one step
// late), and flips within h_min of each other are landed together, on the
// last of them. So every landed step is at least h_min long, and a threshold
// that tracks `time` itself cannot drive dt to zero. At a landed breakpoint
// the solvers restart their history as at a clock tick that changed the
// logic's outputs (vm->tick_changed): BDF1 and a short first step.
//
// WHEN. Always, for the variable-step solvers (ndf2, bdf2, trapezoidal,
// euler_adaptive) — with or without .zcd: a breakpoint is part of what the
// deck says, not a detection heuristic. .zcd still locates what breakpoints
// cannot: thresholds that move within a step, crossings of nonlinear
// functions of time, events caused by the solution. A deck whose code has no
// recognised comparison never enables any of this and runs exactly as before.
// The fixed-step solvers keep their grid: the deck chose that sampling.
// ─────────────────────────────────────────────────────────────────────────────

struct VM;

// Candidate breakpoints are kept in a small fixed list (no allocation).
constexpr int BP_LIST = 32;

struct VMBreakpointList {
    int    n = 0;
    double t[BP_LIST];
    void clear() { n = 0; }
    void add(double e);   // ignores duplicates; when full, keeps the earliest
};

struct VMBreakpoints {
    bool   enabled   = false;  // codegen found recognised comparisons (Sim::enable_breakpoints)
    bool   logic     = false;  // ...some of them in logic: run the logic pass early to find them
    bool   collect   = false;  // the early logic pass is running: logic comparisons report
    bool   skip_structural = false;  // ...it ran: the solver's call of the phase returns at once
    bool   skip_digital    = false;
    double t0        = 0.0;    // start of the current step attempt
    double h_min     = 0.0;    // see PROGRESS above
    VMBreakpointList logic_next;   // first flips after t0 + h_min, from the early logic pass
    VMBreakpointList analog_next;  // flips analog code reported since the attempt began
    double analog_cross = std::numeric_limits<double>::infinity();  // earliest flip analog code saw itself pass
    double land_from = std::numeric_limits<double>::quiet_NaN();    // this attempt ends on the cluster [land_from, land_at]
    double land_at   = std::numeric_limits<double>::quiet_NaN();
    double land_left = std::numeric_limits<double>::quiet_NaN();    // the double below land_at (what analog code reads there)
    bool   restart   = false;  // the attempt starts on the breakpoint the previous step landed on
    int    cuts      = 0;      // vm_breakpoint_cut retries from the same t0
    Eigen::VectorXd view_saved;  // the solver's read-view, while the early logic pass reads x_n
};

// A recognised comparison: evaluates `S(t) op x` exactly as the original
// expression (form: codegen/breakpoints.go, decoded in breakpoints.cpp), and
// reports where it next flips while the early logic pass or analog code runs.
double vm_bp_compare(VM *vm, int form, double t, double x, double c, double p);

// Called by the variable-step solvers where they choose dt, in place of
// vm_limit_step (whose clock ticks it applies too). Returns dt shortened to
// end exactly on the next breakpoint, and sets vm->tick_changed when the step
// starts on one. Only when vm->bp.enabled.
double vm_breakpoint_step(VM *vm, double dt, double max_dt);

// After a converged step: true when analog code passed a flip inside the step
// that it was not landed on — the solver then restores its checkpoint and
// retries; the retry's vm_breakpoint_step lands on the flip.
bool vm_breakpoint_cut(VM *vm);
