#include "vm.hpp"
#include "zcd.hpp"
#include <cmath>
#include <cstdio>

// ─────────────────────────────────────────────────────────────────────────────
// LIFECYCLE
// ─────────────────────────────────────────────────────────────────────────────

void vm_init(VM *vm, API *api, Solver *solver) {
    vm->api              = api;
    vm->solver           = solver;
    vm->time             = 0.0;
    vm->time_step        = 1e-6;
    vm->end_time         = 0.01;
    vm->zcd_enabled      = 0;
    vm->op_enabled       = 0;
    vm->strategy         = nullptr;
    vm->phase_structural = nullptr;
    vm->phase_digital    = nullptr;
    vm->phase_analog     = nullptr;
    vm->phase_b          = nullptr;
    vm->phase_log        = nullptr;
    vm->save_state_vars    = nullptr;
    vm->restore_state_vars = nullptr;
    int n = solver->sys->size;
    vm->jacobian_scratch = Eigen::MatrixXd::Zero(n, n);
    vm->base_b_scratch   = Eigen::VectorXd::Zero(n);
}

// ─────────────────────────────────────────────────────────────────────────────
// PHASE EXECUTION
// Called by solvers each timestep. Function pointers set by generated sim.cpp.
// ─────────────────────────────────────────────────────────────────────────────

void vm_run_digital(VM *vm) {
    if (vm->phase_structural) vm->phase_structural(vm);
    if (vm->phase_digital)    vm->phase_digital(vm);
}

void vm_run_analog(VM *vm) {
    if (vm->phase_analog) vm->phase_analog(vm);
}

void vm_run_phase_b(VM *vm) {
    if (vm->phase_b) vm->phase_b(vm);
}

// ─────────────────────────────────────────────────────────────────────────────
// JACOBIAN
// Numerical dI/dV estimate via 1µV node perturbation.
// Used by implicit solvers (BDF2, TR-BDF2, etc.)
// ─────────────────────────────────────────────────────────────────────────────

const Eigen::MatrixXd& vm_compute_jacobian(VM *vm, Eigen::VectorXd &x_guess) {
    int n = vm->solver->sys->size;

    // MATLAB's ode15s/ode23tb use a RELATIVE perturbation step for their
    // numerical Jacobians by default: delta_i = sqrt(eps) * max(|x_i|, 1.0)
    // per variable, not one fixed absolute step for the entire vector.
    //
    // The previous fixed delta_v = 1e-6 (an absolute volt/amp/radian
    // value applied uniformly to every node) breaks down badly for any
    // node whose natural scale is far from "around 1e-6" — e.g.
    // theta_e_node in a PMSM model, which integrates an electrical angle
    // and can reach hundreds of radians within milliseconds. Perturbing
    // a 440-radian value by 1e-6 is a relative change of about 2e-9 —
    // nine orders of magnitude smaller than the value itself — which is
    // exactly the regime where floating-point cancellation in
    // (B_static_perturbed - B_static_baseline) produces a numerically
    // garbage derivative estimate, even though cos()/sin() themselves
    // remain accurate at that magnitude (verified separately — this is
    // NOT a trig-precision problem, it's a perturbation-step-size
    // problem).
    //
    // This fix matches MATLAB's documented, standard practice and is a
    // genuine correctness improvement on its own — but tested directly
    // against a real PMSM/FOC motor-control circuit that was failing to
    // converge under TR-BDF2, it did NOT resolve that specific failure
    // (the stall point was identical with and without this fix). That
    // means the PMSM stall has a different root cause than a poorly
    // scaled numerical Jacobian perturbation — this fix is kept because
    // it is correct and matches documented best practice, not because it
    // was confirmed to fix that particular circuit's instability.
    //
    // sqrt(eps) ≈ 1.49e-8 for IEEE double precision (eps ≈ 2.22e-16) is
    // the standard, textbook-derived optimal relative step size for
    // central-difference-free (one-sided) numerical differentiation —
    // small enough to approximate the true derivative well, large
    // enough to avoid being swamped by floating-point rounding noise.
    // The max(|x_i|, 1.0) floor matches MATLAB's own convention: for a
    // node sitting at or near zero, fall back to a step of sqrt(eps)
    // absolute rather than letting the step collapse to zero.
    const double sqrt_eps = 1.4901161193847656e-8; // sqrt(2.22e-16), IEEE double

    vm->jacobian_scratch.setZero();

    // Baseline. Peek (read-view only): the Jacobian is a probe, so none of
    // these perturbation evaluations may advance prev_solution / nr_prev().
    api_peek_solution(vm->api, x_guess);
    vm_run_analog(vm);
    vm_run_phase_b(vm);
    vm->base_b_scratch = vm->solver->sys->B_static;

    for (int i = 0; i < n; i++) {
        double orig = x_guess(i);

        // Per-node relative perturbation — this is the actual fix.
        double scale = std::abs(orig);
        if (scale < 1.0) scale = 1.0;
        double delta_v = sqrt_eps * scale;

        x_guess(i) += delta_v;

        api_peek_solution(vm->api, x_guess);
        vm_run_analog(vm);
        vm_run_phase_b(vm);

        for (int j = 0; j < n; j++) {
            double delta_i = vm->solver->sys->B_static(j) - vm->base_b_scratch(j);
            if (delta_i != 0.0) {
                vm->jacobian_scratch(j, i) = -(delta_i / delta_v);
            }
        }

        x_guess(i) = orig;
    }

    // Restore baseline. Peek again so last_solution is left pointing at the
    // real current iterate (x_guess) with prev_solution untouched — the
    // caller's next api_update_solution then shifts the correct value into
    // prev_solution for nr_prev().
    api_peek_solution(vm->api, x_guess);
    vm_run_analog(vm);
    vm_run_phase_b(vm);

    return vm->jacobian_scratch;
}

