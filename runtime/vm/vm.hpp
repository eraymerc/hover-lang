#pragma once

#include <Eigen/Dense>
#include <string>
#include <vector>
#include <unordered_map>
#include <stdexcept>

#include "../mna/system.hpp"
#include "../mna/engine.hpp"
#include "../mna/api.hpp"
#include "solver_strategy.hpp"
#include "snapshot.hpp"
#include "logger.hpp"
#include "state_slots.hpp"

// ─────────────────────────────────────────────────────────────────────────────
// RETURN SIGNAL
// Used to unwind the call stack when a return statement is hit inside a
// function call. Mirrors Go: type returnSignal struct{ value float64 }
// ─────────────────────────────────────────────────────────────────────────────

// One clock domain (`.clock(name, period);`). Digital modules bound to it
// with `@name` run once per tick, at t = k * period, and hold their outputs
// in between. Ticks run from vm_run_digital at the START of a solver step;
// adaptive solvers shorten a step so it ends exactly on the next tick
// (vm_limit_step), and fixed-step solvers are required by the compiler to
// use a step that divides every clock period.
struct VMClock {
    double period    = 0.0;
    long   last_tick = -1;   // highest tick index already run; snapshotted
};

struct ReturnSignal {
    double value;
};

// ─────────────────────────────────────────────────────────────────────────────
// VM
//
// Mirrors Go:
//   type VM struct {
//       Program    *elaborator.ElaboratedProgram
//       Values     map[string]float64
//       MnaAPI     *mna.API
//       MnaSolver  *mna.Solver
//       Logger     *mna.Logger
//       Strategy   SolverStrategy
//       Time, TimeStep, EndTime float64
//       ZCDEnabled, OPEnabled   bool
//       ctxPrefix  string
//       ctxParams  map[string]float64
//       ctxPorts   map[string]string
//       jacobianScratch, baseBScratch []float64
//   }
// ─────────────────────────────────────────────────────────────────────────────

struct VM {
    // ── Program ──────────────────────────────────────────────────────────────
    // ── Signal store ─────────────────────────────────────────────────────────
    std::unordered_map<std::string, double> values;
    // `state` variables as save_state_vars last copied them, by codegen index
    // (Sim::set_state_var). What a snapshot rolls back.
    std::vector<double> state_vals;

    // ── MNA layer ────────────────────────────────────────────────────────────
    API    *api;
    Solver *solver;
    Logger  logger;

    // ── Solver ───────────────────────────────────────────────────────────────
    SolverStrategy *strategy;

    // ── Simulation time ──────────────────────────────────────────────────────
    double time;
    double time_step;
    double end_time;

    // ── Feature flags ────────────────────────────────────────────────────────
    int zcd_enabled;
    int op_enabled;

    // ── .ac small-signal sweep ───────────────────────────────────────────────
    // Set by sim.cpp only for a deck with .ac; sim.cpp's main then calls
    // vm_run_ac itself, and vm_run only if the deck also has .tran. (vm_run
    // does not dispatch to vm_run_ac: an extra call there changed how the
    // compiler optimised the transient loop it inlines, and decks without .ac
    // stopped reproducing their previous output bit for bit.)
    int    ac_sweep     = 1;      // 0 = lin (ac_points total), 1 = dec, 2 = oct (points per)
    int    ac_points    = 0;
    double ac_fstart    = 0.0;
    double ac_fstop     = 0.0;
    std::vector<std::string> ac_nodes;     // .save'd MNA nodes
    std::vector<std::string> ac_branches;  // .save'd branch currents (element names)
    int    ac_skipped_signals = 0;         // .save'd logic signals: no AC meaning
    int    op_converged = 0;               // set by vm_solve_op

