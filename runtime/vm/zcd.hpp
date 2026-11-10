#pragma once

#include <Eigen/Dense>
#include "snapshot.hpp"

// Forward declaration
struct VM;

// ─────────────────────────────────────────────────────────────────────────────
// ZERO CROSSING DETECTION (.zcd) — locating logic discontinuities in a step
//
// Logic (module and digital domains) runs once at the start of each step and
// its outputs hold for the step. When a logic output jumps — a comparator
// flips, a friction model changes sign with the speed — at an instant inside
// a step, the step integrates the old value past it. .zcd finds that instant
// and ends the step there.
//
// What it watches is the DRIVE VECTOR: the right-hand side the logic writes
// into the circuit (every driven voltage and current source, vm_drive_vector),
// i.e. exactly the channel through which a jump reaches the integration.
//
// How it looks, after the step has been solved: the logic is re-run at points
// along the step — time t_n + s*dt, solution interpolated linearly from x_n to
// x_{n+1} — so a jump caused by the SOLUTION (omega crossing zero) is seen as
// well as one caused by time (a PWM carrier). A component jumps if it changes
// by more than ZCD_REL of its own size AND its change is not smooth: for a
// step located anywhere in the interval the second difference
// P(1) - 2 P(1/2) + P(0) is as large as the change itself, for a smooth signal
// it is O(dt) smaller. The jump is then bisected to ZCD_FRAC of the step.
//
// Until 0.9 the check ran BEFORE solving, with the solution frozen at x_n, so
// events driven by the solution were invisible; it compared vm->values (the
// logged signals and state variables, which a probe never refreshes) against
// an absolute threshold of 1.0 with a strict '>', so a 0 -> 1 logic edge did
// not count; and the fixed-step path shrank vm->time_step permanently after
// the first event. examples/DCMotor/h_bridge.hvr's LoadTorque — a +-10 mA
// friction torque reversing with the speed — went undetected on all counts.
//
// Clock ticks never fire inside a probe (vm->in_probe): vm_limit_step already
// lands every tick on a step boundary.
// ─────────────────────────────────────────────────────────────────────────────

constexpr double ZCD_REL  = 1e-3;   // jump threshold, relative to the component
constexpr double ZCD_ABS  = 1e-12;  // absolute floor (A or V)
constexpr double ZCD_FRAC = 1e-6;   // bisection resolution, fraction of the step

// The source right-hand side the logic currently drives, without leaving any
// trace: B_static and every source's last value are restored bit for bit
// (see vm_noncon_ratio for why that once mattered).
Eigen::VectorXd vm_drive_vector(VM *vm);

// Looks for a logic discontinuity inside the step that starts from `start`
// (taken BEFORE the step's own logic pass) and ends at solution x_end after dt.
// Returns the fraction s in (0, 1] of the step at which the jump has happened
// (the post-jump side of the bisection), or -1 when there is none. The VM is
// left exactly as the caller had it.
double vm_locate_event(VM *vm, const VMSnapshot &start, const Eigen::VectorXd &x_end, double dt);

// Fixed-step solvers (euler_fixed, gauss_siedel), which cannot re-solve a
// step: looks along time only, with the solution held at its current value,
// and returns dt shortened to the event if there is one. Unlike before 0.9 it
// does NOT change vm->time_step — the next step is the full step again.
double vm_check_zero_crossing(VM *vm, double dt);
