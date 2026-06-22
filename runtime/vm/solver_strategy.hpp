#pragma once

// Forward declaration — VM is defined in vm.hpp
struct VM;

// ─────────────────────────────────────────────────────────────────────────────
// SOLVER STRATEGY
// Pure virtual interface — each solver implements Run(vm).
// Mirrors Go: type SolverStrategy interface { Run(vm *VM) }
// ─────────────────────────────────────────────────────────────────────────────

struct SolverStrategy {
    virtual void run(VM *vm) = 0;
    virtual ~SolverStrategy() = default;
};