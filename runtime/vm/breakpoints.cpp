// Breakpoints: landing steps on the times the deck's code compares `time`
// against. The why and the rules are in breakpoints.hpp.

#include "breakpoints.hpp"
#include "vm.hpp"
#include "snapshot.hpp"
#include "zcd.hpp"
#include "../solvers/lte.hpp"

#include <cmath>
#include <cstdio>

namespace {

constexpr double INF = std::numeric_limits<double>::infinity();

// ── The comparison, exactly as the generated code would have computed it ───
//
// form = op | U << 2 | O << 4 (codegen/breakpoints.go):
//   op  0 S > x    1 S >= x    2 S < x    3 S <= x
//   U   0 u = t    1 u = t + c 2 u = t - c
//       3 u = t ± c[0] ± c[1] ...  a chain (Sim::tcmp_chain), one term at a time
//   O   0 S = u    1 S = fmod(u, p)    2 S = (p == 0 ? 0 : fmod(u, p))  (Hover's %)
//       3 S = u - floor(p == 0 ? 0 : u / p) * p                         (Hover's guarded /)
// Every operation is the one the original expression performs, in the same
// order, so the result is bit-identical to the code it replaces (IEEE, no
// contraction: -ffp-contract=off). `c + time` is t + c: addition commutes. A
// chain is applied term by term — (t - a) - b is not t - (a + b) to the ulp.

// The offsets of u, in order: U = 1 and 2 are a chain of one.
struct Chain {
    int      n   = 0;
    unsigned neg = 0;   // bit i: term i is subtracted
    double   c[BP_CHAIN] = {};
    double   scale = 0.0;   // sum of |c[i]|: bounds every partial result's offset
};

inline Chain chain_of(int form, double c) {
    Chain ch;
    const int U = (form >> 2) & 3;
    if (U == 1 || U == 2) {
        ch.n = 1;
        ch.neg = U == 2 ? 1u : 0u;
        ch.c[0] = c;
    }
    ch.scale = std::fabs(c);   // c is 0.0 for U = 0, as passed
    return ch;
}

inline double u_of(const Chain &ch, double t) {
    double u = t;
    for (int i = 0; i < ch.n; i++) u = (ch.neg >> i & 1u) ? u - ch.c[i] : u + ch.c[i];
    return u;
}

// The time at which u takes the value u (approximately — the search refines it).
inline double t_of(const Chain &ch, double u) {
    for (int i = ch.n - 1; i >= 0; i--) u = (ch.neg >> i & 1u) ? u + ch.c[i] : u - ch.c[i];
    return u;
}

inline double s_of(int form, const Chain &ch, double t, double p) {
    const double u = u_of(ch, t);
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

inline bool pred(int form, const Chain &c, double t, double x, double p) {
    return holds(form, s_of(form, c, t, p), x);
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
double flip_near(int form, double x, const Chain &c, double p, double t_ref, double E0, double scale) {
    if (!std::isfinite(E0)) return INF;
    const double h = 64.0 * ulp_of(std::fmax(std::fabs(E0), scale));
    double lo = E0 - h, hi = E0 + h;
    if (!(hi > t_ref)) return INF;
    if (lo < t_ref) lo = t_ref;
    const bool a = pred(form, c, lo, x, p);
    if (pred(form, c, hi, x, p) == a) return INF;
    // Bisection down to neighbouring doubles: pred(lo) == a != pred(hi).
    // (u is non-decreasing in t: every term is one rounded addition.)
    for (int k = 0; k < 2100; k++) {
        const double mid = lo + 0.5 * (hi - lo);
        if (!(mid > lo && mid < hi)) break;
        if (pred(form, c, mid, x, p) == a) lo = mid; else hi = mid;
    }
    return hi;
}

// The first time after t_ref at which the comparison's result changes, or INF.
double next_flip(int form, double x, const Chain &c, double p, double t_ref) {
    if (std::isnan(x)) return INF;
    const int O = (form >> 4) & 3;
    if (O == 0 || (O == 3 && p == 0.0)) {
        // S is non-decreasing in t: > and >= turn true once, < and <= false
        // once. Already turned: nothing ahead.
        const bool late = (form & 3) < 2;
        if (pred(form, c, t_ref, x, p) == late) return INF;
        return flip_near(form, x, c, p, t_ref, t_of(c, x), std::fmax(c.scale, std::fabs(x)));
    }
    if (!(p != 0.0) || !std::isfinite(p)) return INF;   // fmod(u, 0) is NaN, u % 0 is 0: constant
    // Periodic: S rises with u between wraps at u = m P, and meets the level x
    // at most once per period, at u = m P + r (r = x reduced into [0, P)).
    // Those are the only places it can flip; try the next ones after t_ref.
    const double P = std::fabs(p);
    const double r = x - std::floor(x / P) * P;
    const double m0 = std::floor(u_of(c, t_ref) / P);
    double best = INF;
    for (int k = 0; k <= 2; k++) {
        for (int lvl = 0; lvl < 2; lvl++) {
            const double u = (m0 + k) * P + (lvl ? r : 0.0);
            const double e = flip_near(form, x, c, p, t_ref, t_of(c, u),
                                       std::fmax(std::fabs(u), c.scale));
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

// What the logic drives into the circuit as it stands: the sources it drives
// (vm_drive_vector) and the logic values analog code reads (phase_zcd_watch)
// — the same vector a .zcd probe compares, and clock.cpp's test of whether a
// tick changed anything. Restores everything it touches.
Eigen::VectorXd circuit_drive(VM *vm) {
    Eigen::VectorXd drive = vm_drive_vector(vm);
    if (vm->phase_zcd_watch) {
        vm->phase_zcd_watch(vm);
        const Eigen::Index n = drive.size();
        drive.conservativeResize(n + (Eigen::Index)vm->zcd_watch.size());
        for (size_t i = 0; i < vm->zcd_watch.size(); i++) drive(n + (Eigen::Index)i) = vm->zcd_watch[i];
    }
    return drive;
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

namespace {

// A periodic comparison whose period is not above h_min (breakpoints.hpp,
// PERIOD BELOW h_min): it would pin every step to h_min. True, after warning
// once for its site, when its flips are not to be reported.
bool unresolvable(VM *vm, int form, double x, double p) {
    VMBreakpoints &bp = vm->bp;
    const int O = (form >> 4) & 3;
    if (O == 0 || !(std::fabs(p) <= bp.h_min) || p == 0.0) return false;
    const size_t site = (size_t)(form >> 6);   // 0: comparisons in function bodies, one warning for all
    if (site >= bp.sites.size()) bp.sites.resize(site + 1);
    if (!bp.sites[site].warned) {
        bp.sites[site].warned = true;
        std::fprintf(stderr,
                     "[VM] WARNING: a periodic `time` comparison (period %.3e s, threshold %.3e) "
                     "repeats faster than the shortest step a breakpoint can be landed on "
                     "(%.3e s = 1e-9 x the .tran maximum step); its flips are not landed and it "
                     "is only sampled where the solver steps anyway. Lower .tran's maximum step "
                     "below %.3e s to resolve it.\n",
                     std::fabs(p), x, bp.h_min, std::fabs(p) / 1e-9);
    }
    return true;
}

double compare(VM *vm, int form, double t, double x, const Chain &c, double p) {
    const bool result = pred(form, c, t, x, p);
    VMBreakpoints &bp = vm->bp;
    if ((bp.collect || (vm->in_analog && bp.h_min > 0.0)) && unresolvable(vm, form, x, p))
        return result ? 1.0 : 0.0;
    if (bp.collect) {
        // The logic pass at t_n: the first flip a landed step can be made of —
        // unless the threshold is tracking time (breakpoints.hpp).
        const int site = form >> 6;
        if (site > 0) {
            if ((size_t)site >= bp.sites.size()) bp.sites.resize((size_t)site + 1);
            VMBreakpointSite &s = bp.sites[site];
            if (!(vm->time == s.t)) {   // a new step start (a retried attempt re-evaluates the same one)
                const bool first = std::isnan(s.t) || vm->time < s.t;   // first use, or a rebooted run
                s.moves = (!first && x != s.x) ? s.moves + 1 : 0;
                s.t = vm->time;
                s.x = x;
            }
            if (s.moves >= 2) return result ? 1.0 : 0.0;
        }
        // A flip less than h_min ahead cannot be landed exactly (PROGRESS), but
        // it must not be dropped either: the value sampled now would then be
        // held for a whole free step (a 0.5 ns pulse lasted 3.6 ms). The step is
        // ended h_min from here instead, and the next sample sees the new value.
        const double t_min = vm->time + bp.h_min;
        if (next_flip(form, x, c, p, vm->time) <= t_min) bp.logic_next.add(std::nextafter(t_min, INF));
        bp.logic_next.add(next_flip(form, x, c, p, t_min));
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

}  // namespace

double vm_bp_compare(VM *vm, int form, double t, double x, double c, double p) {
    return compare(vm, form, t, x, chain_of(form, c), p);
}

double vm_bp_compare_chain(VM *vm, int form, double t, double x, double p,
                           unsigned neg, const double *c, int n) {
    Chain ch;
    ch.n = n < BP_CHAIN ? n : BP_CHAIN;   // codegen emits at most BP_CHAIN terms
    ch.neg = neg;
    for (int i = 0; i < ch.n; i++) {
        ch.c[i] = c[i];
        ch.scale += std::fabs(c[i]);
    }
    return compare(vm, form, t, x, ch, p);
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
    // The step that ended here landed on a breakpoint. Whether that restarts
    // the history is decided below, at the step's first attempt; a retried
    // attempt from the same t is still that step: land_at was cleared by the
    // first attempt, so the decision is carried (without it the retry lost the
    // short first step).
    const bool landed = (t == bp.land_at);
    const bool landed_analog = landed && bp.land_analog;
    if (!landed && !(t == bp.t0 && bp.restart)) bp.restart = false;
    bp.h_min = lte_min_step(max_dt);
    const double t_min = t + bp.h_min;

    // Candidates: what analog code reported during the last attempt, then the
    // logic's comparisons as they stand at t.
    VMBreakpointList cand, from_analog;
    for (int i = 0; i < bp.analog_next.n; i++)
        if (bp.analog_next.t[i] > t_min) {
            cand.add(bp.analog_next.t[i]);
            from_analog.add(bp.analog_next.t[i]);
        }
    bp.t0 = t;
    bp.analog_next.clear();
    bp.analog_cross = INF;
    bp.land_from = bp.land_at = bp.land_left = std::numeric_limits<double>::quiet_NaN();
    bp.land_analog = false;
    // RESTART ONLY ON A CHANGE. A flip of analog code changed the analog
    // source itself: restart. A flip of logic restarts only if the logic pass
    // here changed what it drives into the circuit — the drive vector and the
    // logic values analog code reads — exactly as a clock tick (clock.cpp).
    // A flag that is only saved, or a branch that writes the same value,
    // changes nothing the solution depends on.
    const bool decide = landed && !landed_analog && bp.logic;
    Eigen::VectorXd drive_before;
    if (decide) drive_before = circuit_drive(vm);
    if (bp.logic) {
        run_logic_early(vm);
        for (int i = 0; i < bp.logic_next.n; i++)
            if (bp.logic_next.t[i] > t_min) cand.add(bp.logic_next.t[i]);
    }
    if (landed) {
        if (decide) {
            const Eigen::VectorXd drive_after = circuit_drive(vm);
            bp.restart = !(drive_before.size() == drive_after.size() &&
                           (drive_before.array() == drive_after.array()).all());
        } else {
            bp.restart = true;
        }
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
        for (int i = 0; i < from_analog.n; i++)
            if (from_analog.t[i] >= from && from_analog.t[i] <= to) bp.land_analog = true;
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
