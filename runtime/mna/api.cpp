#include "api.hpp"
#include <cstring>
#include <cstdio>

// ─────────────────────────────────────────────────────────────────────────────
// LIFECYCLE
// ─────────────────────────────────────────────────────────────────────────────

void api_init(API *api, System *sys, Solver *solver) {
    api->sys           = sys;
    api->solver        = solver;
    api->last_solution = Eigen::VectorXd::Zero(sys->size);
    api->prev_solution = Eigen::VectorXd::Zero(sys->size);
    api->g_dirty       = 0;
}

void api_update_solution(API *api, const Eigen::VectorXd &solution) {
    // Save whatever WAS current before overwriting it — this becomes
    // "the previous Newton iteration's value" for nr_prev() reads until
    // the NEXT call to this function. On the very first call of a run,
    // prev_solution stays whatever it was default-constructed to (a
    // zero-size Eigen vector), which is intentional: there is no
    // meaningful "previous iteration" before the first one, the same
    // edge case idt()/ddt() and the NDF2/TR-BDF23 solvers already handle
    // via their own cold-start conventions.
    api->prev_solution = api->last_solution;
    api->last_solution = solution;
}

// Set the read-view (last_solution) WITHOUT shifting prev_solution. This is
// the probe path: the Jacobian's finite-difference perturbation and the
// trust region's residual_at both need V()/I() to reflect a trial point so
// the analog phase computes device currents there — but a trial point is
// NOT a Newton iterate, so it must never become nr_prev()'s answer. Callers
// that perturb the read-view are responsible for restoring it (to the real
// current iterate) when finished, so the next api_update_solution shifts the
// correct value into prev_solution.
void api_peek_solution(API *api, const Eigen::VectorXd &solution) {
    api->last_solution = solution;
}

// ─────────────────────────────────────────────────────────────────────────────
// READ — V()
// Mirrors Go:
//   func (api *API) V(netName string) float64 {
//       if netName == "gnd" || netName == "0" { return 0.0 }
//       idx, ok := api.sys.NodeMap[netName]
//       if !ok { warn; return 0.0 }
//       return api.lastSolution[idx]
//   }
// ─────────────────────────────────────────────────────────────────────────────

double api_V(const API *api, const char *net_name) {
    if (strcmp(net_name, "gnd") == 0 || strcmp(net_name, "0") == 0) {
        return 0.0;
    }
    int idx = api->sys->resolve_node(net_name);
    if (idx < 0) {
        fprintf(stderr, "[MNA API] warning: V(\"%s\") — net not found, returning 0\n", net_name);
        return 0.0;
    }
    return api->last_solution(idx);
}

// ─────────────────────────────────────────────────────────────────────────────
// READ — I()
// Mirrors Go:
//   func (api *API) I(elementName string) float64 {
//       idx, ok := api.sys.BranchNameToIdx[elementName]
//       if !ok { warn; return 0.0 }
//       return api.lastSolution[idx]
//   }
// ─────────────────────────────────────────────────────────────────────────────

double api_I(const API *api, const char *element_name) {
    int idx = api->sys->resolve_branch(element_name);
    if (idx < 0) {
        fprintf(stderr, "[MNA API] warning: I(\"%s\") — element has no branch current, returning 0\n",
                element_name);
        return 0.0;
    }
    return api->last_solution(idx);
}

// ─────────────────────────────────────────────────────────────────────────────
// READ — V_prev()/I_prev() — previous Newton iteration's value
//
// Backing store for nr_prev(expr). Bounds-checked against prev_solution's
// actual size (not just sys->size) because prev_solution may still be its
// default-constructed zero-size state on the very first Newton iteration
// of a run, before api_update_solution has ever been called twice — in
// that case there is genuinely no previous iteration to report, and 0.0
// is returned the same way an unknown net would be.
// ─────────────────────────────────────────────────────────────────────────────

double api_V_prev(const API *api, const char *net_name) {
    if (strcmp(net_name, "gnd") == 0 || strcmp(net_name, "0") == 0) {
        return 0.0;
    }
    int idx = api->sys->resolve_node(net_name);
    if (idx < 0 || idx >= api->prev_solution.size()) {
        return 0.0;
    }
    return api->prev_solution(idx);
}

double api_I_prev(const API *api, const char *element_name) {
    int idx = api->sys->resolve_branch(element_name);
    if (idx < 0 || idx >= api->prev_solution.size()) {
        return 0.0;
    }
    return api->prev_solution(idx);
}

// ─────────────────────────────────────────────────────────────────────────────
// WRITE — SetVoltageSource
// Direct assignment to B_static at the branch row.
// No G change — LU stays valid.
//
// Mirrors Go:
//   func (api *API) SetVoltageSource(name string, voltage float64) {
//       branchIdx := api.sys.BranchNameToIdx[name]
//       api.sys.B_static[branchIdx] = voltage
//   }
// ─────────────────────────────────────────────────────────────────────────────

void api_set_voltage_source(API *api, const char *name, double voltage) {
    int idx = api->sys->resolve_branch(name);
    if (idx < 0) {
        fprintf(stderr, "[MNA API] error: SetVoltageSource(\"%s\") — element not found\n", name);
        return;
    }
    // source_alpha makes the .op source-stepping ladder reach DRIVEN sources:
    // this restamp is an absolute overwrite, so applying the scale here is the
    // only way Jacobian and residual probes (which re-run phase_b) stay
    // alpha-consistent. It is 1.0 outside vm_solve_op, so the transient is
    // unaffected. api_set_current_source is deliberately NOT scaled — junction
    // device currents flow through it, and those are the nonlinearities.
    api->sys->B_static(idx) = voltage * api->sys->source_alpha;
}

// ─────────────────────────────────────────────────────────────────────────────
// WRITE — SetCurrentSource
// Unstamps old value from B_static then stamps new value.
// No G change — LU stays valid.
//
// Mirrors Go:
//   func (api *API) SetCurrentSource(name string, current float64) {
//       rec := api.sys.CurrentSources[name]
//       // unstamp old
//       B_static[rec.N1] += rec.LastValue
//       B_static[rec.N2] -= rec.LastValue
//       // stamp new
//       B_static[rec.N1] -= current
//       B_static[rec.N2] += current
//       rec.LastValue = current
//   }
// ─────────────────────────────────────────────────────────────────────────────

void api_set_current_source(API *api, const char *name, double current) {
    CurrentSourceRecord *rec = api->sys->find_current_source(name);
    if (rec == nullptr) {
        fprintf(stderr, "[MNA API] error: SetCurrentSource(\"%s\") — current source not found\n", name);
        return;
    }

    // Unstamp old value
    if (rec->n1 >= 0) api->sys->B_static(rec->n1) += rec->last_value;
    if (rec->n2 >= 0) api->sys->B_static(rec->n2) -= rec->last_value;

    // Stamp new value
    if (rec->n1 >= 0) api->sys->B_static(rec->n1) -= current;
    if (rec->n2 >= 0) api->sys->B_static(rec->n2) += current;

    rec->last_value = current;
}