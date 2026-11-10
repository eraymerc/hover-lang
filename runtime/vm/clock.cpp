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
//     step's start time runs its phase there (vm_run_clocks).
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

void vm_run_clocks(VM *vm) {
    vm->tick_changed = false;
    if (vm->in_probe) return;   // a .zcd probe re-runs logic; ticks stay on step boundaries

    bool due_any = false;
    for (const VMClock &c : vm->clocks) if (tick_at(vm, c) > c.last_tick) { due_any = true; break; }
    if (!due_any) return;

    // Did the tick change anything the circuit sees? Compare the drive vector
    // before and after (bit-exact: an unchanged output re-drives the same
    // value). Only then do the solvers restart their history — before 0.9
    // every tick did, though h_bridge.hvr's sequencer changes its outputs on
    // 3 of 400 ticks. Outputs that feed an analog module directly rather than
    // a source are not seen here; a controller drives the circuit through
    // sources.
    const Eigen::VectorXd before = vm_drive_vector(vm);
    for (int i = 0; i < (int)vm->clocks.size(); i++) {
        VMClock &c = vm->clocks[i];
        const long due = tick_at(vm, c);
        if (due > c.last_tick) {
            c.last_tick = due;
            if (vm->run_clock) vm->run_clock(vm, i);
        }
    }
    const Eigen::VectorXd after = vm_drive_vector(vm);
    vm->tick_changed = !(before.size() == after.size() && (before.array() == after.array()).all());
}

double vm_limit_step(const VM *vm, double dt) {
    for (const VMClock &c : vm->clocks) {
        const double next = (double)(tick_at(vm, c) + 1) * c.period;
        const double remaining = next - vm->time;
        // Cross the tick, or stop just short of it: end exactly on it.
        if (remaining > 0.0 && dt > remaining - tick_tol(vm, c)) {
            dt = remaining;
        }
    }
    return dt;
}


void vm_reset_clocks(VM *vm) {
    for (VMClock &c : vm->clocks) c.last_tick = -1;
}
