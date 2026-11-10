#pragma once
// ─────────────────────────────────────────────────────────────────────────────
// VARIABLE-STEP BDF / NDF COEFFICIENTS — shared by bdf2 and ndf2
//
// Both solvers integrate with the companion model
//
//     dy/dt(t_{n+1})  ≈  alpha * (y_{n+1} - x_blend)
//
// and both used to compute alpha and x_blend from the CONSTANT-step formulas
// (BDF2: alpha = 1.5/h, x_blend = (4/3)y_n - (1/3)y_{n-1}) while their step
// controllers grow h by 1.2x on every easy step. With a step ratio
// w = h_n / h_{n-1} != 1 those formulas are not merely less accurate, they are
// inconsistent: for y = t they return a slope of 1.5*(1 - 1/(3w)), 1.083 at
// w = 1.2, not 1. Measured on an RC charge (tau = 100 us, .tran ceiling 10 us)
// against the exact exponential, bdf2's error behaved like backward Euler's.
//
// The formulas below are exact on the actual, uneven time grid:
//
//   BDF1 (backward Euler)   alpha = 1/h,  x_blend = y_n
//
//   BDF2, w = h/h1          alpha   = (1 + 2w) / ((1 + w) h)
//                           x_blend = ((1 + w)^2 y_n - w^2 y_{n-1}) / (1 + 2w)
//                           (w = 1: 1.5/h and (4/3)y_n - (1/3)y_{n-1})
//
//   NDF_k (Shampine & Reichelt, "The MATLAB ODE Suite", 1997)
//       BDF_k(y_{n+1}) - kappa_k * gamma_k * (y_{n+1} - y0_{n+1}) / h = f
//   where gamma_1 = 1, gamma_2 = 3/2 and y0 is the order-k PREDICTOR — the
//   polynomial through the last k+1 points, extrapolated to t_{n+1}. On a
//   constant step y_{n+1} - y0 is exactly the backward difference
//   nabla^{k+1} y_{n+1}, which is O(h^{k+1}) and so leaves the order intact.
//
// That last point is what ndf2 used to get wrong: its "order 2" took the
// LINEAR predictor 2y_n - y_{n-1}, making the correction nabla^2 y — O(h^2),
// one order too large. That turns NDF2 into a first-order method with a
// dissipative term, which is the artificial damping recorded in
// docs/hoveR_sorunlar.txt item 12 (a sampled PID loop at its stability limit
// settled under ndf2 while the exact solution and bdf2 kept oscillating).
//
// Order selection is by available history, not by an error estimate:
// a step after a reset (run start, clock tick, zero crossing, rejection) has
// only y_n and runs BDF1; NDF then takes NDF1 with two points, NDF2 with three.
//
// A RESTART cuts the FORMULA's order only; the points stay valid for the
// truncation-error estimate (lte.hpp), which counts them separately (`known`).
// That is SPICE's breakpoint rule (ngspice dctran.c: CKTorder = 1 at a
// breakpoint, while CKTterr keeps differencing CKTstates[] across it): the
// integrated quantities — capacitor and inductor state, ddt()/idt() slots —
// are continuous through a switching event, only their derivatives break, and
// a difference taken across the break is exactly what makes the controller
// shrink the step into the transient. Until 0.9 a restart also blinded the
// estimate (it needs p+1 points), so the step right after every .zcd event or
// clock tick was taken with no error control at all, at the full pre-event
// size: examples/MOSFET/h_bridge_inverter.hvr crossed each ~40 ns gate
// transient in one 150-1000 ns backward-Euler step.
// A step ratio above W_MAX also falls back to BDF1 for that step: variable-step
// BDF2 is zero-stable only for w < 1 + sqrt(2), and a step that follows a
// tiny one (landing on a tick or a zero crossing) can have w in the thousands.
// ─────────────────────────────────────────────────────────────────────────────

#include <Eigen/Dense>
#include <algorithm>

