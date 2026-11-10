#include "snapshot.hpp"
#include "vm.hpp"

// ─────────────────────────────────────────────────────────────────────────────
// SAVE STATE
// Deep copy of everything that needs to be restored on solver step rejection
// or ZCD rollback.
//
// Mirrors Go:
//   func (vm *VM) SaveState() VMSnapshot {
//       snap.Time = vm.Time; snap.TimeStep = vm.TimeStep
//       snap.Values = copy(vm.Values)
//       snap.LastSolution = copy(vm.MnaSolver.LastSolution)
//       snap.BStatic = copy(vm.MnaSolver.Sys.B_static)
//       snap.CSLastVals = copy(current source last values)
//   }
// ─────────────────────────────────────────────────────────────────────────────

VMSnapshot vm_save_state(const VM *vm) {
    VMSnapshot snap;
    snap.time      = vm->time;
    snap.time_step = vm->time_step;
    snap.last_solution = vm->solver->last_solution;     // Eigen copy
    snap.b_static      = vm->solver->sys->B_static;    // Eigen copy

    // Current sources' last values, in the map's iteration order — fixed for
    // the run, since sources are only registered while the netlist is built.
    snap.cs_last_vals.reserve(vm->solver->sys->current_sources.size());
    for (const auto &kv : vm->solver->sys->current_sources)
        snap.cs_last_vals.push_back(kv.second.last_value);
    for (const VMClock &c : vm->clocks) snap.clock_last_tick.push_back(c.last_tick);
    snap.slots = vm->slots;

    // Every Hover `state` variable's current value, copied out of its C++
    // global by save_state_vars into vm->state_vals. vm->values (the logged
    // signals) is NOT part of a snapshot: phase_log rewrites it before every
    // logged row, so nothing ever reads a rolled-back copy of it.
    // const_cast is safe: save_state_vars only writes vm->state_vals.
    if (vm->save_state_vars) vm->save_state_vars(const_cast<VM *>(vm));
    snap.state_vals = vm->state_vals;

    return snap;
}

// ─────────────────────────────────────────────────────────────────────────────
// RESTORE STATE
// Mirrors Go:
//   func (vm *VM) RestoreState(snap VMSnapshot) {
//       vm.Time = snap.Time; vm.TimeStep = snap.TimeStep
//       vm.Values = snap.Values
//       vm.MnaSolver.LastSolution = snap.LastSolution
//       vm.MnaSolver.Sys.B_static = snap.BStatic
//       for k, val := range snap.CSLastVals { sys.CurrentSources[k].LastValue = val }
//   }
// ─────────────────────────────────────────────────────────────────────────────

void vm_restore_state(VM *vm, const VMSnapshot &snap) {
    vm->time      = snap.time;
    vm->time_step = snap.time_step;
    vm->solver->sys->dt = snap.time_step;

    vm->state_vals               = snap.state_vals;
    vm->solver->last_solution    = snap.last_solution;
    vm->solver->sys->B_static    = snap.b_static;

    size_t k = 0;
    for (auto &kv : vm->solver->sys->current_sources) {
        if (k >= snap.cs_last_vals.size()) break;
        kv.second.last_value = snap.cs_last_vals[k++];
    }
    for (size_t i = 0; i < snap.clock_last_tick.size() && i < vm->clocks.size(); i++) {
        vm->clocks[i].last_tick = snap.clock_last_tick[i];
    }
    vm->slots = snap.slots;

    // Write the restored values back into the real C++ state globals —
    // this is the actual rollback. Without this call vm->state_vals would
    // be restored but the globals themselves (e.g. a `state int` counter
    // that a ZCD probe advanced) would stay wherever the probe left them.
    if (vm->restore_state_vars) {
        vm->restore_state_vars(vm);
    }
}