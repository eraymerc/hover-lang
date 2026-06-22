#include "zcd.hpp"
#include "vm.hpp"

#include <cmath>

// ─────────────────────────────────────────────────────────────────────────────
// DETECT DISCONTINUITY
// Compares current logic values against the snapshot.
// A jump > 1.0 in any variable signals a discrete switching event.
//
// Mirrors Go:
//   func (vm *VM) DetectDiscontinuity(snap VMSnapshot) bool {
//       for k, oldVal := range snap.Values {
//           if newVal, ok := vm.Values[k]; ok {
//               if math.Abs(newVal-oldVal) > 1.0 { return true }
//           }
//       }
//       return false
//   }
// ─────────────────────────────────────────────────────────────────────────────

int vm_detect_discontinuity(const VM *vm, const VMSnapshot &snap) {
    for (const auto &[key, old_val] : snap.values) {
        auto it = vm->values.find(key);
        if (it != vm->values.end()) {
            if (std::abs(it->second - old_val) > 1.0) return 1;
        }
    }
    return 0;
}

// ─────────────────────────────────────────────────────────────────────────────
// ISOLATE ZERO CROSSING
// Bisects the interval [0, current_dt] until the crossing time is isolated
// to within 1 ps tolerance.
//
// Mirrors Go:
//   func (vm *VM) IsolateZeroCrossing(checkpoint VMSnapshot, currentDt float64) float64
// ─────────────────────────────────────────────────────────────────────────────

double vm_isolate_zero_crossing(VM *vm, const VMSnapshot &checkpoint, double current_dt) {
    double left  = 0.0;
    double right = current_dt;
    const double tolerance = 1e-12;  // 1 ps

    while ((right - left) > tolerance) {
        double mid = (left + right) / 2.0;

        vm_restore_state(vm, checkpoint);
        vm->time += mid;

        api_update_solution(vm->api, checkpoint.last_solution);
        vm_run_digital(vm);
        vm_run_analog(vm);

        if (vm_detect_discontinuity(vm, checkpoint)) {
            right = mid;
        } else {
            left = mid;
        }
    }

    return right;
}

// ─────────────────────────────────────────────────────────────────────────────
// CHECK ZERO CROSSING
// Probes one step ahead. If a discontinuity is found, bisects to exact time.
// VM state is fully restored — probe only.
// Updates sys->dt and g_dirty if dt shrinks.
//
// Mirrors Go:
//   func (vm *VM) CheckZeroCrossing(dt float64) float64
// ─────────────────────────────────────────────────────────────────────────────

double vm_check_zero_crossing(VM *vm, double dt) {
    if (!vm->zcd_enabled) return dt;

    VMSnapshot checkpoint = vm_save_state(vm);
    vm->time += dt;

    api_update_solution(vm->api, checkpoint.last_solution);
    vm_run_digital(vm);
    vm_run_analog(vm);

    if (vm_detect_discontinuity(vm, checkpoint)) {
        dt = vm_isolate_zero_crossing(vm, checkpoint, dt);
    }

    vm_restore_state(vm, checkpoint);

    if (dt != vm->time_step) {
        vm->time_step              = dt;
        vm->solver->sys->dt        = dt;
        vm->solver->g_dirty        = 1;
    }

    return dt;
}