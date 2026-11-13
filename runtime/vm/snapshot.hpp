#pragma once
#include "state_slots.hpp"
#include "nr_locals.hpp"

#include <Eigen/Dense>
#include <unordered_map>
#include <string>
#include <vector>

// ─────────────────────────────────────────────────────────────────────────────
// VM SNAPSHOT
//
// Holds a complete copy of VM state for ZCD rollback and solver step rejection.
// Mirrors Go:
//   type VMSnapshot struct {
//       Time, TimeStep   float64
//       Values           map[string]float64
//       LastSolution     []float64
//       BStatic          []float64
//       CSLastVals       map[string]float64
//   }
// ─────────────────────────────────────────────────────────────────────────────

struct VMSnapshot {
    double                                  time;
    double                                  time_step;
    // Indexed, not named: a snapshot is taken and restored several times per
    // step (solver checkpoints, every .zcd probe), and the name-keyed maps it
    // used to hold cost a string allocation and a hash per entry each time.
    std::vector<double>                     state_vals;    // `state` variables, VM::state_vals order
    Eigen::VectorXd                         last_solution;
    Eigen::VectorXd                         b_static;
    std::vector<double>                     cs_last_vals;  // current sources, System::current_sources iteration order
    std::vector<long>                       clock_last_tick; // per VMClock: a rolled-back step un-runs its tick
    StateSlots                              slots;           // ddt/idt history and recipe
    NrLocals                                nr;              // nr_prev() of analog variables
};

// Forward declaration
struct VM;

// Save current VM state into a snapshot (deep copy).
// Mirrors Go: func (vm *VM) SaveState() VMSnapshot
VMSnapshot vm_save_state(const VM *vm);

// Restore VM state from a snapshot.
// Mirrors Go: func (vm *VM) RestoreState(snap VMSnapshot)
void vm_restore_state(VM *vm, const VMSnapshot &snap);