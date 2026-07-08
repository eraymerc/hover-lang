// ── HOVERCRAFT INTERNAL RUNTIME GLUE ────────────────────────────────────────
// Declarations shared between generated sim.cpp (--hovercraft library mode)
// and runtime/vm/hvr_runtime.cpp. Deliberately separate from hover_runtime.h
// (the header the standalone build has always used) so nothing about the
// existing standalone path changes by anything in this file being added.
//
// Includes hover_runtime.h itself so this header is self-contained
// regardless of include order at the call site.
// ─────────────────────────────────────────────────────────────────────────────
#pragma once

#include "hover_runtime.h"
#include "hovercraft.h"

// vm_boot / vm_run_until — the incremental split of the previously
// monolithic vm_run (runtime/vm/vm.cpp). vm_run itself now just calls them
// back-to-back, unchanged in behavior; the hovercraft library API drives
// them directly to implement HVR_step/HVR_run.
void vm_boot(VM *vm);
void vm_run_until(VM *vm, double target_time);

// Generic log-retrieval / lifecycle helpers, implemented in
// runtime/vm/hvr_runtime.cpp against Logger's public fields. Kept as free
// functions rather than added to the Logger/VM structs themselves, so none
// of the existing runtime headers need to change for this feature.
void         hvr_rt_mark_batch(VM *vm);
void         hvr_rt_reset_log(VM *vm);
int          hvr_rt_save_log_csv(VM *vm, const char *filename);
HVRLogResult hvr_rt_query_all(VM *vm);
HVRLogResult hvr_rt_query_range(VM *vm, double t0, double t1);
HVRLogResult hvr_rt_query_latest(VM *vm);
HVRLogResult hvr_rt_query_last_step(VM *vm);
void         hvr_rt_clear_before(VM *vm, double t);
void         hvr_rt_free_result(HVRLogResult *r);
