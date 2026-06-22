#pragma once

#include <Eigen/Dense>
#include "../vm/solver_strategy.hpp"

// ─────────────────────────────────────────────────────────────────────────────
// TRAPEZOIDAL FIXED
// 2nd-order Crank-Nicolson with Modified Newton-Raphson, FIXED timestep
// (renamed from the original Trapezoidal, which was already fixed-step
// despite not having "_fixed" in its name).
//
// A-stable and energy-conservative. Susceptible to numerical ringing after
// hard discontinuities.
//
// Per-iteration damping uses diagonally-scaled trust-region (see
// newton_trust_region.hpp) instead of the original fixed absolute voltage
// clamp (max_step=0.05) — same reasoning as tr_bdf23 and bdf2_fixed: a
// fixed clamp tuned for one circuit's scale does not generalize.
//
// Deliberately has NO adaptive step-size control and NO embedded error
// estimate — this is intentional, not an oversight. Part of validating the
// Newton/Jacobian foundation independent of adaptive-step-size logic,
// since every convergence failure investigated so far involved adaptive
// dt/error-estimate machinery interacting with Newton, never a pure fixed-
// step Newton solve failing on its own. Isolating that variable makes it
// possible to tell whether a future failure is "Newton itself is wrong"
// or "the adaptive layer is wrong" — conflating the two was a real
// difficulty in earlier debugging sessions.
// ─────────────────────────────────────────────────────────────────────────────

struct TrapezoidalFixed : SolverStrategy {
    double rtol     = 1e-3;
    double atol     = 1e-6;
    int    max_iter = 100;

    void run(VM *vm) override;

private:
    bool trap_converged(const Eigen::VectorXd &x_new, const Eigen::VectorXd &x_old) const;
};