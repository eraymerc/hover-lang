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
    while (vm->time <= vm->end_time) {
        double dt = vm_check_zero_crossing(vm, vm->time_step);
        
        // FIX: Sync the timestep so the MNA engine uses the correct alpha
        vm->solver->sys->dt = dt;

        api_update_solution(vm->api, vm->solver->last_solution);
        vm_run_digital(vm);


        vm_run_analog(vm);
        vm_run_phase_b(vm);

        solver_step(vm->solver);

        logger_log_step(&vm->logger, vm->time,
                        vm->solver->sys,
                        vm->solver->last_solution);
        
        if (vm->phase_log) vm->phase_log(vm);
        logger_log_signals(&vm->logger, vm->values);

        vm->time += dt;
    }
}