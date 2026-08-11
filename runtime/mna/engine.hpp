#pragma once

#include "system.hpp"

// ─────────────────────────────────────────────────────────────────────────────
// MNA ENGINE
//
// The Solver holds the factorized LU decomposition and pre-allocated scratch
// vectors. It wraps Eigen's PartialPivLU for the linear solve.
//
// Mirrors Go:
//   type Solver struct {
//       Sys          *System
//       LU           mat.LU
//       LastSolution []float64
//       gEffData     []float64
//       lastAlpha    float64
//       ...scratchpads
//   }
//
// C-style: Solver is a plain struct, functions take Solver* as first arg.
// ─────────────────────────────────────────────────────────────────────────────

struct Solver {
    System                          *sys;
    Eigen::PartialPivLU<Eigen::MatrixXd> lu;
    Eigen::MatrixXd                  G_eff;      // G + alpha*C + jacobian, EQUILIBRATED
    Eigen::VectorXd                  last_solution;
    Eigen::VectorXd                  gx_scratch;  // ComputeGx result buffer
    Eigen::VectorXd                  d_scratch;   // ComputeDerivatives result buffer

    // Equilibration scale factors, all exact powers of two. G_eff as factorized
    // is row_scale.asDiagonal() * (G + alpha*C + J) * col_scale.asDiagonal(),
    // so a solve scales the RHS by row_scale going in and the result by
    // col_scale coming out. See solver_factorize for why this is needed.
    Eigen::VectorXd                  row_scale;
    Eigen::VectorXd                  col_scale;
    Eigen::VectorXd                  rhs_scratch; // scaled RHS, kept to avoid a malloc per solve

    double                           last_alpha;
    int                              g_dirty;     // 1 = must refactorize
};

// ─────────────────────────────────────────────────────────────────────────────
// LIFECYCLE
// ─────────────────────────────────────────────────────────────────────────────

void solver_init(Solver *s, System *sys);

// ─────────────────────────────────────────────────────────────────────────────
// PRIMITIVE 1 — FACTORIZE
// Builds G_eff = G + alpha*C + jacobian and runs LU decomposition.
// jacobian may be NULL for linear circuits.
// Mirrors Go: func (s *Solver) Factorize(alpha float64, jacobian []float64)
// ─────────────────────────────────────────────────────────────────────────────

void solver_factorize(Solver *s, double alpha, const Eigen::MatrixXd *jacobian);

// ─────────────────────────────────────────────────────────────────────────────
// PRIMITIVE 2 — SOLVE RHS
// Builds B_dynamic and solves G_eff * x = B_dynamic.
// Refactorizes only when g_dirty or alpha changed.
// Mirrors Go: func (s *Solver) SolveRHS(...) []float64
// ─────────────────────────────────────────────────────────────────────────────

const Eigen::VectorXd& solver_solve_rhs(
    Solver               *s,
    double                b_scale,
    const Eigen::VectorXd &x_history,
    double                alpha,
    const Eigen::VectorXd *correction,
    const Eigen::MatrixXd *jacobian);

// ─────────────────────────────────────────────────────────────────────────────
// PRIMITIVE 3 — COMPUTE G*x
// Returns G * x (static conductance matrix times solution vector).
// Mirrors Go: func (s *Solver) ComputeGx(x []float64) []float64
// ─────────────────────────────────────────────────────────────────────────────

const Eigen::VectorXd& solver_compute_gx(Solver *s, const Eigen::VectorXd &x);

// ─────────────────────────────────────────────────────────────────────────────
// COMMIT — ADVANCE TIME
// Copies solution into last_solution and updates the API.
// Mirrors Go: func (s *Solver) AdvanceTime(solution []float64, api *API)
// ─────────────────────────────────────────────────────────────────────────────

void solver_advance_time(Solver *s, const Eigen::VectorXd &solution);

// ─────────────────────────────────────────────────────────────────────────────
// CONVENIENCE — FIXED STEP
// Single forward Euler step using last_solution as history.
// Mirrors Go: func (s *Solver) Step(api *API)
// ─────────────────────────────────────────────────────────────────────────────

void solver_step(Solver *s);

// ─────────────────────────────────────────────────────────────────────────────
// CONVENIENCE — COMPUTE DERIVATIVES
// Returns dx/dt = C^{-1} * (B_static - G*x) for explicit ODE solvers.
// Mirrors Go: func (s *Solver) ComputeDerivatives(x []float64) []float64
// ─────────────────────────────────────────────────────────────────────────────

const Eigen::VectorXd& solver_compute_derivatives(Solver *s, const Eigen::VectorXd &x);