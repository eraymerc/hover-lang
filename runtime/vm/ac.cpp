// .ac small-signal sweep. Kept in its own translation unit, and called from
// sim.cpp's main rather than from vm_run, on purpose: either placement inside
// vm.cpp changed the compiler's optimisation of the transient code there, and
// decks without .ac (h_bridge, opamp) stopped reproducing their previous
// output bit for bit.
#include "vm.hpp"
#include <cmath>
#include <complex>
#include <cstdio>
#include <string>
#include <vector>

// ─────────────────────────────────────────────────────────────────────────────
// AC SMALL-SIGNAL SWEEP
//
// Linearise at the true DC operating point and solve
//     (G + J + jwC) X = B_ac
// at each frequency. Every piece is the transient's own:
//   G, C  the MNA matrices (C carries capacitors and -L for inductors, so
//         G x + C dx/dt = B is the same equation backward Euler discretises);
//   J     vm_compute_jacobian's -dB/dx. Analog modules only ever write source
//         values into B, never G, so J is the whole nonlinear part: gm, diode
//         conductances, and ddt()-built charges, which reach C through ddt's
//         hidden 1 H inductor and their dq/dv through J;
//   B_ac  the ac= / ac_phase= excitation stamped at build time.
// Time-dependent source values are frozen at their t = 0 value by the DC solve
// and contribute no excitation; the only AC source is ac=.
//
// The operating point is solved WITH the DC relaxation whether or not the deck
// says .op — a small-signal answer about uncharged capacitors means nothing —
// and the VM state is restored afterwards, so a following .tran starts exactly
// as it would have without .ac.
// ─────────────────────────────────────────────────────────────────────────────

static std::vector<double> ac_frequencies(const VM *vm) {
    std::vector<double> f;
    const double f0 = vm->ac_fstart, f1 = vm->ac_fstop;
    const int    n  = vm->ac_points;
    if (vm->ac_sweep == 0) {                      // lin: n points in total
        if (n == 1) { f.push_back(f0); return f; }
        for (int k = 0; k < n; k++) f.push_back(f0 + (f1 - f0) * k / (n - 1));
        return f;
    }
    const double base = (vm->ac_sweep == 1) ? 10.0 : 2.0;   // dec / oct
    const double span = std::log(f1 / f0) / std::log(base);
    const int    last = (int)std::floor(span * n + 1e-9);
    for (int k = 0; k <= last; k++) f.push_back(f0 * std::pow(base, (double)k / n));
    if (f.back() < f1 * (1.0 - 1e-12)) f.push_back(f1);      // always end on fstop
    return f;
}

bool vm_run_ac(VM *vm) {
    System *sys = vm->solver->sys;
    const int n = sys->size;

    printf("[VM] AC: solving DC operating point...\n");
    VMSnapshot snap = vm_save_state(vm);
    // vm_solve_op relaxes to DC only under .op, so request it for this solve.
    // (Going through vm_solve_op rather than a new entry point into the DC
    // solver is deliberate, for the same reason as the note above.)
    const int op_enabled_saved = vm->op_enabled;
    vm->op_enabled = 1;
    vm_solve_op(vm);
    vm->op_enabled = op_enabled_saved;
    const bool op_ok = vm->op_converged != 0;
    if (!op_ok) {
        fprintf(stderr, "[VM] AC: the DC operating point did not converge — "
                        "no AC analysis (it would linearise around a wrong bias).\n");
        vm_restore_state(vm, snap);
        return false;
    }

    Eigen::VectorXd x_op = vm->solver->last_solution;
    const Eigen::MatrixXd J = vm_compute_jacobian(vm, x_op);   // copy: scratch is reused
    const Eigen::MatrixXd GJ = sys->G + J;

    if (sys->B_ac.cwiseAbs().maxCoeff() == 0.0) {
        fprintf(stderr, "[VM] AC: no source has ac= set — every result is 0. "
                        "Excite with e.g. current_source<0, ac=1>() or voltage_source<V, ac=1>().\n");
    }
    if (vm->ac_skipped_signals > 0) {
        printf("[VM] AC: %d saved logic signal(s) have no small-signal value and are "
               "left out of ac_output.csv (MNA nodes and branch currents only).\n",
               vm->ac_skipped_signals);
    }

    // Column rows, resolved once.
    std::vector<int> rows;
    std::vector<std::string> cols;
    for (const auto &name : vm->ac_nodes) {
        rows.push_back(sys->resolve_node(name));        // -1 for gnd -> 0
        cols.push_back(name);
    }
    for (const auto &name : vm->ac_branches) {
        rows.push_back(sys->resolve_branch(name));
        cols.push_back("I(" + name + ")");
    }

    const std::vector<double> freqs = ac_frequencies(vm);
    FILE *out = fopen("ac_output.csv", "w");
    if (!out) {
        fprintf(stderr, "[VM] AC: cannot create ac_output.csv\n");
        vm_restore_state(vm, snap);
        return false;
    }
    fprintf(out, "Freq");
    for (const auto &c : cols) fprintf(out, ",%s.re,%s.im", c.c_str(), c.c_str());
    fprintf(out, "\n");

    Eigen::MatrixXcd A(n, n);
    Eigen::VectorXd  rs(n), cs(n);
    Eigen::PartialPivLU<Eigen::MatrixXcd> lu;
    for (double f : freqs) {
        const double w = 2.0 * M_PI * f;
        A.real() = GJ;
        A.imag() = w * sys->C;

        // Same exact power-of-two equilibration as solver_factorize: the
        // jwC entries span many decades across a sweep, and a GMIN-only row
        // must not vanish beside them.
        for (int i = 0; i < n; i++) {
            double m = A.row(i).cwiseAbs().maxCoeff(); int e = 0;
            rs(i) = (m > 0.0) ? (std::frexp(m, &e), std::ldexp(1.0, -e)) : 1.0;
            A.row(i) *= rs(i);
        }
        for (int j = 0; j < n; j++) {
            double m = A.col(j).cwiseAbs().maxCoeff(); int e = 0;
            cs(j) = (m > 0.0) ? (std::frexp(m, &e), std::ldexp(1.0, -e)) : 1.0;
            A.col(j) *= cs(j);
        }
        lu.compute(A);
        const Eigen::VectorXcd X =
            cs.cast<std::complex<double>>().cwiseProduct(
                lu.solve(rs.cast<std::complex<double>>().cwiseProduct(sys->B_ac)));

        fprintf(out, "%.9e", f);
        for (int r : rows) {
            const std::complex<double> v = (r >= 0) ? X(r) : std::complex<double>(0.0, 0.0);
            fprintf(out, ",%.9e,%.9e", v.real(), v.imag());
        }
        fprintf(out, "\n");
    }
    fclose(out);
    printf("[VM] AC complete: %zu frequencies, %.3e .. %.3e Hz -> ac_output.csv\n",
           freqs.size(), freqs.front(), freqs.back());

    vm_restore_state(vm, snap);
    return true;
}
