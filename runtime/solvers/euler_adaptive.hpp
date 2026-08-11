#pragma once

#include <Eigen/Dense>
#include <utility>
#include "../vm/solver_strategy.hpp"

// ─────────────────────────────────────────────────────────────────────────────
// EULER ADAPTIVE
// Backward Euler with Modified Newton-Raphson and convergence-driven
// adaptive timestepping. Expands dt by 1.5x when the inner loop converges
// quickly, contracts by 0.5x on failure.
//
// Per-iteration damping uses diagonally-scaled trust-region (see
// newton_trust_region.hpp) instead of the original fixed absolute voltage
// clamp (max_step=0.05) — kept consistent with bdf2/ndf2/trapezoidal_fixed,
// which all made the same change earlier: a fixed clamp tuned for one
// circuit's scale does not generalize to circuits at a different
// voltage/current scale.
// ─────────────────────────────────────────────────────────────────────────────

struct EulerAdaptive : SolverStrategy {
    double rtol     = 1e-3;
    double atol     = 1e-6;
    int    max_iter = 100;
    double max_dt   = 0.0;  // 0 = use vm->time_step at first run() call

    void run(VM *vm) override;
    bool is_adaptive() const override { return true; }

private:
    // Returns (iterations, converged)
    std::pair<int, bool> take_implicit_step(VM *vm, double dt);
    bool has_converged(const Eigen::VectorXd &x_new, const Eigen::VectorXd &x_old) const;
};