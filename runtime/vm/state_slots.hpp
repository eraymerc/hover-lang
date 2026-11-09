#pragma once
// ─────────────────────────────────────────────────────────────────────────────
// STATE SLOTS — ddt() and idt() as integrated state, not as circuit elements
//
// Until 0.9 the elaborator rewrote ddt(q) into a hidden node holding a 1 H
// inductor fed by a current source of value q, and idt(x) into a hidden node
// holding a 1 F capacitor fed by x. The solver then integrated those elements
// like any other, which delegated the derivative to the active integration
// formula — the right idea — but made q (or the integral) an UNKNOWN of the
// MNA system. For a junction charge that unknown lives at ~1e-10 C, its row is
// scaled by alpha = 1/dt (2^67..2^96 in failure dumps), and the current read
// back is alpha*(q - q_hist): the difference of two nearly equal charges
// multiplied by 1/dt. As dt shrank that difference drowned in round-off, so a
// rejected step that halved dt made convergence harder, not easier — the
// signature of examples/MOSFET/h_bridge_inverter.hvr (FATAL at 1.37 ms with a
// 1 us ceiling, at 23 ps with 500 ns).
//
// This is how SPICE, and Verilog-A simulators for the contribution form
// I(a,b) <+ ddt(q), do it instead: the charge is device STATE, kept outside the
// matrix. Each ddt()/idt() call site gets a slot holding the value it saw at the
// last few accepted time points; the active solver publishes, once per attempt,
// the same companion recipe it applies to the C matrix:
//
//     derivative(s) = alpha * (s - blend) - beta * derivative_prev
//     blend         = w0*s_n + w1*s_{n-1} + w2*s_{n-2}
//
//   backward Euler  alpha = 1/h,  w = {1},          beta = 0
//   trapezoidal     alpha = 2/h,  w = {1},          beta = 1
//   BDF2 / NDF      alpha, w from bdf_history.hpp,  beta = 0
//
// ddt(q) returns that derivative of q. idt(x) solves the same relation for s
// with the derivative equal to x. Both are still implicit: they are evaluated
// at the new time point inside the Newton loop, and because analog Jacobians
// are taken by perturbation, alpha*dq/dV reaches the Jacobian on its own —
// SPICE's geq, without stamping it by hand. Nothing is added to the matrix.
//
// What is committed to history is the evaluation AT THE ACCEPTED SOLUTION:
// Newton solvers re-run the analog pass there first (vm_commit_slots_at). The
// last in-loop evaluation is from the iterate before the final solve, and was
// measured to matter — at the nanosecond steps after a switching tick, its
// tolerance-sized offset times a BJT's diffusion capacitance over dt is an
// ampere-scale error in the next charge current (examples/DCMotor/h_bridge.hvr
// drifted 4 % off its fine-step answer). The hidden-element scheme never had
// this problem: q was an unknown, so it was always the accepted q.
// Solvers without Newton (euler_fixed, gauss_siedel) commit their single
// evaluation, which reproduces the hidden element's one-step lag exactly.
// ─────────────────────────────────────────────────────────────────────────────

#include <vector>

struct StateSlots {
    // Per slot. s = the integrated quantity (q for ddt, the output for idt);
    // d = its time derivative (the output for ddt, x for idt).
    std::vector<double> s_now, d_now;     // from the most recent evaluation
    std::vector<double> h0, h1, h2;       // s at the last three accepted points
    std::vector<double> d_prev;           // d at the last accepted point
    std::vector<double> blend;            // recipe applied to h, per slot

    // The recipe in force. inv_alpha is kept separately so .ac can drive the
    // two operators independently (see vm/ac.cpp).
    double alpha = 0.0, inv_alpha = 0.0, beta = 0.0;

    int size() const { return (int)s_now.size(); }
};

void slots_resize(StateSlots &st, int n);
// Everything to zero: an uncharged circuit at rest (vm_boot).
void slots_reset(StateSlots &st);

// Publish a recipe: blend = w0*h0 + w1*h1 + w2*h2.
void slots_begin(StateSlots &st, double alpha, const double w[3], double beta);
// Backward Euler from the last accepted point: w = {1, 0, 0}, beta = 0.
void slots_begin_be(StateSlots &st, double alpha);
// Backward Euler from an all-zero history (the bias point without .op).
void slots_begin_zero(StateSlots &st, double alpha);

double slots_ddt(StateSlots &st, int k, double q);
double slots_idt(StateSlots &st, int k, double x);

// Accept the last evaluation as the new time point: shift the history.
void slots_commit(StateSlots &st);
// Make the last evaluation the whole history (start of a transient, or a
// converged rung of the .op relaxation).
void slots_anchor(StateSlots &st);
