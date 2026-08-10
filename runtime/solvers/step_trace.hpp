#pragma once

#include <cstdio>
#include <cstdlib>

// ─────────────────────────────────────────────────────────────────────────────
// OPT-IN STEP-SIZE TRACE
//
// Set HOVER_TRACE_STEP in the environment to get one CSV line on stderr per
// step ATTEMPT (not per accepted step — rejected attempts are the interesting
// ones):
//
//   [step] solver,attempt,t,dt,iters,event
//
// `event` is what the controller decided after the attempt:
//
//   accept       taken; dt unchanged (iteration count landed in the dead band)
//   grow         taken; dt raised because the step was easy
//   shrink       taken; dt lowered because the step was hard
//   jac-refresh  failed; Jacobian re-formed and the SAME dt retried
//   reject       failed with a current Jacobian; dt cut
//
// This exists because the step controller is chosen entirely from Newton
// iteration counts, with no truncation-error estimate anywhere, and the only
// externally visible consequence is the total step count printed at the end of
// a run. That is not enough to tell a solver that is genuinely working hard
// from one stuck in a grow/fail/shrink cycle making no net progress — a
// distinction that has already mattered once, in the ndf2 rejection path.
//
// Off unless the variable is set, and one getenv for the whole run when it is.
// Note that a stiff run can produce millions of lines: redirect stderr, and
// prefer a short .tran window when tracing.
// ─────────────────────────────────────────────────────────────────────────────

inline bool step_trace_enabled() {
    static const bool on = (getenv("HOVER_TRACE_STEP") != nullptr);
    return on;
}

inline void step_trace(const char *solver, long attempt, double t, double dt,
                       int iters, const char *event) {
    if (!step_trace_enabled()) return;
    fprintf(stderr, "[step] %s,%ld,%.9e,%.6e,%d,%s\n",
            solver, attempt, t, dt, iters, event);
}
