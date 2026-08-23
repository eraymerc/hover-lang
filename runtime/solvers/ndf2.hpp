#pragma once

#include <Eigen/Dense>
#include "../vm/solver_strategy.hpp"
#include "newton_core.hpp"

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
// JACOBIAN REUSE POLICY — identical to BDF2's, and for the same measured
// reasons. One fresh Jacobian per ACCEPTED step; within a step, Modified Newton
// by default with a progress-triggered re-form every eighth iteration (see
// newton_core.hpp); on a failed step, blame the Jacobian before dt — if it was
// not formed during this step, retry at the SAME dt with a fresh one, and only
// a step that fails on a fresh Jacobian is treated as a step-size problem.
//
// This solver previously carried a Jacobian across accepted steps indefinitely,
// clearing it only when a step FAILED, and it cost correct answers rather than
// merely speed: examples/Diode/rectifier.hvr returned 0.25 .. 22.18 V — an
// unfiltered waveform, as though the reservoir capacitor were absent — where the
// identical netlist under bdf2 gives 12.14 .. 22.53 V, and examples/BJT/
// npn_amp.hvr biased to a collector sitting at 0.21 V instead of swinging
// 8.2 .. 12.3 V. examples/Optoelectronics/phototransistor/optocoupler.hvr took
// 3.9 MILLION steps and 102 seconds without reaching the end of the run. All
// three are the stale-Jacobian signature documented at bdf2.cpp's accept site:
// the iteration keeps the true solution as its fixed point for any invertible J,
// so it stays plausible while degrading from quadratic to linear convergence,
// and at a commutation the contraction crosses 1.
//
// Newton damping is trust-region based (see newton_trust_region.hpp) rather than
// a fixed voltage clamp, as for BDF2.
// ─────────────────────────────────────────────────────────────────────────────

struct NDF2 : SolverStrategy {
    double rtol     = 1e-3;

    // atol is the VOLTAGE tolerance and abstol the CURRENT one, matching SPICE's
    // vntol/abstol — the solution vector holds node voltages in rows
    // 0..num_nodes-1 and branch currents after them, and one absolute tolerance
    // cannot serve both. See newton_core.hpp.
    double atol     = 1e-6;    // volts  — .solver argument 2
    double abstol   = 1e-12;   // amps   — .solver argument 5
    int    max_iter = 100;
    double max_dt   = 0.0;  // 0 = use vm->time_step at first run() call

    void run(VM *vm) override;
    bool is_adaptive() const override { return true; }

private:
    Eigen::VectorXd x_prev1, x_prev2;

    // Row index where branch currents start; everything below it is a node
    // voltage. Captured in run() so the convergence test can pick the right
    // absolute tolerance per row.
    int n_nodes = 0;

    // What the last convergence test measured — see newton_core.hpp.
    ConvergenceReport conv;
};