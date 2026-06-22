#pragma once

#include <Eigen/Dense>
#include <unordered_map>
#include <string>

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
    std::unordered_map<std::string, double> values;
    Eigen::VectorXd                         last_solution;
    Eigen::VectorXd                         b_static;
    std::unordered_map<std::string, double> cs_last_vals;  // current source last values
};

// Forward declaration
struct VM;

// Save current VM state into a snapshot (deep copy).
// Mirrors Go: func (vm *VM) SaveState() VMSnapshot
VMSnapshot vm_save_state(const VM *vm);

// Restore VM state from a snapshot.
// Mirrors Go: func (vm *VM) RestoreState(snap VMSnapshot)
void vm_restore_state(VM *vm, const VMSnapshot &snap);