#pragma once
// ─────────────────────────────────────────────────────────────────────────────
// NR LOCALS — nr_prev(x) for an analog VARIABLE x
//
// nr_prev(V(a) - V(b)) reads the previous Newton iterate's NODE voltages
// (api.hpp, prev_solution). nr_prev(x) for a variable x an analog block assigns
// reads x as the evaluation at the previous Newton iterate left it — one slot
// per such variable per module instance, numbered by the compiler
// (codegen/nrlocals.go). Its purpose is SPICE's junction-limiter anchor:
//
//     double vl = pnjlim(V(a) - V(b), nr_prev(vl), nvt, vcrit);
//
// anchors pnjlim on the LIMITED voltage the device used last iteration, as
// DIOload does with *(CKTstate0 + DIOvoltage), where nr_prev(V(a) - V(b))
// anchors it on the node voltages. The difference matters when the iterate
// stops moving while the junction is still far from it: the node anchor then
// sees |dv| < 2 nVt and lets the junction jump the whole way, the device
// anchor keeps walking it.
//
// THE ANALOG BLOCK RUNS MANY TIMES PER ITERATE — residual, AD Jacobian, finite
// differences, trust-region trials, the convergence test, .zcd probes — and
// only one of those evaluations is "the evaluation at the iterate". Which one
// is told from the read-view, not from the caller, so no solver needed a new
// call. api_update_solution INSTALLS an iterate (api->serial, inst_last,
// inst_prev); each evaluation (nr_begin, emitted at the top of phase_analog)
// compares its view (last_solution, prev_solution) with that:
//
//   installed view    (last, prev) == (inst_last, inst_prev): the iterate's
//                     own evaluation. Reads A, the committed values; what it
//                     writes is kept as L when the next evaluation begins.
//   released view     last == prev, not installed (vm_noncon_ratio's "with
//                     every limiter released"): the previous iterate IS this
//                     point, so nr_prev(x) is x from the evaluation just before
//                     it, at the same point (P). For nr_prev(V) this view is
//                     the identity; for a limiter anchored on its own output it
//                     is one more application of the limiter at this point —
//                     idle iff the limiter was already idle, which is the
//                     question the convergence test asks.
//   prospective view  prev == inst_last: the point the next iteration would
//                     evaluate (vm_noncon_ratio's "as the NEXT Newton iteration
//                     would see them"). Reads L, what the next install commits.
//   any other view    a probe (finite differences, trust-region trial, LTE,
//                     .zcd): reads A, writes nothing that is kept.
//
// COMMIT: when nr_begin sees a new api->serial it sets A = L — once per
// installed iterate, however many evaluations it took. Two installs with no
// evaluation between commit the same L twice, which is a no-op.
// ROLLBACK: the whole struct is part of VMSnapshot, so a rejected step, an LTE
// rejection or a .zcd probe puts it back, and the retry starts exactly as the
// first attempt did.
// SEED: a time step's first install commits the L of the previous accepted
// step's last iterate — SPICE's state1. The very first evaluation of a run
// reads 0, as nr_prev(V(...)) does (the zero solution).
// AD: the Jacobian phase reads the values the evaluation before it chose
// (always the baseline at the same view — vm_compute_jacobian) as constants,
// and records nothing.
// ─────────────────────────────────────────────────────────────────────────────

#include <vector>

struct API;

struct NrLocals {
    std::vector<double> A;     // committed: the installed iterate's nr_prev(x)
    std::vector<double> L;     // written by the last installed-view evaluation
    std::vector<double> W;     // written by the current (last) evaluation
    std::vector<double> P;     // written by the evaluation before the current one
    std::vector<double> cur;   // what nr_prev(x) reads in the current evaluation
    long seen_serial   = -1;   // api->serial at the last commit
    bool cur_installed = false;  // the current (last) evaluation is at the installed view

    int size() const { return (int)A.size(); }
};

void nr_resize(NrLocals &nr, int n);
// Called at the top of every phase_analog evaluation (generated code).
void nr_begin(NrLocals &nr, const API &api);
