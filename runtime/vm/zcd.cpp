#include "zcd.hpp"
#include "vm.hpp"

#include <algorithm>
#include <cmath>
#include <vector>

Eigen::VectorXd vm_drive_vector(VM *vm) {
    System *sys = vm->solver->sys;
    const Eigen::VectorXd b_saved = sys->B_static;
    std::vector<double> cs_saved;
    cs_saved.reserve(sys->current_sources.size());
    for (const auto &kv : sys->current_sources) cs_saved.push_back(kv.second.last_value);

    vm_run_phase_b(vm);
    Eigen::VectorXd drive = sys->B_static;

    sys->B_static = b_saved;
    size_t k = 0;
    for (auto &kv : sys->current_sources) kv.second.last_value = cs_saved[k++];
    return drive;
}

// The drive vector at fraction s of the step: logic re-run from the step's
// starting state at t_n + s*dt with the solution interpolated to x(s).
static Eigen::VectorXd probe(VM *vm, const VMSnapshot &start, const Eigen::VectorXd &x_end,
                             double dt, double s) {
    vm_restore_state(vm, start);
    vm->time = start.time + s * dt;
    const Eigen::VectorXd x = start.last_solution + s * (x_end - start.last_solution);
    vm->api->last_solution = x;
    vm->api->prev_solution = x;
    vm->in_probe = true;
    vm_run_digital(vm);
    vm->in_probe = false;
    return vm_drive_vector(vm);
}

static bool jumped(double a, double b) {
    return std::abs(b - a) > ZCD_REL * std::max(std::abs(a), std::abs(b)) + ZCD_ABS;
}

double vm_locate_event(VM *vm, const VMSnapshot &start, const Eigen::VectorXd &x_end_in, double dt) {
    // A COPY: callers may pass vm->solver->last_solution itself (euler_adaptive
    // has already committed its step), and every probe's restore overwrites it.
    const Eigen::VectorXd x_end = x_end_in;
    // Everything the probes disturb, to put back at the end.
    const VMSnapshot here = vm_save_state(vm);
    const Eigen::VectorXd api_last = vm->api->last_solution;
    const Eigen::VectorXd api_prev = vm->api->prev_solution;

    const Eigen::VectorXd p0 = probe(vm, start, x_end, dt, 0.0);
    const Eigen::VectorXd p1 = probe(vm, start, x_end, dt, 1.0);

    // Components that changed at all; only they need the midpoint test.
    std::vector<int> moved;
    for (int i = 0; i < p0.size(); i++)
        if (jumped(p0(i), p1(i))) moved.push_back(i);

    double result = -1.0;
    if (!moved.empty()) {
        const Eigen::VectorXd pm = probe(vm, start, x_end, dt, 0.5);
        std::vector<int> jumps;
        for (int i : moved) {
            const double change = p1(i) - p0(i);
            if (std::abs(p1(i) - 2.0 * pm(i) + p0(i)) > 0.5 * std::abs(change)) jumps.push_back(i);
        }
        if (!jumps.empty()) {
            // Post-jump: at least one jumping component is nearer its end value.
            auto past = [&](const Eigen::VectorXd &p) {
                for (int i : jumps)
                    if (std::abs(p(i) - p0(i)) > 0.5 * std::abs(p1(i) - p0(i))) return true;
                return false;
            };
            double a = 0.0, b = 1.0;
            if (past(pm)) b = 0.5; else a = 0.5;
            const double frac = std::max(ZCD_FRAC, 1e-15 / dt);   // ~femtosecond floor
            while (b - a > frac) {
                const double mid = 0.5 * (a + b);
                if (past(probe(vm, start, x_end, dt, mid))) b = mid; else a = mid;
            }
            result = b;
        }
    }

    // The probes left the logic's non-state outputs at the last probe's
    // values; replay the step's own logic pass from its start (ticks included,
    // exactly as the step ran it), then restore the caller's state.
    vm_restore_state(vm, start);
    api_update_solution(vm->api, start.last_solution);
    vm_run_digital(vm);
    vm_restore_state(vm, here);
    vm->api->last_solution = api_last;
    vm->api->prev_solution = api_prev;
    return result;
}

double vm_check_zero_crossing(VM *vm, double dt) {
    if (!vm->zcd_enabled) return dt;
    const VMSnapshot start = vm_save_state(vm);
    const double s = vm_locate_event(vm, start, start.last_solution, dt);
    return (s > 0.0 && s < 1.0) ? s * dt : dt;
}
