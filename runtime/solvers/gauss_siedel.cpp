#include "gauss_siedel.hpp"
#include "../vm/vm.hpp"
#include "../vm/zcd.hpp"

#include <cstdio>
#include <cmath>

// ─────────────────────────────────────────────────────────────────────────────
// CONVERGENCE CHECK
// Mirrors Go: func (s *GaussSiedel) hasConverged(xNew, xOld []float64) bool
// ─────────────────────────────────────────────────────────────────────────────

bool GaussSiedel::has_converged(const Eigen::VectorXd &x_new, const Eigen::VectorXd &x_old) const {
    for (int i = 0; i < x_new.size(); i++) {
        double scale = atol + rtol * std::abs(x_new(i));
        if (std::abs(x_new(i) - x_old(i)) > scale) return false;
    }
    return true;
}

// ─────────────────────────────────────────────────────────────────────────────
// RUN
//
// Mirrors Go:
//   func (s *GaussSiedel) Run(vm *VM) {
//       for vm.Time <= vm.EndTime {
//           dt := vm.CheckZeroCrossing(vm.TimeStep)
//           alpha := 1.0 / dt
//           vm.MnaAPI.UpdateSolution(vm.MnaSolver.LastSolution)
//           vm.RunDigital(); vm.RunPhaseB()
//           xAnchor := copy(LastSolution); xGuess := copy(xAnchor)
//           for iter := 0; iter < MaxIter; iter++ {
//               UpdateSolution(xGuess); RunAnalog(); RunPhaseB()
//               xNew := SolveRHS(1.0, xAnchor, alpha, nil, nil)
//               if hasConverged(xNew, xGuess) { xGuess = xNew; break }
//               xGuess += damping*(xNew - xGuess), clamped to MaxStep
//           }
//           AdvanceTime(xGuess); Log; vm.Time += dt
//       }
//   }
// ─────────────────────────────────────────────────────────────────────────────

void GaussSiedel::run(VM *vm) {
    const double damping_factor = 0.05;

    while (vm->time <= vm->end_time) {
        double dt = vm_check_zero_crossing(vm, vm->time_step);
        double alpha = 1.0 / dt;

        api_update_solution(vm->api, vm->solver->last_solution);
        vm_run_digital(vm);
        vm_run_phase_b(vm);

        Eigen::VectorXd x_anchor = vm->solver->last_solution;
        Eigen::VectorXd x_guess  = x_anchor;

        bool converged = false;

        for (int iter = 0; iter < max_iter; iter++) {
            api_update_solution(vm->api, x_guess);
            vm_run_analog(vm);
            vm_run_phase_b(vm);

            const Eigen::VectorXd &x_new = solver_solve_rhs(
                vm->solver, 1.0, x_anchor, alpha, nullptr, nullptr);

            if (has_converged(x_new, x_guess)) {
                x_guess = x_new;
                converged = true;
                break;
            }

            for (int i = 0; i < x_new.size(); i++) {
                double delta = (x_new(i) - x_guess(i)) * damping_factor;
                if (delta >  max_step) delta =  max_step;
                if (delta < -max_step) delta = -max_step;
                x_guess(i) += delta;
            }
        }

        if (!converged) {
            fprintf(stderr, "[GS] warning: did not converge at t=%.3e\n", vm->time);
        }

        solver_advance_time(vm->solver, x_guess);
        logger_log_step(&vm->logger, vm->time, vm->solver->sys, vm->solver->last_solution);
        if (vm->phase_log) vm->phase_log(vm);
        logger_log_signals(&vm->logger, vm->values);

        vm->time += dt;
    }
}
