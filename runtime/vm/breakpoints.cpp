// Breakpoints: landing steps on the times the deck's code compares `time`
// against. The why and the rules are in breakpoints.hpp.

#include "breakpoints.hpp"
#include "vm.hpp"
#include "snapshot.hpp"
#include "../solvers/lte.hpp"

#include <cmath>

namespace {

constexpr double INF = std::numeric_limits<double>::infinity();

// ── The comparison, exactly as the generated code would have computed it ───
//
// form = op | U << 2 | O << 4 (codegen/breakpoints.go):
//   op  0 S > x    1 S >= x    2 S < x    3 S <= x
//   U   0 u = t    1 u = t + c 2 u = t - c
//   O   0 S = u    1 S = fmod(u, p)    2 S = (p == 0 ? 0 : fmod(u, p))  (Hover's %)
//       3 S = u - floor(p == 0 ? 0 : u / p) * p                         (Hover's guarded /)
// Every operation is the one the original expression performs, in the same
// order, so the result is bit-identical to the code it replaces (IEEE, no
// contraction: -ffp-contract=off). `c + time` is t + c: addition commutes.

inline double u_of(int form, double t, double c) {
    switch ((form >> 2) & 3) {
    case 1: return t + c;
    case 2: return t - c;
    default: return t;
    }
}

// The time at which u takes the value u (approximately — the search refines it).
inline double t_of(int form, double u, double c) {
    switch ((form >> 2) & 3) {
    case 1: return u - c;
    case 2: return u + c;
    default: return u;
    }
}

inline double s_of(int form, double t, double c, double p) {
    const double u = u_of(form, t, c);
    switch ((form >> 4) & 3) {
    case 1: return std::fmod(u, p);
    case 2: return p == 0.0 ? 0.0 : std::fmod(u, p);
    case 3: return u - std::floor(p == 0.0 ? 0.0 : u / p) * p;
    default: return u;
    }
}

inline bool holds(int form, double s, double x) {
    switch (form & 3) {
    case 0: return s > x;
    case 1: return s >= x;
    case 2: return s < x;
    default: return s <= x;
    }
}

inline bool pred(int form, double t, double x, double c, double p) {
    return holds(form, s_of(form, t, c, p), x);
}

inline double ulp_of(double v) {
    v = std::fabs(v);
    return std::nextafter(v, INF) - v;
}

// The flip of pred near E0, if it is after t_ref: the first double in
// (max(t_ref, E0 - h), E0 + h] where pred differs from its value at the
// bracket's start. h is a few dozen ulps of the magnitudes involved: E0 is
// computed with a handful of roundings, the flip it estimates is exact.
// INF when pred does not change across the bracket.
double flip_near(int form, double x, double c, double p, double t_ref, double E0, double scale) {
    if (!std::isfinite(E0)) return INF;
    const double h = 64.0 * ulp_of(std::fmax(std::fabs(E0), scale));
    double lo = E0 - h, hi = E0 + h;
    if (!(hi > t_ref)) return INF;
    if (lo < t_ref) lo = t_ref;
    const bool a = pred(form, lo, x, c, p);
    if (pred(form, hi, x, c, p) == a) return INF;
    // Bisection down to neighbouring doubles: pred(lo) == a != pred(hi).
    for (int k = 0; k < 2100; k++) {
        const double mid = lo + 0.5 * (hi - lo);
        if (!(mid > lo && mid < hi)) break;
        if (pred(form, mid, x, c, p) == a) lo = mid; else hi = mid;
    }
    return hi;
}

// The first time after t_ref at which the comparison's result changes, or INF.
double next_flip(int form, double x, double c, double p, double t_ref) {
    if (std::isnan(x)) return INF;
    const int O = (form >> 4) & 3;
    if (O == 0 || (O == 3 && p == 0.0)) {
        // S is non-decreasing in t: > and >= turn true once, < and <= false
        // once. Already turned: nothing ahead.
        const bool late = (form & 3) < 2;
        if (pred(form, t_ref, x, c, p) == late) return INF;
        return flip_near(form, x, c, p, t_ref, t_of(form, x, c), std::fmax(std::fabs(c), std::fabs(x)));
    }
    if (!(p != 0.0) || !std::isfinite(p)) return INF;   // fmod(u, 0) is NaN, u % 0 is 0: constant
    // Periodic: S rises with u between wraps at u = m P, and meets the level x
    // at most once per period, at u = m P + r (r = x reduced into [0, P)).
    // Those are the only places it can flip; try the next ones after t_ref.
    const double P = std::fabs(p);
    const double r = x - std::floor(x / P) * P;
    const double m0 = std::floor(u_of(form, t_ref, c) / P);
    double best = INF;
    for (int k = 0; k <= 2; k++) {
        for (int lvl = 0; lvl < 2; lvl++) {
            const double u = (m0 + k) * P + (lvl ? r : 0.0);
            const double e = flip_near(form, x, c, p, t_ref, t_of(form, u, c),
                                       std::fmax(std::fabs(u), std::fabs(c)));
            if (e < best) best = e;
        }
        if (best < INF) break;   // later periods only come after
    }
    return best;
}

// Ascending insertion sort of a short list.
void sort_list(VMBreakpointList &l) {
    for (int i = 1; i < l.n; i++) {
        const double v = l.t[i];
        int j = i - 1;
        while (j >= 0 && l.t[j] > v) { l.t[j + 1] = l.t[j]; j--; }
        l.t[j + 1] = v;
    }
}

// The step's own logic pass, run here, at the start of the attempt, with
// reporting on: what the logic evaluates at t_n is exactly what tells where it
// next flips. It IS the step's pass — the solver's vm_run_digital that follows
// skips the generated logic phases once (Sim::logic_ran_early) and runs only
// the clock ticks. The attempt's checkpoint was taken before this, so a
// rejected attempt rolls it back as before. It reads the solution at t_n, as
// the solver's pass would; the read-view is put back afterwards, so the
// solver's api_update_solution shifts the same vectors as without this (the
// first iterate's nr_prev() is unchanged).
void run_logic_early(VM *vm) {
    VMBreakpoints &bp = vm->bp;
    Eigen::VectorXd &view = vm->api->last_solution;
    std::swap(bp.view_saved, view);
    view = vm->solver->last_solution;
    bp.logic_next.clear();
    bp.collect = true;
    vm->in_probe = true;   // no clock ticks here: they run in the solver's call
    vm_run_digital(vm);
    vm->in_probe = false;
    bp.collect = false;
    std::swap(bp.view_saved, view);
    bp.skip_structural = bp.skip_digital = true;
}

// dt such that t + dt == e in floating point (it nearly always is e - t).
double step_to(double t, double e) {
    double d = e - t;
    for (int k = 0; k < 8 && t + d != e; k++)
        d = (t + d < e) ? std::nextafter(d, INF) : std::nextafter(d, -INF);
    return d;
}

}  // namespace

