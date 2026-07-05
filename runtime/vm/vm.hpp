#pragma once

#include <Eigen/Dense>
#include <string>
#include <unordered_map>
#include <stdexcept>

#include "../mna/system.hpp"
#include "../mna/engine.hpp"
#include "../mna/api.hpp"
#include "solver_strategy.hpp"
#include "snapshot.hpp"
#include "logger.hpp"

// ─────────────────────────────────────────────────────────────────────────────
// RETURN SIGNAL
// Used to unwind the call stack when a return statement is hit inside a
// function call. Mirrors Go: type returnSignal struct{ value float64 }
// ─────────────────────────────────────────────────────────────────────────────

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

    // Pre-allocated Jacobian scratch — used by implicit solvers
    Eigen::MatrixXd jacobian_scratch;
    Eigen::VectorXd base_b_scratch;

};

// ─────────────────────────────────────────────────────────────────────────────
// LIFECYCLE
// ─────────────────────────────────────────────────────────────────────────────

// Zero-initialise VM. sim.cpp sets fields directly after calling this.
void vm_init(VM *vm, API *api, Solver *solver);

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
// MAIN RUN LOOP
// Mirrors Go: func (vm *VM) Run()
// ─────────────────────────────────────────────────────────────────────────────

void vm_run(VM *vm);