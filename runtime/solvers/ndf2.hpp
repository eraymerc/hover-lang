#pragma once

#include <Eigen/Dense>
#include "../vm/solver_strategy.hpp"

// ─────────────────────────────────────────────────────────────────────────────
// NDF2
//
// Shampine & Reichelt's order-2 Numerical Differentiation Formula — the
// real algorithm underlying MATLAB's ode15s (a variable-order solver
// covering orders 1-5; this implementation covers orders 1-2 only).
// NDFs generalize the Backward Differentiation Formulas (BDFs / Gear's
// method) by adding a correction term weighted by a per-order constant
// kappa (Shampine & Reichelt, "The MATLAB ODE Suite," SIAM J. Sci.
// Comput. 18, 1997):
//
//   kappa = [-0.1850, -1/9, -0.0823, -0.0415, 0]   for orders 1 through 5
//
// This implementation covers orders 1 and 2 (NDF1, equivalent to Backward
// Euler with a kappa correction, and NDF2 itself). Orders 3-5 require
// additional solution history (x_prev3, x_prev4) and are deliberately not
// implemented: per Dahlquist's second barrier, no linear multistep method
// of order 3 or higher can be A-stable, and NDF's kappa correction is
// specifically designed to improve accuracy WITHIN that A-stable order-2
// regime, not to extend past it — going to order 3+ would trade away the
// one property (A-stability) that makes this solver trustworthy on
// oscillatory, rotating-frame circuits.
//
// The order-1 NDF reduces to:
//   y_{n+1} - y_n - kappa_1*(y_{n+1} - 2y_n + y_{n-1}) = h*f(t_{n+1}, y_{n+1})
//
// The order-2 NDF:
//   y_{n+1} - (4/3)y_n + (1/3)y_{n-1} - kappa_2*(2/3)*(y_{n+1} - 2y_n + y_{n-1})
//     = (2/3)*h*f(t_{n+1}, y_{n+1})
//
// Real MATLAB ode15s is variable-order, automatically selecting among
// orders 1-5 based on observed convergence and error behavior. This
// implementation starts at order 1 and switches to order 2 once enough
// solution history is available — a simplified order-selection rule, not
// MATLAB's full error-estimator-based scheme (which requires the embedded
// local error estimate machinery this engine does not yet implement
// reliably for any solver — see tr_bdf23's history with this).
//
// Jacobian reuse and Newton damping follow the same policies established
// for BDF2/TR-BDF2: trust-region damping (see newton_trust_region.hpp)
// instead of a fixed voltage clamp, and a Jacobian that carries over
// across steps until a convergence failure proves it stale (using
// has_converged itself as the staleness signal, not a flat step counter
// — an earlier counter-based version was tested and found to allow
// silent divergence on a stiff test circuit; see tr_bdf23's history).
// ─────────────────────────────────────────────────────────────────────────────

struct NDF2 : SolverStrategy {
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