    // ── Phase function pointers ───────────────────────────────────────────────
    // Set by generated sim.cpp — no ElaboratedProgram needed at runtime.
    void (*phase_structural)(VM *vm);
    void (*phase_digital)(VM *vm);
    void (*phase_analog)(VM *vm);
    void (*phase_b)(VM *vm);
    void (*phase_log)(VM *vm);  // copies C globals into vm->values for logger
    // Evaluates the `state` initialisers that are not compile-time constants —
    // `state double x = V(node);` and the like — at t = 0 (vm_boot). Null when
    // the deck has none.
    void (*state_init)(VM *vm) = nullptr;
    // The compiler-differentiated copy of phase_analog (hvr::Dual): fills
    // jacobian_scratch directly. Null when the deck's analog code could not be
    // differentiated (codegen/autodiff.go) — the Jacobian is then numerical.
    void (*phase_jacobian)(VM *vm) = nullptr;

    // save_state_vars / restore_state_vars copy every Hover `state`
    // variable's current C++ global value into/from vm->state_vals, by the
    // index codegen gives it (until 0.9: vm->values, by dotted name). This is what lets
    // vm_save_state/vm_restore_state (snapshot.cpp) actually roll back
    // real program state during a ZCD probe or solver step rejection —
    // without these, vm->values only ever reflects whatever phase_log
    // last copied at the most recent successful log, which is NOT the
    // same as "the state right now" during a mid-step probe. A probe that
    // runs phase_digital and then "restores" without these pointers set
    // would silently leave the real state globals (e.g. a `state int`
    // counter) permanently advanced, even though the probe was meant to
    // have zero lasting effect.
    void (*save_state_vars)(VM *vm);
    void (*restore_state_vars)(VM *vm);

    // The hvr::Sim that owns this VM (runtime/hvr_sim/hvr_sim.cpp). Its phase-hook
    // trampolines use it to hand generated phases their Sim& — the VM
    // itself never reads it.
    void *host = nullptr;

    // Clock domains, in declaration order, and the hook that runs one
    // domain's phase (installed by hvr::Sim). Empty for an unclocked deck,
    // which is then simulated exactly as before clocks existed.
    std::vector<VMClock> clocks;

    // ddt()/idt() state, one slot per call site (vm/state_slots.hpp). Sized by
    // the generated code before boot; empty for a deck without either.
    StateSlots slots;

    // Set when a solver gives up (its FATAL path): the run stops at
    // failed_at, the CSV still holds everything up to there, and the process
    // exits with code 2 (HVR_ERR_CONVERGE from the library API). Cleared by
    // vm_boot, so a library reset starts clean.
    bool   failed    = false;
    double failed_at = 0.0;
    void (*run_clock)(VM *vm, int index) = nullptr;

    // Set by vm_run_clocks: true when a tick ran at this step's start AND
    // changed what the logic drives into the circuit (vm_drive_vector before
    // vs after). A tick that changes nothing must not restart the
    // integrators' history (docs/hoveR_sorunlar.txt item 13).
    bool tick_changed = false;
    // True while vm_locate_event re-runs the logic along a step: clock ticks
    // must not fire there (vm/zcd.cpp).
    bool in_probe = false;

    // Pre-allocated Jacobian scratch — used by implicit solvers
    Eigen::MatrixXd jacobian_scratch;
    long jacobian_ad_fallbacks = 0;   // AD Jacobians that fell back to the numerical one
    Eigen::VectorXd base_b_scratch;

};

// ─────────────────────────────────────────────────────────────────────────────
// LIFECYCLE
// ─────────────────────────────────────────────────────────────────────────────

// Zero-initialise VM. sim.cpp sets fields directly after calling this.
void vm_init(VM *vm, API *api, Solver *solver);

// A solver's FATAL path: records that the run stopped, and where.
inline void vm_fail(VM *vm) { vm->failed = true; vm->failed_at = vm->time; }

// Accept a Newton step's solution x into the ddt()/idt() history. The slots'
// last evaluation is from the iterate BEFORE the final solve; at nanosecond
// steps the tolerance-sized difference times a diffusion capacitance over dt
// is an ampere-scale error in the next step's charge current. So the analog
// pass is re-run at x first — only when the deck has slots.
void vm_commit_slots_at(VM *vm, const Eigen::VectorXd &x);
// The first half of that: evaluate the slots at x without committing (the LTE
// estimate needs the new point's values before the step is accepted).
void vm_eval_slots_at(VM *vm, const Eigen::VectorXd &x);

