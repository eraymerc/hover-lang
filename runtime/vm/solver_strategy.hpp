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

    // Does this solver choose its own dt?
    //
    // Asked before the transient starts, by vm_boot, to decide whether
    // vm->time_step may be replaced with the cold-start value (see
    // cold_start_dt in solvers/step_limits.hpp). The distinction is not
    // cosmetic: on a VARIABLE-step solver vm->time_step carries .tran's t_step
    // as a CEILING and the opening step should be well under it, while on a
    // FIXED-step solver vm->time_step IS every step the run will ever take and
    // must be left exactly as the deck wrote it.
    //
    // This is the runtime's copy of the same split the compiler encodes as
    // solverFields.maxDt (compiler/codegen/utils.go) — a struct that declares
    // max_dt is a struct that adapts. Keep the two in agreement: a solver added
    // to one and not the other will either lose its ceiling or silently have
    // its fixed step divided by ten.
    virtual bool is_adaptive() const { return false; }

    virtual ~SolverStrategy() = default;
};