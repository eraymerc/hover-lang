#include "nr_locals.hpp"
#include "../mna/api.hpp"

#include <cstring>

void nr_resize(NrLocals &nr, int n) {
    for (auto *v : {&nr.A, &nr.L, &nr.W, &nr.P, &nr.cur}) v->assign(n, 0.0);
    nr.seen_serial = -1;
    nr.cur_installed = false;
}

// Bit-for-bit: a view is the installed one only if it IS that vector, not a
// point near it.
static bool same(const Eigen::VectorXd &a, const Eigen::VectorXd &b) {
    return a.size() == b.size() &&
           std::memcmp(a.data(), b.data(), sizeof(double) * (size_t)a.size()) == 0;
}

void nr_begin(NrLocals &nr, const API &api) {
    if (nr.A.empty()) return;

    // What the evaluation that just ended wrote.
    if (nr.cur_installed) nr.L = nr.W;
    nr.P = nr.W;

    // A new iterate was installed since the last commit.
    if (api.serial != nr.seen_serial) {
        nr.A = nr.L;
        nr.seen_serial = api.serial;
    }

    const Eigen::VectorXd &last = api.last_solution;
    const Eigen::VectorXd &prev = api.prev_solution;
    nr.cur_installed = same(last, api.inst_last) && same(prev, api.inst_prev);
    if (nr.cur_installed)              nr.cur = nr.A;   // the iterate's own evaluation
    else if (same(last, prev))         nr.cur = nr.P;   // released: the previous iterate is this point
    else if (same(prev, api.inst_last)) nr.cur = nr.L;  // prospective: the next iterate's view
    else                               nr.cur = nr.A;   // a probe
}
