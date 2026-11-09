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

    // save_state_vars / restore_state_vars copy every Hover `state`
    // variable's current C++ global value into/from vm->values, under the
    // same dotted Hover name phase_log uses. This is what lets
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

    // Pre-allocated Jacobian scratch — used by implicit solvers
    Eigen::MatrixXd jacobian_scratch;
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

// KCL check at a step-converged x: the worst row of |r_i| / (rtol*traffic_i +
// atol), where r is the TRUE nonlinear residual and traffic_i the magnitude of
// the currents flowing in row i (residual_converged's yardstick). "True" means
// device models are evaluated with nr_prev() == x, so no junction or FET
// limiter clips anything: a point that only balances because a limiter is
// holding a device voltage away from its node voltages is exactly the false
// convergence SPICE's CKTnoncon exists to refuse. correction is the solver's
// PHYSICAL extra RHS term (trapezoidal's trap_base), never the Norton term.
// The API's read-view and Newton history are restored afterwards.
//
// The yardstick also includes what the Newton STEP tolerance already allows:
// sum_j (|G_ij| + alpha*|C_ij|) * tol_j, tol_j = step_rtol*|x_j| + vntol (nodes)
// or abstol (branches). Without it the veto contradicted the step test on stiff
// rows: an inductor carrying 13.4 A at a 0.26 ns step has alpha*L ~ 2e7, so the
// 13 mA the step test accepts is a 1e5 V residual on a row whose own traffic is
// ~10 V — examples/MOSFET/mosfet_h_bridge.hvr was vetoed on every iteration at
// its 198 ms commutation and stopped. A limiter-held false solution leaves a
// residual the size of the row's whole current, far above this term.
double vm_kcl_ratio(VM *vm, const Eigen::VectorXd &x, double alpha,
                    const Eigen::VectorXd &x_history, const Eigen::VectorXd *correction,
                    double rtol, double atol,
                    double step_rtol, double vntol, double abstol,
                    int *worst_row = nullptr);

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

// True when a step starting at vm->time will run at least one clock tick.
// A tick changes the digital outputs — the inputs the analog side sees — so
// a multistep solver (bdf2, ndf2) must not carry history across it, exactly
// as at a zero crossing: it restarts at order 1, which SPICE does at its
// breakpoints too. Without that, every tick leaks a small error that acts as
// artificial damping and can turn an unstable sampled loop into a stable one.
bool vm_tick_due(const VM *vm);

// ─────────────────────────────────────────────────────────────────────────────
// JACOBIAN
// Numerical Jacobian via 1µV perturbation — for implicit solvers.
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