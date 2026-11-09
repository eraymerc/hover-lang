#include "euler_fixed.hpp"
#include "../vm/vm.hpp"
#include "../vm/zcd.hpp"

// ─────────────────────────────────────────────────────────────────────────────
// EULER FIXED — RUN
//
// Mirrors Go:
//   func (s *EulerFixed) Run(vm *VM) {
//       for vm.Time <= vm.EndTime {
//           dt := vm.CheckZeroCrossing(vm.TimeStep)
//           vm.MnaAPI.UpdateSolution(vm.MnaSolver.LastSolution)
//           vm.RunDigital()
//           vm.RunAnalog()
//           vm.RunPhaseB()
//           vm.MnaSolver.Step(vm.MnaAPI)
//           vm.Logger.LogStep(...)
//           vm.Logger.LogSignals(vm.Values)
//           vm.Time += dt
//       }
//   }
// ─────────────────────────────────────────────────────────────────────────────
// Inside runtime/solvers/euler_fixed.cpp



void EulerFixed::run(VM *vm) {
    // Runs until the next step would overshoot end_time by more than half a
    // step — not `time <= end_time`, which took one step past the end, and
    // whose answer flipped with the rounding accumulated in `time += dt`.
    // Each row is logged AFTER time advances, so it carries the time its
    // solution belongs to; the t = 0 row is the initial state (vm_boot).
    while (vm->time < vm->end_time - 0.5 * vm->time_step) {
        double dt = vm_check_zero_crossing(vm, vm->time_step);
        
        // FIX: Sync the timestep so the MNA engine uses the correct alpha
        vm->solver->sys->dt = dt;

        api_update_solution(vm->api, vm->solver->last_solution);
        vm_run_digital(vm);

        // ddt/idt: backward Euler from the last step. The analog pass below is
        // this solver's only one, at the step's START, so a ddt() here is the
        // derivative over the previous step — explicit, as every device
        // current in this solver is (state_slots.hpp).
        slots_begin_be(vm->slots, 1.0 / dt);


        vm_run_analog(vm);
        vm_run_phase_b(vm);

        solver_step(vm->solver);
        slots_commit(vm->slots);
        vm->time += dt;

        logger_log_step(&vm->logger, vm->time,
                        vm->solver->sys,
                        vm->solver->last_solution);
        
        if (vm->phase_log) vm->phase_log(vm);
        logger_log_signals(&vm->logger, vm->values);
    }
}