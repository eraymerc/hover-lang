#pragma once

#include <Eigen/Dense>
#include "../vm/solver_strategy.hpp"

// ─────────────────────────────────────────────────────────────────────────────
// BDF2
// 2nd-order Backward Differentiation Formula (Gear's method) with Modified
// Newton-Raphson and adaptive timestepping. L-stable — actively damps
// spurious oscillations after switching events. Uses a Backward Euler primer
// on startup and after any step rejection.
//
// Jacobian reuse policy mimics MATLAB's ode15s, as documented in Shampine &
// Reichelt, "The MATLAB ODE Suite" (1997), Section 2.3: "The rate of
// convergence is monitored and the iteration terminated if it is predicted
// that convergence will not be achieved in four iterations. Should this
// happen and the Jacobian not be current, a new Jacobian is formed.
// Otherwise the step size is reduced." This is a meaningfully more precise
// rule than a flat staleness counter or a single converged/not-converged
// check — it predicts failure EARLY (without burning the full iteration
// budget) using the observed contraction rate between successive corrections,
// and only re-forms the Jacobian if it wasn't already freshly computed this
// step. If it WAS already current and convergence is still predicted to
// fail, the step size is reduced instead — the Jacobian is not blamed twice
// in the same step.
//
// Per-iteration voltage clamping is replaced by diagonally-scaled
// trust-region damping (see newton_trust_region.hpp) for the same reasons
// established for TR-BDF2: a fixed absolute clamp tuned for one circuit's
// voltage scale does not generalize.
// ─────────────────────────────────────────────────────────────────────────────

struct BDF2 : SolverStrategy {
    double rtol     = 1e-3;
    double atol     = 1e-6;
    int    max_iter = 100;
    double min_dt   = 1e-12;
    double max_dt   = 0.0;  // 0 = use vm->time_step at first run() call

    void run(VM *vm) override;

private:
    Eigen::VectorXd x_prev1, x_prev2;
    bool has_converged(const Eigen::VectorXd &x_new, const Eigen::VectorXd &x_old) const;
};