#pragma once

#include <Eigen/Dense>
#include "../vm/solver_strategy.hpp"

// ─────────────────────────────────────────────────────────────────────────────
// GAUSS-SEIDEL
// Backward Euler with fixed-point iteration and under-relaxation (ω = 0.05).
// No Jacobian computed. Converges slowly but never diverges on well-posed
// circuits. Use as a lightweight diagnostic solver when Newton-Raphson
// convergence is unreliable.
//
// Mirrors Go: type GaussSiedel struct { Rtol, Atol float64; MaxIter int; MaxStep float64 }
// ─────────────────────────────────────────────────────────────────────────────

struct GaussSiedel : SolverStrategy {
    double rtol     = 1e-3;
    double atol     = 1e-6;
    int    max_iter = 500;
    double max_step = 1.0;

    void run(VM *vm) override;

private:
    bool has_converged(const Eigen::VectorXd &x_new, const Eigen::VectorXd &x_old) const;
};