// ─────────────────────────────────────────────────────────────────────────────
// DC OPERATING POINT
// ─────────────────────────────────────────────────────────────────────────────

static int op_converged(const Eigen::VectorXd &x_new,
                        const Eigen::VectorXd &x_old,
                        double rtol, double atol)
{
    for (int i = 0; i < x_new.size(); i++) {
        double scale = atol + rtol * std::abs(x_new(i));
        if (std::abs(x_new(i) - x_old(i)) > scale) return 0;
    }
    return 1;
}

void vm_solve_op(VM *vm) {
    printf("[VM] Solving DC operating point...\n");

    int n = vm->solver->sys->size;
    Eigen::VectorXd zero_anchor = Eigen::VectorXd::Zero(n);
    Eigen::VectorXd b_full(n);
    Eigen::VectorXd x_guess = Eigen::VectorXd::Zero(n);

    vm->time = 0.0;
    api_update_solution(vm->api, zero_anchor);
    vm_run_digital(vm);
    vm_run_analog(vm);
    vm_run_phase_b(vm);
    b_full = vm->solver->sys->B_static;

    static const double alphas[] = {
        0.05, 0.1, 0.15, 0.2, 0.3, 0.4, 0.5, 0.6, 0.7, 0.8, 0.9, 1.0
    };

    for (double alpha : alphas) {
        for (const auto &kv : vm->solver->sys->branch_map) {
            int row = vm->solver->sys->num_nodes + kv.second;  // branch_map holds LOCAL idx; row = num_nodes + local
            vm->solver->sys->B_static(row) = b_full(row) * alpha;
        }

        for (int iter = 0; iter < 500; iter++) {
            api_update_solution(vm->api, x_guess);
            vm_run_analog(vm);
            vm_run_phase_b(vm);

            for (const auto &kv : vm->solver->sys->branch_map) {
                int row = vm->solver->sys->num_nodes + kv.second;  // branch_map holds LOCAL idx; row = num_nodes + local
                vm->solver->sys->B_static(row) = b_full(row) * alpha;
            }

            double a = 1.0 / vm->time_step;
            const Eigen::VectorXd &x_new = solver_solve_rhs(
                vm->solver, 1.0, zero_anchor, a, nullptr, nullptr);

            if (op_converged(x_new, x_guess, 1e-3, 1e-6)) {
                x_guess = x_new;
                break;
            }

            for (int i = 0; i < n; i++) {
                double delta = x_new(i) - x_guess(i);
                if (delta >  0.05) delta =  0.05;
                if (delta < -0.05) delta = -0.05;
                x_guess(i) += delta;
            }
        }
        printf("[VM] OP alpha=%.2f complete\n", alpha);
    }

    printf("[VM] OP complete\n");
    api_update_solution(vm->api, x_guess);
    vm_run_analog(vm);
    vm_run_phase_b(vm);
    solver_advance_time(vm->solver, x_guess);
}

// ─────────────────────────────────────────────────────────────────────────────
// MAIN RUN LOOP
// ─────────────────────────────────────────────────────────────────────────────

// vm_boot / vm_run_until are the incremental split of what used to be the
// entire body of vm_run: vm_boot is the one-time numerical init (DC
// operating point, or the first Backward-Euler step + factorization),
// vm_run_until advances the transient loop up to a target time. Splitting
// them out lets the --hovercraft library API (runtime/hvr_runtime.hpp,
// runtime/vm/hvr_runtime.cpp) drive a simulation incrementally via
// HVR_step/HVR_run, while vm_run below is preserved byte-for-byte in
// behavior for the standalone binary — it's now just boot() + run_until(end
// _time) + the same CSV export it always did.
void vm_boot(VM *vm) {
    if (vm->op_enabled) {
        vm_solve_op(vm);
    } else {
        vm_run_digital(vm);
        vm_run_analog(vm);
        vm_run_phase_b(vm);

        double alpha = 1.0 / vm->time_step;
        vm->solver->g_dirty = 1;
        solver_factorize(vm->solver, alpha, nullptr);

        int n = vm->solver->sys->size;
        Eigen::VectorXd x_zero = Eigen::VectorXd::Zero(n);
        const Eigen::VectorXd &x_init = solver_solve_rhs(
            vm->solver, 1.0, x_zero, alpha, nullptr, nullptr);
        solver_advance_time(vm->solver, x_init);
    }
}

void vm_run_until(VM *vm, double target_time) {
    vm->end_time = target_time;
    if (vm->time >= vm->end_time) {
        return;
    }

    // g_dirty is set unconditionally here (matching the original vm_run,
    // which set it after the boot if/else regardless of which branch ran)
    // so every subsequent HVR_step/HVR_run call re-marks the solver dirty,
    // not just the very first one.
    vm->solver->g_dirty = 1;

    if (vm->strategy) {
        vm->strategy->run(vm);
    }
}

void vm_run(VM *vm) {
    printf("[VM] Starting Co-Simulation... T_stop: %.3e, dt: %.3e\n",
           vm->end_time, vm->time_step);

    vm_boot(vm);
    vm_run_until(vm, vm->end_time);

    printf("[VM] Simulation Complete.\n");
    logger_export_csv(&vm->logger, "simulation_output.csv");
}