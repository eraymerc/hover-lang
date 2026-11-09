#pragma once
// ─────────────────────────────────────────────────────────────────────────────
// LOCAL TRUNCATION ERROR — the step-size controller of the adaptive solvers
//
// Until 0.9 bdf2, ndf2, trapezoidal and euler_adaptive sized their steps by
// how many Newton iterations the last one took: easy steps grew the step by
// 1.2-2x up to the .tran ceiling. Newton convergence says nothing about
// accuracy — a linear circuit converges in one iteration at any step — so the
// step sat at the ceiling and the ceiling WAS the accuracy setting: the
// rectifier's dc_out moved with it, an inverter deck at 10 us was 22 % off its
// ngspice amplitude, and better conditioning (fewer iterations) made results
// WORSE by growing the step sooner.
//
// Now the step follows the estimated truncation error, as in SPICE (CKTterr)
// and ode15s, and the .tran step is only a ceiling (SPICE's TMAX). The
// estimate is the textbook one:
//
//     LTE ~= C * h^(p+1) * y^(p+1),   y^(p+1) ~= (p+1)! * (divided difference)
//
// with the (p+1)-th divided difference taken over the new point and the last
// accepted ones — exact on the uneven grid an adaptive solver leaves behind.
// C is the method's error constant in the y-normalised form:
//
//     backward Euler 1/2, NDF1 0.315 (1/2 + kappa1), BDF2 2/9, NDF2 1/9,
//     trapezoidal 1/12
//
// (NDF2's constant is half of BDF2's — the factor of two measured between the
// two solvers on an RC charge; see bdf_history.hpp.)
//
// Only STATE is checked: node voltages and branch currents that the C matrix
// touches (capacitors, inductors), and the ddt()/idt() slots (vm/state_slots
// .hpp) — the quantities the integration formula actually integrates. An
// algebraic node can jump at a switching edge without any truncation error,
// and checking it would only shrink the step for nothing (SPICE likewise
// checks charges and fluxes, not every node). Measured: checking every row
// instead stalled examples/Diode/rectifier.hvr for over ten minutes at its
// diode commutations.
//
// Each state component is held to LTE_TRTOL * (rtol * |value| + atol), atol
// being the solver's vntol for nodes and abstol for branch currents; a slot,
// whose expression is a charge in every library model, is held relative to its
// own recent magnitude plus SPICE's CHGTOL (1e-14 C). LTE_TRTOL = 7 is SPICE's default: the estimate is known to
// be pessimistic, and without the factor steps come out several times smaller
// than the error actually requires.
//
// Step control, also SPICE's: the worst ratio r = |LTE| / tolerance over all
// components. r > 1 rejects the step; either way the next step is
// h * 0.9 * r^(-1/(p+1)), clamped to [h/4, 2h] and to the ceiling. When there
// are not yet enough accepted points for an estimate (right after the start or
// a restart), the old iteration-count rule is used for that one step.
// ─────────────────────────────────────────────────────────────────────────────

#include <Eigen/Dense>
#include <algorithm>
#include <cmath>
#include <vector>

#include "bdf_history.hpp"
#include "../vm/state_slots.hpp"
#include "../mna/system.hpp"

constexpr double LTE_TRTOL      = 7.0;
constexpr double LTE_SAFETY     = 0.9;
constexpr double LTE_GROW_MAX   = 2.0;
constexpr double LTE_SHRINK_MIN = 0.25;
// Absolute floor for a slot, SPICE's CHGTOL: a ddt() argument is a charge in
// every standard-library model (the MOSFET's Meyer terms are written as
// ddt(COX*v) for exactly this reason). Without it a junction charge passing
// near zero — a diode switching off — was held to a purely relative
// tolerance of ~1e-3 * nothing: examples/Diode/rectifier.hvr took 22x the
// steps and cmos_inverter.hvr 36x, for no gain.
constexpr double LTE_CHGTOL     = 1e-14;

struct LteMethod {
    int    p;   // order
    double C;   // error constant, y-normalised
};
inline LteMethod lte_be()   { return {1, 0.5}; }
inline LteMethod lte_ndf1() { return {1, 0.5 - 0.1850}; }
inline LteMethod lte_bdf2() { return {2, 2.0 / 9.0}; }
inline LteMethod lte_ndf2() { return {2, 1.0 / 9.0}; }
inline LteMethod lte_trap() { return {2, 1.0 / 12.0}; }

