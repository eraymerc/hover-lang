#pragma once

#include <Eigen/Dense>
#include <cmath>

// ─────────────────────────────────────────────────────────────────────────────
// NEWTON CORE — the two mechanisms every implicit adaptive solver here needs
//
// This header holds the parts of the Newton loop that are NOT specific to an
// integration formula: how a step decides it has converged, and when a
// Modified-Newton Jacobian has gone stale within a step. Everything that IS
// formula-specific — order selection, history blends, Euler fallbacks, the
// step-size controller — stays in each solver's own run().
//
// All of this was written, measured and debugged inside bdf2.cpp on real decks.
// It lives here so that ndf2, trapezoidal and euler_adaptive get the same
// behaviour rather than each carrying its own older copy: before this header
// existed, all three used a convergence test that reported a NaN row as
// CONVERGED and that collapsed across a zero crossing, and none of them had any
// notion of a Jacobian going stale mid-step. The measurements quoted below are
// from bdf2's runs; the comments are kept with the code they justify.
//
// Not covered here, deliberately: the ACROSS-step Jacobian policy (re-form once
// per accepted step, and blame the Jacobian before blaming dt on a failure).
// That one is a handful of lines of control flow woven through each solver's
// own reject path, and the solvers do not agree on what a rejection means —
// ndf2 keeps its multistep history and its order, trapezoidal drops to Backward
// Euler. It is documented at the accept site in bdf2.cpp and mirrored by hand.
// ─────────────────────────────────────────────────────────────────────────────


// ─────────────────────────────────────────────────────────────────────────────
// CONVERGENCE
// ─────────────────────────────────────────────────────────────────────────────

// ConvergenceReport carries what the last convergence test measured. One field
// is functional and three are diagnostic:
//
//   worst_ratio  the WEIGHTED NORM of the Newton step — the largest per-row
//                |step| expressed in units of that row's own tolerance, so
//                "converged" is exactly worst_ratio <= 1 whatever mix of volts,
//                amps and scales the rows carry. JacRefresh reads it every
//                iteration; it is not diagnostic-only.
//
//   worst_row / worst_step / worst_tol
//                which row blocked convergence and by how much. Read only when
//                HOVER_DUMP_FAIL is set (see fail_dump.hpp). The residual dump
//                can say which EQUATIONS are unsatisfied but not which row
//                failed the convergence TEST, and those are different
//                questions: the test is on the step between iterates, not on
//                the residual.
struct ConvergenceReport {
    double worst_ratio = 0.0;
    int    worst_row   = -1;
    double worst_step  = 0.0;   // |x_new - x_old| at that row
    double worst_tol   = 0.0;   // the tolerance it was compared against
};

// newton_converged answers whether the Newton step from x_old to x_new is small
// enough to stop, and fills `rep` with the measurements above.
//
// n_nodes is the row index where branch currents start: rows below it are node
// voltages and take vntol, the rest are branch currents and take abstol. A
// single absolute tolerance cannot serve both — 1e-6 is a sensible microvolt on
// a node and a wildly loose microamp on a branch — which is SPICE's vntol/abstol
// split and the reason .solver() has two of them.
inline bool newton_converged(const Eigen::VectorXd &x_new,
                             const Eigen::VectorXd &x_old,
                             int n_nodes, double rtol, double vntol,
                             double abstol, ConvergenceReport &rep)
{
    // Scan every row rather than returning on the first failure, so the WORST
    // offender is recorded rather than merely the lowest-numbered one. The extra
    // work is one pass over a vector that was just computed by an LU solve.
    bool ok         = true;
    rep.worst_ratio = 0.0;
    rep.worst_row   = -1;

    for (int i = 0; i < x_new.size(); i++) {
        double abs_tol = (i < n_nodes) ? vntol : abstol;

        // The relative term is measured against the LARGER of the two iterates,
        // not against x_new alone. Using x_new alone is not safe across a zero
        // crossing: a node stepping from 10 V to ~0 V has |x_new| ~ 0, so the
        // scale collapses to abs_tol and a 10 V step is judged against a
        // microvolt. A rectifier is a circuit whose entire behaviour is nodes
        // crossing zero.
        double mag   = std::max(std::abs(x_new(i)), std::abs(x_old(i)));
        double scale = abs_tol + rtol * mag;
        double step  = std::abs(x_new(i) - x_old(i));

        // NEGATED comparison, deliberately — do NOT "simplify" this back to
        // `step > scale`. Every comparison against NaN is false, so `step >
        // scale` REPORTS A NaN ROW AS CONVERGED: a solution vector containing
        // NaN would satisfy every row, be committed to the history, be logged,
        // and advance time, with the FATAL path never firing and the NaN
        // spreading through the rest of the run under the label "converged".
        // Written as !(step <= scale), a NaN fails the row and the step is
        // rejected, which is the only defensible reading of "we do not know
        // that this is converged".
        //
        // Keep the verdict on this DIRECT comparison rather than deriving it
        // from worst_ratio <= 1 below. The two look equivalent and are not: a
        // row that has already blown up to infinity gives step = scale = inf
        // and a ratio of inf/inf = NaN, which would poison worst_ratio and make
        // the step unconvergeable forever. Deriving the verdict from the ratio
        // broke examples/BJT/npn_amp.hvr at t = 0 for exactly that reason.
        if (!(step <= scale)) {
            ok = false;
        }

        // worst_ratio — the step in units of this row's own tolerance — is
        // tracked separately and over EVERY row, converged ones included, so
        // that it is a usable norm of the whole Newton step and not just of its
        // failing part. Same negation, same NaN reason.
        double ratio = step / scale;
        if (!(ratio <= rep.worst_ratio)) {
            rep.worst_ratio = ratio;
            rep.worst_row   = i;
            rep.worst_step  = step;
            rep.worst_tol   = scale;
        }
    }
    return ok;
}


