#pragma once

#include "system.hpp"
#include "engine.hpp"

// ─────────────────────────────────────────────────────────────────────────────
// MNA API
//
// The bridge between the VM (logic execution) and the MNA engine.
// Exposes V(), I() for reading node voltages and branch currents,
// and setters that update B_static so changes are picked up next solve.
//
// Mirrors Go:
//   type API struct {
//       sys          *System
//       lastSolution []float64
//       GDirty       bool
//   }
//
// C-style: plain struct, free functions take API* as first arg.
// ─────────────────────────────────────────────────────────────────────────────

struct API {
    System          *sys;
    Solver          *solver;
    Eigen::VectorXd  last_solution;  // updated after every solve
    // prev_solution holds whatever last_solution was BEFORE the most
    // recent api_update_solution call — i.e. the previous Newton
    // iteration's guess, within the SAME timestep's solve. This is
    // distinct from any timestep-level "previous value" (Hover's `state`
    // variables already cover that) — prev_solution specifically tracks
    // iteration-to-iteration history within one Newton solve, which is
    // what every pnjlim/fetlim/sinhlim-style limiting function needs by
    // definition: "limiting" is inherently a question about how much a
    // value CHANGED between consecutive Newton guesses, not a property
    // of any single value in isolation.
    Eigen::VectorXd  prev_solution;
    int              g_dirty;        // 1 = G changed, must refactorize
};

// ─────────────────────────────────────────────────────────────────────────────
// LIFECYCLE
// ─────────────────────────────────────────────────────────────────────────────

void api_init(API *api, System *sys, Solver *solver);

// Copies solution into last_solution. Called by engine after every solve.
// Mirrors Go: func (api *API) UpdateSolution(solution []float64)
void api_update_solution(API *api, const Eigen::VectorXd &solution);

// Make a trial point visible to V()/I() reads WITHOUT advancing the
// Newton-iteration history. Sets last_solution but leaves prev_solution
// untouched. Used by internal PROBES — the numerical Jacobian's per-node
// perturbation (vm_compute_jacobian) and the trust region's residual
// scoring (residual_at) — which must evaluate device currents at a trial
// point without that point being mistaken for "the previous Newton
// iterate" by nr_prev(). Only api_update_solution advances prev_solution,
// and it must be called exactly once per real Newton iteration; probes use
// this instead so nr_prev()/api_V_prev()/api_I_prev() stay correct.
void api_peek_solution(API *api, const Eigen::VectorXd &solution);

// ─────────────────────────────────────────────────────────────────────────────
// READ OPERATIONS
// ─────────────────────────────────────────────────────────────────────────────

// Voltage at a named net node from the last MNA solution.
// Returns 0.0 for ground or unknown nets.
// Language intrinsic: V(netName)
double api_V(const API *api, const char *net_name);

// Branch current through a named element (V, L, E, H sources only).
// Returns 0.0 with a warning for unknown element names.
// Language intrinsic: I(elementName)
double api_I(const API *api, const char *element_name);

// Voltage/current as of the PREVIOUS Newton-Raphson iteration within the
// current timestep's solve — i.e. what api_V/api_I would have returned
// immediately before the most recent api_update_solution call. Backing
// store for the nr_prev(expr) language construct (see elaborator's
// processAnalogNrPrev), which rewrites any V()/I() call nested inside an
// nr_prev(...) argument to use these instead of the plain current-value
// reads. Returns 0.0 for ground/unknown nets, matching api_V/api_I.
double api_V_prev(const API *api, const char *net_name);
double api_I_prev(const API *api, const char *element_name);

// ─────────────────────────────────────────────────────────────────────────────
// WRITE OPERATIONS
// ─────────────────────────────────────────────────────────────────────────────

// Update output voltage of a named voltage source.
// Only touches B_static — no LU refactorization needed.
// Language usage: voltage_source(signal)[out, gnd]
void api_set_voltage_source(API *api, const char *name, double voltage);

// Update output current of a named current source.
// Unstamps old value then stamps new value into B_static.
// Language usage: current_source(signal)[out, gnd]
void api_set_current_source(API *api, const char *name, double current);