void VMBreakpointList::add(double e) {
    if (!(e < INF)) return;
    for (int i = 0; i < n; i++) if (t[i] == e) return;
    if (n < BP_LIST) { t[n++] = e; return; }
    int worst = 0;   // full: keep the earliest
    for (int i = 1; i < n; i++) if (t[i] > t[worst]) worst = i;
    if (e < t[worst]) t[worst] = e;
}

double vm_bp_compare(VM *vm, int form, double t, double x, double c, double p) {
    const bool result = pred(form, t, x, c, p);
    VMBreakpoints &bp = vm->bp;
    if (bp.collect) {
        // The logic pass at t_n: the first flip a landed step can be made of.
        bp.logic_next.add(next_flip(form, x, c, p, vm->time + bp.h_min));
    } else if (vm->in_analog && bp.h_min > 0.0) {
        // Analog code, at the solved point t (the left limit on a landing step).
        // (h_min is set by vm_breakpoint_step: only the variable-step solvers
        // use breakpoints, and only from their first step on.)
        const double e = next_flip(form, x, c, p, bp.t0 + bp.h_min);
        if (e <= t && e < bp.analog_cross) bp.analog_cross = e;   // passed inside the step
        bp.analog_next.add(e);
        // ...and the first flip after t, for the step that starts there.
        if (!(e > t + bp.h_min)) bp.analog_next.add(next_flip(form, x, c, p, t + bp.h_min));
    }
    return result ? 1.0 : 0.0;
}

double vm_breakpoint_step(VM *vm, double dt, double max_dt) {
    VMBreakpoints &bp = vm->bp;
    const double t = vm->time;
    bp.skip_structural = bp.skip_digital = false;
    if (t < bp.t0) {   // a library reset rebooted the run: forget the old one
        bp.analog_next.clear();
        bp.land_at = bp.land_from = bp.land_left = std::numeric_limits<double>::quiet_NaN();
    }
    if (t != bp.t0) bp.cuts = 0;
    bp.restart = (t == bp.land_at);   // the step that ended here landed on a breakpoint
    bp.h_min = lte_min_step(max_dt);
    const double t_min = t + bp.h_min;

    // Candidates: what analog code reported during the last attempt, then the
    // logic's comparisons as they stand at t.
    VMBreakpointList cand;
    for (int i = 0; i < bp.analog_next.n; i++)
        if (bp.analog_next.t[i] > t_min) cand.add(bp.analog_next.t[i]);
    bp.t0 = t;
    bp.analog_next.clear();
    bp.analog_cross = INF;
    bp.land_from = bp.land_at = bp.land_left = std::numeric_limits<double>::quiet_NaN();
    if (bp.logic) {
        run_logic_early(vm);
        for (int i = 0; i < bp.logic_next.n; i++)
            if (bp.logic_next.t[i] > t_min) cand.add(bp.logic_next.t[i]);
    }

    const double dt_clock = vm->clocks.empty() ? dt : vm_limit_step(vm, dt);
    vm->tick_changed = bp.restart;   // restart the history as at a tick (clock.cpp keeps it)
    if (cand.n == 0) return dt_clock;

    // The earliest breakpoint, merged with any that follow it within h_min:
    // the step lands on the last of the cluster.
    sort_list(cand);
    const double from = cand.t[0];
    double to = from;
    for (int i = 1; i < cand.n && cand.t[i] <= to + bp.h_min; i++) to = cand.t[i];

    const double rem = from - t;
    if (dt_clock >= rem - bp.h_min) {
        // Reaches it (or would stop within h_min short of it): end exactly on it.
        const double d = step_to(t, to);
        bp.land_from = from;
        bp.land_at = t + d;
        bp.land_left = std::nextafter(bp.land_at, -INF);
        return d;
    }
    if (dt_clock == dt && dt_clock > 0.9 * rem) {
        // Would stop just short, leaving a sliver: take half the way now and
        // the rest next step (ngspice's rule, as vm_limit_step does for ticks).
        return 0.5 * rem;
    }
    return dt_clock;
}

bool vm_breakpoint_cut(VM *vm) {
    VMBreakpoints &bp = vm->bp;
    if (!(bp.analog_cross < INF)) return false;
    if (bp.analog_cross >= bp.land_from) return false;   // part of the cluster this step lands on
    if (bp.cuts >= 8) return false;                       // never loop: accept the step as solved
    bp.cuts++;
    return true;
}