// ─────────────────────────────────────────────────────────────────────────────
// WITHIN-STEP JACOBIAN REFRESH
//
// A per-accepted-step re-form fixes a Jacobian that is stale ACROSS steps. It
// does nothing for one that goes stale WITHIN a step, and that is a separate,
// real failure — Modified Newton holds J fixed at the step's initial guess for
// all max_iter iterations, so a device whose conductance moves by orders of
// magnitude between that guess and the solution is linearized at the wrong
// point for the entire solve.
//
// Measured on examples/Optoelectronics/phototransistor/optocoupler.hvr at
// t = 1.576 ms, the step where the red LED turns off: J was formed with the LED
// conducting 1.63 mA (g = 3.34e-2 S) and the step's own solution has it at
// 7.1e-7 A (g = 1.46e-5 S) — a 2280x change inside one 50 us step. The
// iteration still converges, because the fixed point of
// (G + alpha*C + J)x = B(x) + J*x is the true solution for any invertible J,
// but it converges LINEARLY at a contraction of 0.88: 100 iterations buy six
// orders where three iterations should buy twelve, and the step is abandoned
// still short of tolerance.
//
// Cutting dt cannot help here and measurably does not: the trace shows twenty
// successive halvings from 5e-5 to 1.5e-12, every one at the iteration ceiling
// with the identical 0.88 rate and the identical opening residual. The LED
// branch carries no capacitance, so alpha*C does not reach it and a smaller dt
// moves the target almost not at all. The stiffness is algebraic — it is the
// diode's exponential, not a time constant — and dt is simply the wrong knob.
//
// The trigger is PROGRESS, not rate. A rate monitor read from two consecutive
// iterations was tried first (see the accept site in bdf2.cpp) and bailed out
// during Newton's superlinear warm-up. Measuring over a window of CHECK_PERIOD
// iterations avoids that: a healthy step converges or dies well inside eight
// iterations, whereas a circuit crawling at 0.88 manages 0.88^8 = 0.36 against
// the 100x demanded here and is caught. The window is what makes this different
// from the monitor that did not work.
//
// Bounded to MAX_REFRESH per step because each refresh is a full O(nodes)
// perturbation sweep — the cost is what killed an earlier attempt to re-form on
// trust-region stagnation, which fired on ordinary healthy BJT steps.
// ─────────────────────────────────────────────────────────────────────────────

struct JacRefresh {
    static constexpr int    CHECK_PERIOD    = 8;     // iterations between progress checks
    static constexpr int    MAX_REFRESH     = 6;     // per step, ceiling on extra sweeps
    static constexpr double PROGRESS_FACTOR = 1e-2;  // required drop in the weighted-step
                                                     // norm across one check period

    int    refreshes    = 0;    // re-forms spent this step
    double eta_at_check = 0.0;  // worst_ratio at the last checkpoint

    // Call once at the top of each step attempt.
    void begin_step() { refreshes = 0; eta_at_check = 0.0; }

    // Call after the FIRST iteration's convergence test, to open the window.
    void seed(double worst_ratio) { eta_at_check = worst_ratio; }

    // Call at the top of each iteration; returns true when the caller should
    // re-form the Jacobian at the current iterate. Bookkeeping (the window
    // baseline, the refresh count) is handled here, so the caller's side is a
    // plain if-statement.
    bool due(int iter, double worst_ratio) {
        if (iter == 0 || iter % CHECK_PERIOD != 0) return false;
        if (refreshes >= MAX_REFRESH) return false;   // baseline deliberately not
                                                      // advanced once we stop looking
        bool stalled = !(worst_ratio <= eta_at_check * PROGRESS_FACTOR);
        eta_at_check = worst_ratio;
        if (stalled) {
            refreshes++;
            return true;
        }
        return false;
    }
};
