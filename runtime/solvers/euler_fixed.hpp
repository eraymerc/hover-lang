#pragma once

#include "../vm/solver_strategy.hpp"

// ─────────────────────────────────────────────────────────────────────────────
// EULER FIXED
// Forward Euler, fixed timestep, first-order.
// One derivative evaluation per step, no error control.
// Use only as a debugging baseline.
//
// Mirrors Go: type EulerFixed struct{}
// ─────────────────────────────────────────────────────────────────────────────

struct EulerFixed : SolverStrategy {
    void run(VM *vm) override;
};