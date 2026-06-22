#include "matrices.hpp"

// ─────────────────────────────────────────────────────────────────────────────
// INTERNAL HELPER
// Accumulates val into G(i,j). Skips if either index is -1 (ground).
// ─────────────────────────────────────────────────────────────────────────────

static inline void g_add(System *sys, int i, int j, double val) {
    if (i >= 0 && j >= 0) sys->G(i, j) += val;
}

static inline void c_add(System *sys, int i, int j, double val) {
    if (i >= 0 && j >= 0) sys->C(i, j) += val;
}

// ─────────────────────────────────────────────────────────────────────────────
// RESISTOR
// Conductance g = 1/R stamped into G as a 2x2 stamp:
//   G[n1][n1] += g    G[n1][n2] -= g
//   G[n2][n1] -= g    G[n2][n2] += g
// ─────────────────────────────────────────────────────────────────────────────

void stamp_resistor(System *sys, int n1, int n2, double resistance) {
    double g = 1.0 / resistance;
    g_add(sys, n1, n1,  g);
    g_add(sys, n2, n2,  g);
    g_add(sys, n1, n2, -g);
    g_add(sys, n2, n1, -g);
}

// ─────────────────────────────────────────────────────────────────────────────
// CAPACITOR
// Stamped into C (not G). The solver folds alpha*C into G_eff each timestep.
//   C[n1][n1] += cap    C[n1][n2] -= cap
//   C[n2][n1] -= cap    C[n2][n2] += cap
// ─────────────────────────────────────────────────────────────────────────────

void stamp_capacitor(System *sys, int n1, int n2, double capacitance) {
    c_add(sys, n1, n1,  capacitance);
    c_add(sys, n2, n2,  capacitance);
    c_add(sys, n1, n2, -capacitance);
    c_add(sys, n2, n1, -capacitance);
}

// ─────────────────────────────────────────────────────────────────────────────
// INDUCTOR
// Introduces a branch current variable at branch_idx.
// KCL coupling rows go into G; the physics (-L * di/dt) goes into C diagonal.
//
//   G[n1][br] += 1    G[br][n1] += 1
//   G[n2][br] -= 1    G[br][n2] -= 1
//   C[br][br] -= L
// ─────────────────────────────────────────────────────────────────────────────

void stamp_inductor(System *sys, int n1, int n2, double inductance, int branch_idx) {
    int br = branch_idx;
    g_add(sys, n1, br,  1.0);
    g_add(sys, br, n1,  1.0);
    g_add(sys, n2, br, -1.0);
    g_add(sys, br, n2, -1.0);
    c_add(sys, br, br, -inductance);
}

// ─────────────────────────────────────────────────────────────────────────────
// IDEAL VOLTAGE SOURCE
// Introduces a branch current variable at branch_idx.
// Small resistor on branch diagonal (-1e-6) prevents matrix singularity.
//
//   G[n1][br] += 1    G[br][n1] += 1
//   G[n2][br] -= 1    G[br][n2] -= 1
//   G[br][br] -= 1e-6   (regularisation)
//   B_static[br]  = voltage
// ─────────────────────────────────────────────────────────────────────────────

void stamp_voltage_source(System *sys, int n1, int n2, double voltage, int branch_idx) {
    int br = branch_idx;
    g_add(sys, n1, br,  1.0);
    g_add(sys, br, n1,  1.0);
    g_add(sys, n2, br, -1.0);
    g_add(sys, br, n2, -1.0);
    g_add(sys, br, br, -1e-6);
    sys->B_static(br) = voltage;
}

// ─────────────────────────────────────────────────────────────────────────────
// CURRENT SOURCE
// Stamps directly into B_static and registers the record for later updates.
//   B_static[n1] -= current   (current leaves n1)
//   B_static[n2] += current   (current enters n2)
// ─────────────────────────────────────────────────────────────────────────────

void stamp_current_source(System *sys, int n1, int n2, double current, const char *name) {
    if (n1 >= 0) sys->B_static(n1) -= current;
    if (n2 >= 0) sys->B_static(n2) += current;
    sys->register_current_source(name, n1, n2, current);
}

// ─────────────────────────────────────────────────────────────────────────────
// VCCS — Voltage-Controlled Current Source
// I_out = gm * (V_nc1 - V_nc2), current flows into n1 and out of n2.
// Pure G stamp — no new branch variable needed.
//
//   G[n1][nc1] += gm    G[n1][nc2] -= gm
//   G[n2][nc1] -= gm    G[n2][nc2] += gm
// ─────────────────────────────────────────────────────────────────────────────

void stamp_vccs(System *sys, int n1, int n2, int nc1, int nc2, double gm) {
    g_add(sys, n1, nc1,  gm);
    g_add(sys, n1, nc2, -gm);
    g_add(sys, n2, nc1, -gm);
    g_add(sys, n2, nc2,  gm);
}

// ─────────────────────────────────────────────────────────────────────────────
// VCVS — Voltage-Controlled Voltage Source
// V_n1 - V_n2 = k * (V_nc1 - V_nc2).
// Needs one new branch variable (branch_idx) for the output current.
//
//   G[n1][br] += 1     G[br][n1] += 1
//   G[n2][br] -= 1     G[br][n2] -= 1
//   G[br][nc1] -= k    G[br][nc2] += k
//   B_static[br] = 0   (control voltage drives it)
// ─────────────────────────────────────────────────────────────────────────────

void stamp_vcvs(System *sys, int n1, int n2, int nc1, int nc2, double k, int branch_idx) {
    int br = branch_idx;
    g_add(sys, n1,  br,   1.0);
    g_add(sys, br,  n1,   1.0);
    g_add(sys, n2,  br,  -1.0);
    g_add(sys, br,  n2,  -1.0);
    g_add(sys, br,  nc1, -k);
    g_add(sys, br,  nc2,  k);
    // B_static[br] stays 0
}

// ─────────────────────────────────────────────────────────────────────────────
// CCCS — Current-Controlled Current Source
// I_out = beta * I_sense.
// sens_branch_idx must already exist (a V or L stamped before this call).
// No new branch variable — output current is expressed via G coupling.
//
//   G[n1][sens_br] += beta
//   G[n2][sens_br] -= beta
// ─────────────────────────────────────────────────────────────────────────────

void stamp_cccs(System *sys, int n1, int n2, int sens_branch_idx, double beta) {
    g_add(sys, n1, sens_branch_idx,  beta);
    g_add(sys, n2, sens_branch_idx, -beta);
}

// ─────────────────────────────────────────────────────────────────────────────
// CCVS — Current-Controlled Voltage Source
// V_n1 - V_n2 = r * I_sense.
// sens_branch_idx must already exist.
// Needs one new branch variable (branch_idx) for the output current.
//
//   G[n1][br] += 1      G[br][n1] += 1
//   G[n2][br] -= 1      G[br][n2] -= 1
//   G[br][sens_br] -= r
//   B_static[br] = 0
// ─────────────────────────────────────────────────────────────────────────────

void stamp_ccvs(System *sys, int n1, int n2, int sens_branch_idx, double r, int branch_idx) {
    int br = branch_idx;
    g_add(sys, n1, br,            1.0);
    g_add(sys, br, n1,            1.0);
    g_add(sys, n2, br,           -1.0);
    g_add(sys, br, n2,           -1.0);
    g_add(sys, br, sens_branch_idx, -r);
    // B_static[br] stays 0
}