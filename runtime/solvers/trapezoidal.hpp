#pragma once

#include <Eigen/Dense>
#include "../vm/solver_strategy.hpp"

// ─────────────────────────────────────────────────────────────────────────────
// TRAPEZOIDAL
// True variable-step Trapezoidal solver with iteration-count adaptation.
// Uses Backward Euler exclusively for post-ZCD ringing suppression and 
// recovery from failed convergence steps.
//
// Per-iteration damping uses diagonally-scaled trust-region (see
// newton_trust_region.hpp) instead of the original fixed absolute voltage
// clamp (max_step = 0.05 V). The fixed clamp was a semiconductor-specific
// assumption (it mirrors SPICE's pnjlim junction limit) and does not
// generalize to a Verilog-AMS-style language where a node may be at any
// scale. The trust region scores each candidate against the TRUE nonlinear
// residual — and crucially, the residual is built from the physical
// correction only (trap_base in Trapezoidal mode, none in Euler mode), never
// the Newton linearization term J*x_guess, which would otherwise blind the
// accept/reject decision at stiff junctions.
// ─────────────────────────────────────────────────────────────────────────────

struct Trapezoidal : SolverStrategy {
    double rtol     = 1e-3;
    double atol     = 1e-6;
    int    max_iter = 50;   // SPICE typically limits NR loops to ~20-50 max
    double max_dt   = 0.0;

    void run(VM *vm) override;
    bool is_adaptive() const override { return true; }

private:
    bool has_converged(const Eigen::VectorXd &x_new, const Eigen::VectorXd &x_old) const;
};