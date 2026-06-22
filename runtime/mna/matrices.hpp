#pragma once

#include "system.hpp"

// ─────────────────────────────────────────────────────────────────────────────
// MNA STAMPING FUNCTIONS
//
// Each function accumulates element contributions into sys->G, sys->C,
// or sys->B_static. n1/n2 = -1 means ground (row/col skipped).
//
// Mirrors Go: func (sys *System) StampXxx(...)
// C-style: free functions taking System* as first argument.
// ─────────────────────────────────────────────────────────────────────────────

// R: conductance stamp into G
void stamp_resistor(System *sys, int n1, int n2, double resistance);

// C: capacitance stamp into C matrix
void stamp_capacitor(System *sys, int n1, int n2, double capacitance);

// L: inductor stamp — KCL rows in G, physics (-L) on branch diagonal in C
void stamp_inductor(System *sys, int n1, int n2, double inductance, int branch_idx);

// V: ideal voltage source — KCL rows in G, voltage in B_static
void stamp_voltage_source(System *sys, int n1, int n2, double voltage, int branch_idx);

// I: current source — directly into B_static, records for later update
void stamp_current_source(System *sys, int n1, int n2, double current, const char *name);

// VCCS (G element): I_out = gm * (V_nc1 - V_nc2), pure G stamp
void stamp_vccs(System *sys, int n1, int n2, int nc1, int nc2, double gm);

// VCVS (E element): V_out = k * (V_nc1 - V_nc2), needs branch variable
void stamp_vcvs(System *sys, int n1, int n2, int nc1, int nc2, double k, int branch_idx);

// CCCS (F element): I_out = beta * I_sense, sens_branch_idx must already exist
void stamp_cccs(System *sys, int n1, int n2, int sens_branch_idx, double beta);

// CCVS (H element): V_out = r * I_sense, needs branch variable
void stamp_ccvs(System *sys, int n1, int n2, int sens_branch_idx, double r, int branch_idx);