// SPICE's CKTnoncon, recovered without the models' help: is a junction or FET
// limiter still active at the step-converged x? The device models limit their
// own voltages (pnjlim, fetlim, limvds, written in Hover), so the runtime
// cannot see the flag SPICE's devices raise. It can see its consequence: the
// devices are evaluated at x twice — limited against x_iter, the iterate x was
// solved from (what the next iteration would use), and with every limiter
// released (nr_prev() == x) — and the worst row of
// |B_true - B_limited| / (rtol * max(|B_true|, |B_limited|) + atol) is
// returned. At a genuine solution x == x_iter to tolerance, no limiter clips
// anything and the two evaluations are bit-identical; a point a limiter is
// holding differs by the very current the limiter is withholding.
//
// This replaced a residual check (KCL with limiters released, scaled by each
// row's current "traffic"), which could not be scaled right from node
// voltages alone: the net row current vanishes on balanced rows (a DC motor's
// mechanical node in steady state — damping and torque currents of 0.67 A
// cancelling to 2e-6 A), so round-off read as a violation and
// mosfet_h_bridge.hvr stopped at its 198 ms commutation; the sum of term
// magnitudes swells with common mode, so a BJT left off while commanded on
// passed, and h_bridge.hvr split into runs ending at 64 and at 5 rad/s.
double vm_noncon_ratio(VM *vm, const Eigen::VectorXd &x, const Eigen::VectorXd &x_iter,
                       double rtol, double atol, int *worst_row = nullptr);

// ─────────────────────────────────────────────────────────────────────────────
// PHASE EXECUTION
// Mirrors Go: RunDigital, RunAnalog, RunPhaseB
// ─────────────────────────────────────────────────────────────────────────────

// Phase A part 1+2: structural then digital domains
void vm_run_digital(VM *vm);

// Phase A part 3: analog domain
void vm_run_analog(VM *vm);

// Phase B: stamp driven source values into MNA B_static
void vm_run_phase_b(VM *vm);

// Shortens a proposed step so it does not cross the next clock tick (and
// does not stop just short of one). Returns dt unchanged when there are no
// clocks. Called by the variable-step solvers where they choose dt.
double vm_limit_step(const VM *vm, double dt);

// Runs every clock whose next tick is due at vm->time (vm_run_digital calls
// it), and forgets which ticks have run (vm_boot calls it). vm/clock.cpp.
void vm_run_clocks(VM *vm);
void vm_reset_clocks(VM *vm);

// (vm_tick_due, "will this step run a tick", is gone: the solvers restart
// their history on vm->tick_changed instead — only when the tick changed what
// the logic drives. Carrying history across a tick that DID change the inputs
// still leaks an error that acts as artificial damping, which is why it is
// restarted then, as SPICE does at its breakpoints.)

// ─────────────────────────────────────────────────────────────────────────────
// JACOBIAN
// The Jacobian -dB/dx for the implicit solvers: from the compiler's automatic
// differentiation (phase_jacobian) when there is one, else finite differences.
// Mirrors Go: func (vm *VM) ComputeJacobian(xGuess []float64) []float64
// ─────────────────────────────────────────────────────────────────────────────

const Eigen::MatrixXd& vm_compute_jacobian(VM *vm, Eigen::VectorXd &x_guess);

// ─────────────────────────────────────────────────────────────────────────────
// DC OPERATING POINT
// Mirrors Go: func (vm *VM) solveOP()
// ─────────────────────────────────────────────────────────────────────────────

void vm_solve_op(VM *vm);

// ─────────────────────────────────────────────────────────────────────────────
// AC SMALL-SIGNAL SWEEP — full DC operating point, linearise once, then solve
// (G + J + jwC) X = B_ac per frequency; writes ac_output.csv (Re/Im columns).
// Returns false (and writes nothing) if the operating point did not converge.
// ─────────────────────────────────────────────────────────────────────────────

bool vm_run_ac(VM *vm);

// ─────────────────────────────────────────────────────────────────────────────
// MAIN RUN LOOP
// Mirrors Go: func (vm *VM) Run()
// ─────────────────────────────────────────────────────────────────────────────

void vm_run(VM *vm);