// Rows whose unknown is integrated: any nonzero entry in that row or column of
// C. Computed once per run — C does not change during a transient.
inline std::vector<char> lte_state_rows(const System &sys) {
    std::vector<char> m(sys.size, 0);
    for (int i = 0; i < sys.size; i++)
        for (int j = 0; j < sys.size; j++)
            if (sys.C(i, j) != 0.0) { m[i] = 1; m[j] = 1; }
    return m;
}

// The (m)-th divided difference of the m+1 points (t[k], y[k]).
inline double lte_divided_difference(const double *t, const double *y, int m) {
    double d[4];
    for (int k = 0; k <= m; k++) d[k] = y[k];
    for (int level = 1; level <= m; level++)
        for (int k = 0; k + level <= m; k++)
            d[k] = (d[k] - d[k + 1]) / (t[k] - t[k + level]);
    return d[0];
}

// Everything the estimate needs about one step. The history is the solver's
// (or a dedicated) BdfHistory: y[0] = y_n, y[1] = y_{n-1}, y[2] = y_{n-2},
// h[0] = t_n - t_{n-1}, h[1] = t_{n-1} - t_{n-2}. The slot values follow the
// same points: s_new at the new point, s_hist[0..2] at y[0..2]'s.
struct LteStep {
    const BdfHistory          *hist;
    const Eigen::VectorXd     *x_new;
    const std::vector<char>   *state_rows;
    int                        n_nodes;
    double                     dt;
    double                     rtol, vntol, abstol;
    const std::vector<double> *s_new;           // may be null (no slots)
    const std::vector<double> *s_hist[3];
};

// Worst |LTE| / tolerance over the state components, or -1 when the history is
// too short for a (p+1)-th difference (needs p+1 accepted points).
inline double lte_ratio(const LteStep &st, LteMethod m) {
    const BdfHistory &H = *st.hist;
    if (H.points < m.p + 1) return -1.0;
    const int    npts = m.p + 2;
    double t[4] = {st.dt, 0.0, -H.h[0], -(H.h[0] + H.h[1])};
    double fact = 1.0;
    for (int k = 2; k <= m.p + 1; k++) fact *= k;
    const double coef = m.C * std::pow(st.dt, m.p + 1) * fact;

    double worst = 0.0;
    double y[4];
    const Eigen::VectorXd &xn = *st.x_new;
    for (int i = 0; i < (int)xn.size(); i++) {
        if (!(*st.state_rows)[i]) continue;
        y[0] = xn(i);
        for (int k = 1; k < npts; k++) y[k] = H.y[k - 1](i);
        const double e   = coef * lte_divided_difference(t, y, m.p + 1);
        const double tol = st.rtol * std::max(std::abs(y[0]), std::abs(y[1]))
                         + (i < st.n_nodes ? st.vntol : st.abstol);
        const double r   = std::abs(e) / (LTE_TRTOL * tol);
        if (!(r <= worst)) worst = r;                     // NaN counts as worst
    }
    if (st.s_new) {
        const std::vector<double> &sn = *st.s_new;
        for (size_t k = 0; k < sn.size(); k++) {
            y[0] = sn[k];
            double scale = std::abs(y[0]);
            for (int j = 1; j < npts; j++) {
                y[j] = (*st.s_hist[j - 1])[k];
                scale = std::max(scale, std::abs(y[j]));
            }
            const double e = coef * lte_divided_difference(t, y, m.p + 1);
            const double r = std::abs(e) / (LTE_TRTOL * (st.rtol * scale + LTE_CHGTOL));
            if (!(r <= worst)) worst = r;
        }
    }
    return worst;
}

// The step the error estimate asks for next, as a multiple of dt.
inline double lte_factor(double r, LteMethod m) {
    if (r <= 0.0) return LTE_GROW_MAX;
    const double f = LTE_SAFETY * std::pow(r, -1.0 / (m.p + 1));
    return std::min(LTE_GROW_MAX, std::max(LTE_SHRINK_MIN, f));
}

// Below this, a step is never rejected for truncation error: at a genuine
// discontinuity of a state the estimate does not shrink with h, and refusing
// forever would stall the run. 1e-9 of the ceiling is far below any step the
// controller would otherwise ask for.
inline double lte_min_step(double max_dt) { return 1e-9 * max_dt; }
