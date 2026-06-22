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
    snap.values    = vm->values;                        // deep copy of unordered_map
    snap.last_solution = vm->solver->last_solution;     // Eigen copy
    snap.b_static      = vm->solver->sys->B_static;    // Eigen copy

    // Save current source last values for correct unstamp on restore
    for (const auto &[name, rec] : vm->solver->sys->current_sources) {
        snap.cs_last_vals[name] = rec.last_value;
    }

    // Capture every Hover `state` variable's real current value into
    // vm->values (under the same key phase_log uses), THEN copy that into
    // the snapshot. Without this call, snap.values would only contain
    // whatever phase_log last wrote at the most recent successful log
    // step — stale with respect to any phase_digital/phase_analog calls
    // that ran since then (e.g. a ZCD probe). const_cast is safe here:
    // save_state_vars only writes into vm->values, which this function
    // already treats as mutable working state despite the const VM*
    // parameter (matching the pre-existing pattern — vm->values was
    // always read fresh here, this just ensures it's actually fresh).
    if (vm->save_state_vars) {
        vm->save_state_vars(const_cast<VM *>(vm));
        snap.values = vm->values;
    }

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

    vm->values                   = snap.values;
    vm->solver->last_solution    = snap.last_solution;
    vm->solver->sys->B_static    = snap.b_static;

    for (const auto &[name, val] : snap.cs_last_vals) {
        CurrentSourceRecord *rec = vm->solver->sys->find_current_source(name);
        if (rec) rec->last_value = val;
    }

    // Write the restored values back into the real C++ state globals —
    // this is the actual rollback. Without this call, vm->values would
    // be correctly restored but the state globals themselves (e.g. a
    // `state int` counter that a ZCD probe advanced) would remain at
    // whatever the probe left them at, since they live outside vm->values
    // except at the instants save_state_vars/phase_log run.
    if (vm->restore_state_vars) {
        vm->restore_state_vars(vm);
    }
}