struct BdfHistory {
    // y[0] = y_n, y[1] = y_{n-1}, y[2] = y_{n-2}; h[0] = t_n - t_{n-1},
    // h[1] = t_{n-1} - t_{n-2}. `points` counts how many of y[] are valid
    // since the last restart (1..3) for the FORMULA; `known` counts them since
    // the last reset, for the error estimate only.
    Eigen::VectorXd y[3];
    double h[2] = {0.0, 0.0};
    int points = 1;
    int known  = 1;

    static constexpr double W_MAX = 2.0;

    void reset(const Eigen::VectorXd &y_now) {
        y[0] = y[1] = y[2] = y_now;
        points = 1;
        known  = 1;
    }
    // The next step runs BDF1; the older points remain for the LTE estimate.
    void restart() { points = 1; }

    void accept(const Eigen::VectorXd &y_new, double dt) {
        y[2] = y[1];
        y[1] = y[0];
        y[0] = y_new;
        h[1] = h[0];
        h[0] = dt;
        points = std::min(points + 1, 3);
        known  = std::min(known + 1, 3);
    }

    // The order a step of length dt can run at, given the history and a cap.
    int usable_order(double dt, int max_order) const {
        int order = std::min(points, max_order);
        // Every ratio the formula spans must stay within W_MAX.
        if (order >= 2 && dt / h[0] > W_MAX) order = 1;
        if (order >= 3 && h[0] / h[1] > W_MAX) order = 2;
        return order;
    }

    // A recipe is alpha and the weights w[k] on y[k]: x_blend = sum w[k]*y[k].
    // Computing x_blend from the weights — rather than each formula building
    // its own blend — is what lets the ddt()/idt() slots (vm/state_slots.hpp)
    // apply the very same recipe to their own histories.
    void blend_of(const double w[3], Eigen::VectorXd &x_blend) const {
        x_blend = w[0] * y[0];
        if (w[1] != 0.0) x_blend += w[1] * y[1];
        if (w[2] != 0.0) x_blend += w[2] * y[2];
    }

    // BDF of the given order (1 or 2).
    void bdf_recipe(int order, double dt, double &alpha, double w[3]) const {
        w[0] = 1.0; w[1] = 0.0; w[2] = 0.0;
        if (order <= 1) { alpha = 1.0 / dt; return; }
        const double r = dt / h[0];
        alpha = (1.0 + 2.0 * r) / ((1.0 + r) * dt);
        w[0] = (1.0 + r) * (1.0 + r) / (1.0 + 2.0 * r);
        w[1] = -r * r / (1.0 + 2.0 * r);
    }

    // NDF using `points_used` history points (1: BDF1, 2: NDF1, 3: NDF2),
    // Shampine & Reichelt's kappa.
    void ndf_recipe(int points_used, double dt, double &alpha, double w[3]) const {
        static const double KAPPA[2] = { -0.1850, -1.0 / 9.0 };
        static const double GAMMA[2] = { 1.0, 1.5 };
        const int k = points_used - 1;          // NDF order
        if (k <= 0) { bdf_recipe(1, dt, alpha, w); return; }

        double a_bdf, wb[3];
        bdf_recipe(k, dt, a_bdf, wb);

        // Predictor: polynomial through y[0..k], extrapolated to t_n + dt.
        double L[3] = {0.0, 0.0, 0.0};
        if (k == 1) {
            const double r = dt / h[0];
            L[0] = 1.0 + r;
            L[1] = -r;
        } else {
            const double h1 = h[0], h2 = h[1];
            L[0] = (dt + h1) * (dt + h1 + h2) / (h1 * (h1 + h2));
            L[1] = -dt * (dt + h1 + h2) / (h1 * h2);
            L[2] = dt * (dt + h1) / ((h1 + h2) * h2);
        }

        // alpha*(y - x_blend) = a_bdf*(y - b_bdf) - c*(y - y0),  c = kappa*gamma/h
        const double c = KAPPA[k - 1] * GAMMA[k - 1] / dt;
        alpha = a_bdf - c;
        for (int i = 0; i < 3; i++) w[i] = (a_bdf * wb[i] - c * L[i]) / alpha;
    }
};
