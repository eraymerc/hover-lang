#pragma once

#include "snapshot.hpp"

// Forward declaration
struct VM;

// ─────────────────────────────────────────────────────────────────────────────
// ZERO CROSSING DETECTION
//
// Mirrors Go vm/zcd.go — three functions for detecting and isolating
// discrete logic discontinuities during simulation.
// ─────────────────────────────────────────────────────────────────────────────

// Returns 1 if any logic variable jumped by more than 1.0 since the snapshot.
// Mirrors Go: func (vm *VM) DetectDiscontinuity(snap VMSnapshot) bool
int vm_detect_discontinuity(const VM *vm, const VMSnapshot &snap);

// Bisects to find the exact crossing time down to 1 ps resolution.
// Returns the reduced dt at which the discontinuity first appears.
// Mirrors Go: func (vm *VM) IsolateZeroCrossing(checkpoint VMSnapshot, currentDt float64) float64
double vm_isolate_zero_crossing(VM *vm, const VMSnapshot &checkpoint, double current_dt);

// Probes one step ahead for discontinuities. If found, bisects and returns
// reduced dt. VM state is fully restored — probe-only operation.
// Updates sys->dt and g_dirty if dt shrinks.
// Mirrors Go: func (vm *VM) CheckZeroCrossing(dt float64) float64
double vm_check_zero_crossing(VM *vm, double dt);