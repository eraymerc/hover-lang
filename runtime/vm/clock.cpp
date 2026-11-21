// ─────────────────────────────────────────────────────────────────────────────
// CLOCK DOMAINS — `.clock(name, period);` + `module x = M<>()[] @name;`
//
// A digital module bound to a clock runs once per tick, at t = k * period,
// instead of once per solver step; between ticks its outputs simply keep
// their values (zero-order hold). That makes a controller's sample rate a
// property of the design rather than a side effect of the solver's step.
//
// Mechanics, chosen to touch the seven solvers as little as possible:
//
//   * Ticks run from vm_run_digital, which every solver already calls at the
//     start of each step attempt. A clock whose next tick is due at the
//     step's start time runs its phase there, BEFORE the unclocked logic
//     (vm_run_logic_clocked, below).
//
//   * Each clock remembers the last tick it ran, and that counter is part of
//     the VM snapshot. A rejected step restores the counter along with the
//     module's `state`, so its retry runs the tick again from the same
//     starting point; an accepted step runs each tick exactly once. The DC
//     operating point runs tick 0, and the first transient step then sees it
//     as already done.
//
//   * Variable-step solvers pass their proposed step through vm_limit_step,
//     which shortens it to end exactly on the next tick. Fixed-step solvers
//     do not change their step — the compiler instead requires every clock
//     period to be a whole number of steps, so ticks fall on step
//     boundaries by construction.
//
// A tick counts as reached slightly before its exact time — see tick_tol.
// ─────────────────────────────────────────────────────────────────────────────

#include "vm.hpp"
#include "zcd.hpp"

#include <algorithm>
#include <cmath>

// How early a tick may be considered reached. It has to be:
//   * above the rounding a long run accumulates in `time += dt` — with a
//     fixed step that is ~N * ulp(t) after N steps, so a time that "should"
//     be exactly k * period arrives a hair short of it;
//   * below half a fixed step, or a fixed-step solver would fire the tick
//     one whole step early.
// A quarter of the current step satisfies both (rounding only reaches it
// after ~10^7 steps), capped at 1e-3 of the period and floored at 1e-9 of it
// for the variable-step solvers, whose steps land on ticks to within an ulp
// anyway.
static double tick_tol(const VM *vm, const VMClock &c) {
    return std::min(std::max(0.25 * vm->time_step, 1e-9 * c.period), 1e-3 * c.period);
}

// Index of the latest tick at or before vm->time.
static long tick_at(const VM *vm, const VMClock &c) {
    return (long)std::floor((vm->time + tick_tol(vm, c)) / c.period);
}

bool vm_clocks_due(const VM *vm) {
    for (const VMClock &c : vm->clocks) if (tick_at(vm, c) > c.last_tick) return true;
    return false;
}

void vm_tick_clocks(VM *vm) {
    for (int i = 0; i < (int)vm->clocks.size(); i++) {
        VMClock &c = vm->clocks[i];
        const long due = tick_at(vm, c);
        if (due > c.last_tick) {
            c.last_tick = due;
            if (vm->run_clock) vm->run_clock(vm, i);
        }
    }
}

bool vm_tick_restart(VM *vm, const Eigen::VectorXd &before) {
    const Eigen::VectorXd after = circuit_drive(vm);
    if (!(before.size() == after.size() && (before.array() == after.array()).all())) {
        vm->tick_restart_at = vm->time;
        return true;
    }
    // A retry of a step whose first attempt found a change: the outputs of
    // that attempt that are not `state` were not rolled back, so they already
    // hold the new values and "before" no longer shows the jump.
    return vm->time == vm->tick_restart_at;
}

// THE LOGIC PASS OF A DECK WITH CLOCKS — TICKS FIRST.
//
// A clocked module is a register and the unclocked digital logic is
// combinational, held between steps (sample-and-hold). At a clock edge real
// logic samples its inputs as they were just before the edge, updates the
// registers, and the combinational logic settles on the new values. So the
// ticks due at t_n run first — they read the unclocked values held over the
// previous step, which ARE those values just before t_n — and the unclocked
// logic runs after them, on the registers' new values, once per step as
// always. Until 2026-11-21 the order was the reverse: logic fed by a clocked
// output saw it one step late, so a source driven through an unclocked module
// changed one solver step after the tick (up to the maximum step), and the
// solvers carried their history across that change.
//
// Did the step change anything the circuit sees? circuit_drive — the driven
// sources AND the logic values analog code reads (an analog switch model's
// gate) — before the ticks against after the whole pass, bit-exact, as at a
// landed breakpoint (vm/breakpoints.cpp). Only then do the solvers restart
// their history: before 0.9 every tick did, though h_bridge.hvr's sequencer
// changes its outputs on 3 of 400 ticks. (Until 2026-11-21 only the sources
// were compared, so a clocked gate read by an analog module never restarted.)
//
// In a deck whose logic has breakpoints the step's logic pass runs early, in
// vm_breakpoint_step, and the ticks and this decision run there; the
// solver's call then finds no tick due and keeps bp.restart.
void vm_run_logic_clocked(VM *vm) {
    // A step that starts on a breakpoint restarts the history as a tick does
    // (vm/breakpoints.hpp); bp.restart is false in a deck without any.
    vm->tick_changed = vm->bp.restart;
    // A .zcd probe re-runs logic along the step; its ticks are replayed at the
    // step's start (vm/zcd.cpp), never at the probe's time.
    const bool tick = !vm->in_probe && vm_clocks_due(vm);
    Eigen::VectorXd before;
    if (tick) {
        before = circuit_drive(vm);
        vm_tick_clocks(vm);
    }
    if (vm->phase_structural) vm->phase_structural(vm);
    if (vm->phase_digital)    vm->phase_digital(vm);
    if (tick) vm->tick_changed = vm_tick_restart(vm, before) || vm->bp.restart;
}

double vm_limit_step(const VM *vm, double dt) {
    for (const VMClock &c : vm->clocks) {
        const double next = (double)(tick_at(vm, c) + 1) * c.period;
        const double remaining = next - vm->time;
        if (!(remaining > 0.0)) continue;
        if (dt >= remaining) {
            dt = remaining;                     // would cross the tick: end on it
        } else if (dt > remaining - tick_tol(vm, c)) {
            // Would stop just short, leaving a sliver: take half the way now
            // and the rest next step (ngspice dctran.c, "equalise the last two
            // time steps before the breakpoint"). This used to STRETCH the step
            // to the tick instead, which can undo a truncation-error rejection:
            // a reject shrinking dt by a factor above 0.8 got stretched back to
            // the same step, rejected again, forever — examples/DCMotor/
            // h_bridge.hvr at t = 198.9998 ms, 900k rejections of one step.
            dt = 0.5 * remaining;
        }
    }
    return dt;
}


void vm_reset_clocks(VM *vm) {
    for (VMClock &c : vm->clocks) c.last_tick = -1;
}
