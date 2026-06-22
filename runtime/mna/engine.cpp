#include "engine.hpp"
#include <cstring>

// ─────────────────────────────────────────────────────────────────────────────
// LIFECYCLE
// ─────────────────────────────────────────────────────────────────────────────

void solver_init(Solver *s, System *sys) {
    s->sys           = sys;
    s->last_alpha    = -1.0;  // force factorize on first call
    s->g_dirty       = 1;
    s->G_eff         = Eigen::MatrixXd::Zero(sys->size, sys->size);
    s->last_solution = Eigen::VectorXd::Zero(sys->size);
    s->gx_scratch    = Eigen::VectorXd::Zero(sys->size);
    s->d_scratch     = Eigen::VectorXd::Zero(sys->size);
}

// ─────────────────────────────────────────────────────────────────────────────
// PRIMITIVE 1 — FACTORIZE
// G_eff = G + alpha*C [+ jacobian]
// Mirrors Go:
//   s.gEffData[i*n+j] = G[i,j] + alpha*C[i,j]
//   if jacobian != nil { s.gEffData[i] += jacobian[i] }
//   s.LU.Factorize(gEff)
// ─────────────────────────────────────────────────────────────────────────────

void solver_factorize(Solver *s, double alpha, const Eigen::MatrixXd *jacobian) {
    s->G_eff = s->sys->G + alpha * s->sys->C;
    if (jacobian != nullptr) {
        s->G_eff += *jacobian;
    }
    s->lu.compute(s->G_eff);
    s->last_alpha = alpha;
}

// ─────────────────────────────────────────────────────────────────────────────
// PRIMITIVE 2 — SOLVE RHS
// Builds B_dynamic = b_scale*B_static + alpha*C*x_history [+ correction]
// then solves G_eff * x = B_dynamic.
//
// Mirrors Go:
//   for i, b := range s.Sys.B_static { s.Sys.B_dynamic[i] = bScale * b }
//   s.Sys.C.DoNonZero(func(i,j,v){ B_dynamic[i] += alpha*v*xHistory[j] })
//   if correction != nil { B_dynamic[i] += correction[i] }
//   s.LU.SolveTo(xDense, false, bDense)
// ─────────────────────────────────────────────────────────────────────────────

const Eigen::VectorXd& solver_solve_rhs(
    Solver               *s,
    double                b_scale,
    const Eigen::VectorXd &x_history,
    double                alpha,
    const Eigen::VectorXd *correction,
    const Eigen::MatrixXd *jacobian)
{
    // Refactorize only when topology changed or alpha shifted
    if (s->g_dirty || s->last_alpha != alpha) {
        solver_factorize(s, alpha, jacobian);
        s->g_dirty = 0;
    }

    // Build RHS into B_dynamic
    s->sys->B_dynamic = b_scale * s->sys->B_static
                      + alpha   * s->sys->C * x_history;

    if (correction != nullptr) {
        s->sys->B_dynamic += *correction;
    }

    // Solve and store result back into last_solution for return
    s->last_solution = s->lu.solve(s->sys->B_dynamic);
    return s->last_solution;
}

// ─────────────────────────────────────────────────────────────────────────────
// PRIMITIVE 3 — COMPUTE G*x
// Returns reference to internal scratch — valid until next call.
// Mirrors Go: s.Sys.G.DoNonZero(func(i,j,v){ gxScratch[i] += v*x[j] })
// ─────────────────────────────────────────────────────────────────────────────

const Eigen::VectorXd& solver_compute_gx(Solver *s, const Eigen::VectorXd &x) {
    s->gx_scratch = s->sys->G * x;
    return s->gx_scratch;
}

// ─────────────────────────────────────────────────────────────────────────────
// COMMIT — ADVANCE TIME
// Mirrors Go: copy(s.LastSolution, solution); api.UpdateSolution(solution)
// The API copy is handled by the caller (vm layer reads last_solution directly).
// ─────────────────────────────────────────────────────────────────────────────

void solver_advance_time(Solver *s, const Eigen::VectorXd &solution) {
    s->last_solution = solution;
}

// ─────────────────────────────────────────────────────────────────────────────
// CONVENIENCE — FIXED STEP
// Forward Euler: solve with last_solution as history.
// Mirrors Go: func (s *Solver) Step(api *API)
// ─────────────────────────────────────────────────────────────────────────────

void solver_step(Solver *s) {
    double alpha = 1.0 / s->sys->dt;
    const Eigen::VectorXd &result = solver_solve_rhs(
        s, 1.0, s->last_solution, alpha, nullptr, nullptr);
    solver_advance_time(s, result);
}

// ─────────────────────────────────────────────────────────────────────────────
// CONVENIENCE — COMPUTE DERIVATIVES
// dx/dt = C_diag^{-1} * (B_static - G*x)
// Only valid for diagonal C entries (capacitors and inductors are self-only).
//
// Mirrors Go:
//   s.Sys.C.DoNonZero(func(i,j,v){
//       if i==j && v>0 { dScratch[i] = (B_static[i] - gx[i]) / v }
//   })
// ─────────────────────────────────────────────────────────────────────────────

const Eigen::VectorXd& solver_compute_derivatives(Solver *s, const Eigen::VectorXd &x) {
    const Eigen::VectorXd &gx = solver_compute_gx(s, x);
    s->d_scratch.setZero();

    int n = s->sys->size;
    for (int i = 0; i < n; i++) {
        double c = s->sys->C(i, i);
        if (c > 0.0) {
            s->d_scratch(i) = (s->sys->B_static(i) - gx(i)) / c;
        }
    }
    return s->d_scratch;
}