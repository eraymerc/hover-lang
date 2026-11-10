#pragma once

// ─────────────────────────────────────────────────────────────────────────────
// HOVER SIMULATION INTERFACE — the only runtime header a generated sim.cpp sees
//
// Generated code talks to the runtime exclusively through hvr::Sim. Its
// implementation (System, Solver, API, VM, the solver strategy — everything
// built on Eigen and the STL) lives behind a pointer in runtime/hvr_sim/hvr_sim.cpp,
// so this header includes nothing but <cmath> and <type_traits>, for the
// hvr::Dual number type below. Two consequences, both deliberate:
//
//   * Compiling a sim.cpp no longer parses Eigen. The runtime's numerical
//     core is compiled once, with the runtime, and never again per program;
//     a generated file is just its own logic plus calls through this class.
//     None of that core was ever inlined into sim.cpp anyway — the hot loop
//     (assembly, LU, Newton) lives in the runtime's .cpp files.
//
//   * The runtime's internal layout is no longer part of the generated
//     code's contract. VM or System can gain, lose or reorder members
//     without invalidating anything codegen emits.
//
// Names passed in (nets, elements, signals) are borrowed for the duration of
// the call only; the runtime copies whatever it keeps.
//
// The --hovercraft library build needs more than this — it drives the VM
// directly — and reaches it through runtime/hvr_sim/hvr_sim_internal.hpp.
// ─────────────────────────────────────────────────────────────────────────────

// ─────────────────────────────────────────────────────────────────────────────
// hvr::Dual — forward-mode automatic differentiation for the analog Jacobian
//
// The compiler emits every analog block a second time with `double` replaced
// by hvr::Dual (compiler/codegen/autodiff.go): V(n) becomes a Dual carrying a unit
// derivative with respect to unknown n, the arithmetic and <math.h> below
// carry derivatives along, and every driven source writes its derivative row
// straight into the Jacobian (hvr::Sim::ad_current_source). One evaluation of
// the analog blocks then gives the exact Jacobian, where the numerical one
// re-ran the whole circuit once per unknown (n+2 times; 28 on
// examples/MOSFET/h_bridge_inverter.hvr) and got a finite-difference estimate.
//
// The gradient is SPARSE: up to K (unknown index, derivative) pairs, sorted by
// index. A device reads a handful of nodes, so a value inside it depends on a
// handful of unknowns; K = 8 leaves room for values that flow between
// modules. A result that would need more sets dual_overflow(), and the
// runtime falls back to the numerical Jacobian for that formation — never a
// silently truncated derivative.
//
// The VALUE part of every operation is computed exactly as the double code
// computes it, so a Dual evaluation follows the same branches as the double
// one at the same point. Where a function has a kink (fabs at 0, fmin/fmax at
// a tie, the clamps the models use), the derivative is the one-sided one of
// the branch taken — a finite difference would have returned a secant there.
// ─────────────────────────────────────────────────────────────────────────────

#include <cmath>
#include <type_traits>

namespace hvr {

struct Dual {
    static constexpr int K = 8;

    double v = 0.0;   // value
    int    n = 0;     // number of gradient entries
    int    i[K];      // unknown indices, ascending
    double d[K];      // d(value)/d(x[i[k]])

    Dual() = default;
    Dual(double x) : v(x) {}   // implicit: constants enter with no gradient
    template <class T, std::enable_if_t<std::is_arithmetic_v<T> && !std::is_same_v<T, double>, int> = 0>
    Dual(T x) : v((double)x) {}

    // Only explicitly: an implicit Dual -> double would let a call silently
    // pick the double overload and drop the derivative.
    template <class T, std::enable_if_t<std::is_arithmetic_v<T>, int> = 0>
    explicit operator T() const { return (T)v; }

    // The unknown x[idx] itself.
    static Dual var(double x, int idx) {
        Dual r(x);
        r.n = 1; r.i[0] = idx; r.d[0] = 1.0;
        return r;
    }
};

// Set when a gradient needed more than Dual::K entries. Cleared and checked
// by the runtime around each AD Jacobian (vm_compute_jacobian).
inline bool &dual_overflow() { static bool f = false; return f; }

namespace detail {
// r.grad = ca*a.grad + cb*b.grad (merge of two sorted lists).
inline void lin(Dual &r, double ca, const Dual &a, double cb, const Dual &b) {
    int p = 0, q = 0, m = 0;
    while (p < a.n || q < b.n) {
        int idx; double val;
        if (q >= b.n || (p < a.n && a.i[p] < b.i[q])) { idx = a.i[p]; val = ca * a.d[p]; p++; }
        else if (p >= a.n || b.i[q] < a.i[p])          { idx = b.i[q]; val = cb * b.d[q]; q++; }
        else { idx = a.i[p]; val = ca * a.d[p] + cb * b.d[q]; p++; q++; }
        if (m < Dual::K) { r.i[m] = idx; r.d[m] = val; m++; }
        else dual_overflow() = true;
    }
    r.n = m;
}
// r.grad = c*a.grad
inline void scale(Dual &r, double c, const Dual &a) {
    r.n = a.n;
    for (int k = 0; k < a.n; k++) { r.i[k] = a.i[k]; r.d[k] = c * a.d[k]; }
}
// f(a) with value fv and derivative df
inline Dual unary(const Dual &a, double fv, double df) {
    Dual r(fv);
    if (a.n) scale(r, df, a);
    return r;
}
// f(a, b) with value fv and partial derivatives fa, fb
inline Dual binary(const Dual &a, const Dual &b, double fv, double fa, double fb) {
    Dual r(fv);
    if (a.n == 0)      { if (b.n) scale(r, fb, b); }
    else if (b.n == 0) { scale(r, fa, a); }
    else               { lin(r, fa, a, fb, b); }
    return r;
}
}  // namespace detail

// ── Arithmetic ───────────────────────────────────────────────────────────────
inline Dual operator+(const Dual &a) { return a; }
inline Dual operator-(const Dual &a) { Dual r(-a.v); detail::scale(r, -1.0, a); return r; }

inline Dual operator+(const Dual &a, const Dual &b) { return detail::binary(a, b, a.v + b.v, 1.0, 1.0); }
inline Dual operator+(const Dual &a, double b) { Dual r = a; r.v = a.v + b; return r; }
inline Dual operator+(double a, const Dual &b) { Dual r = b; r.v = a + b.v; return r; }

inline Dual operator-(const Dual &a, const Dual &b) { return detail::binary(a, b, a.v - b.v, 1.0, -1.0); }
inline Dual operator-(const Dual &a, double b) { Dual r = a; r.v = a.v - b; return r; }
inline Dual operator-(double a, const Dual &b) { Dual r(a - b.v); detail::scale(r, -1.0, b); return r; }

inline Dual operator*(const Dual &a, const Dual &b) { return detail::binary(a, b, a.v * b.v, b.v, a.v); }
inline Dual operator*(const Dual &a, double b) { Dual r(a.v * b); detail::scale(r, b, a); return r; }
inline Dual operator*(double a, const Dual &b) { Dual r(a * b.v); detail::scale(r, a, b); return r; }

inline Dual operator/(const Dual &a, const Dual &b) {
    return detail::binary(a, b, a.v / b.v, 1.0 / b.v, -a.v / (b.v * b.v));
}
inline Dual operator/(const Dual &a, double b) { Dual r(a.v / b); detail::scale(r, 1.0 / b, a); return r; }
inline Dual operator/(double a, const Dual &b) { Dual r(a / b.v); detail::scale(r, -a / (b.v * b.v), b); return r; }

inline Dual &operator+=(Dual &a, const Dual &b) { a = a + b; return a; }
inline Dual &operator-=(Dual &a, const Dual &b) { a = a - b; return a; }
inline Dual &operator*=(Dual &a, const Dual &b) { a = a * b; return a; }
inline Dual &operator/=(Dual &a, const Dual &b) { a = a / b; return a; }

// ── Comparisons: on the value ────────────────────────────────────────────────
#define HVR_DUAL_CMP(op)                                                          \
    inline bool operator op(const Dual &a, const Dual &b) { return a.v op b.v; }  \
    inline bool operator op(const Dual &a, double b) { return a.v op b; }         \
    inline bool operator op(double a, const Dual &b) { return a op b.v; }
HVR_DUAL_CMP(<) HVR_DUAL_CMP(>) HVR_DUAL_CMP(<=) HVR_DUAL_CMP(>=) HVR_DUAL_CMP(==) HVR_DUAL_CMP(!=)
#undef HVR_DUAL_CMP

// ── <math.h> ─────────────────────────────────────────────────────────────────
// One overload per function, on Duals only: a call with a double argument in
// generated AD code converts that argument implicitly (its gradient is
// empty), and a call with only doubles still resolves to <math.h>. The set
// here is exactly the one codegen/autodiff.go accepts; any other extern in an
// analog block makes the compiler leave the AD Jacobian out.
inline Dual exp(const Dual &a)   { const double e = std::exp(a.v); return detail::unary(a, e, e); }
inline Dual exp2(const Dual &a)  { const double e = std::exp2(a.v); return detail::unary(a, e, e * 0.69314718055994531); }
inline Dual expm1(const Dual &a) { return detail::unary(a, std::expm1(a.v), std::exp(a.v)); }
inline Dual log(const Dual &a)   { return detail::unary(a, std::log(a.v), 1.0 / a.v); }
inline Dual log10(const Dual &a) { return detail::unary(a, std::log10(a.v), 1.0 / (a.v * 2.3025850929940457)); }
inline Dual log2(const Dual &a)  { return detail::unary(a, std::log2(a.v), 1.0 / (a.v * 0.69314718055994531)); }
inline Dual log1p(const Dual &a) { return detail::unary(a, std::log1p(a.v), 1.0 / (1.0 + a.v)); }
inline Dual sqrt(const Dual &a)  { const double s = std::sqrt(a.v); return detail::unary(a, s, 0.5 / s); }
inline Dual cbrt(const Dual &a)  { const double c = std::cbrt(a.v); return detail::unary(a, c, 1.0 / (3.0 * c * c)); }
inline Dual fabs(const Dual &a)  { return detail::unary(a, std::fabs(a.v), a.v > 0.0 ? 1.0 : (a.v < 0.0 ? -1.0 : 0.0)); }

inline Dual sin(const Dual &a)   { return detail::unary(a, std::sin(a.v), std::cos(a.v)); }
inline Dual cos(const Dual &a)   { return detail::unary(a, std::cos(a.v), -std::sin(a.v)); }
inline Dual tan(const Dual &a)   { const double c = std::cos(a.v); return detail::unary(a, std::tan(a.v), 1.0 / (c * c)); }
inline Dual asin(const Dual &a)  { return detail::unary(a, std::asin(a.v), 1.0 / std::sqrt(1.0 - a.v * a.v)); }
inline Dual acos(const Dual &a)  { return detail::unary(a, std::acos(a.v), -1.0 / std::sqrt(1.0 - a.v * a.v)); }
inline Dual atan(const Dual &a)  { return detail::unary(a, std::atan(a.v), 1.0 / (1.0 + a.v * a.v)); }
inline Dual sinh(const Dual &a)  { return detail::unary(a, std::sinh(a.v), std::cosh(a.v)); }
inline Dual cosh(const Dual &a)  { return detail::unary(a, std::cosh(a.v), std::sinh(a.v)); }
inline Dual tanh(const Dual &a)  { const double t = std::tanh(a.v); return detail::unary(a, t, 1.0 - t * t); }
inline Dual asinh(const Dual &a) { return detail::unary(a, std::asinh(a.v), 1.0 / std::sqrt(a.v * a.v + 1.0)); }
inline Dual acosh(const Dual &a) { return detail::unary(a, std::acosh(a.v), 1.0 / std::sqrt(a.v * a.v - 1.0)); }
inline Dual atanh(const Dual &a) { return detail::unary(a, std::atanh(a.v), 1.0 / (1.0 - a.v * a.v)); }
inline Dual erf(const Dual &a)   { return detail::unary(a, std::erf(a.v), 1.1283791670955126 * std::exp(-a.v * a.v)); }
inline Dual erfc(const Dual &a)  { return detail::unary(a, std::erfc(a.v), -1.1283791670955126 * std::exp(-a.v * a.v)); }

// Piecewise constant: the value, no gradient.
inline Dual floor(const Dual &a)     { return Dual(std::floor(a.v)); }
inline Dual ceil(const Dual &a)      { return Dual(std::ceil(a.v)); }
inline Dual trunc(const Dual &a)     { return Dual(std::trunc(a.v)); }
inline Dual round(const Dual &a)     { return Dual(std::round(a.v)); }
inline Dual nearbyint(const Dual &a) { return Dual(std::nearbyint(a.v)); }
inline Dual rint(const Dual &a)      { return Dual(std::rint(a.v)); }
inline long lround(const Dual &a)    { return std::lround(a.v); }
inline long lrint(const Dual &a)     { return std::lrint(a.v); }
inline Dual ldexp(const Dual &a, int e) { return detail::unary(a, std::ldexp(a.v, e), std::ldexp(1.0, e)); }

inline Dual pow(const Dual &a, const Dual &b) {
    const double p = std::pow(a.v, b.v);
    // The log term only when the exponent actually varies: a constant
    // exponent on a non-positive base is common (a junction's (1 - v/Vj)^(1-M))
    // and log() of it would poison the derivative with NaN for nothing.
    const double fa = (a.n && b.v != 0.0) ? b.v * std::pow(a.v, b.v - 1.0) : 0.0;
    const double fb = b.n ? p * std::log(a.v) : 0.0;
    return detail::binary(a, b, p, fa, fb);
}
inline Dual atan2(const Dual &y, const Dual &x) {
    const double r2 = x.v * x.v + y.v * y.v;
    return detail::binary(y, x, std::atan2(y.v, x.v), x.v / r2, -y.v / r2);
}
inline Dual hypot(const Dual &a, const Dual &b) {
    const double h = std::hypot(a.v, b.v);
    return detail::binary(a, b, h, a.v / h, b.v / h);
}
inline Dual fmod(const Dual &a, const Dual &b) {
    return detail::binary(a, b, std::fmod(a.v, b.v), 1.0, -std::trunc(a.v / b.v));
}
inline Dual remainder(const Dual &a, const Dual &b) {
    return detail::binary(a, b, std::remainder(a.v, b.v), 1.0, -std::nearbyint(a.v / b.v));
}
inline Dual copysign(const Dual &a, const Dual &b) {
    const double s = (std::signbit(a.v) == std::signbit(b.v)) ? 1.0 : -1.0;
    return detail::unary(a, std::copysign(a.v, b.v), s);
}
inline Dual fdim(const Dual &a, const Dual &b) { return a.v > b.v ? a - b : Dual(std::fdim(a.v, b.v)); }
inline Dual fmax(const Dual &a, const Dual &b) { return (b.v > a.v) ? b : a; }
inline Dual fmin(const Dual &a, const Dual &b) { return (b.v < a.v) ? b : a; }
inline Dual fma(const Dual &a, const Dual &b, const Dual &c) {
    Dual r = a * b + c;
    r.v = std::fma(a.v, b.v, c.v);
    return r;
}

}  // namespace hvr

struct VM;

namespace hvr {

class Sim;

namespace internal {
VM &vm(Sim &sim);  // runtime/hvr_sim/hvr_sim_internal.hpp — library builds only
}

class Sim {
public:
    // A phase (or save/restore hook) is a plain function of the simulation.
    using Phase = void (*)(Sim &sim);

    Sim();
    ~Sim();
    Sim(const Sim &) = delete;
    Sim &operator=(const Sim &) = delete;

    // ── Netlist construction — two passes, as the MNA system requires ──────
    // init_system, then register_* for every terminal and branch, then
    // finalize_system (sizes the matrices), then resolve_* and stamp_*.
    void init_system(double dt);
    int  register_node(const char *name);
    int  register_branch(const char *name);
    void finalize_system();
    int  resolve_node(const char *name) const;
    int  resolve_branch(const char *name) const;

    // Node index -1 is ground.
    void stamp_resistor(int n1, int n2, double resistance);
    void stamp_capacitor(int n1, int n2, double capacitance);
    void stamp_inductor(int n1, int n2, double inductance, int branch);
    void stamp_voltage_source(int n1, int n2, double voltage, int branch);
    void stamp_current_source(int n1, int n2, double current, const char *name);
    void stamp_ac_voltage(int branch, double magnitude, double phase_deg);
    void stamp_ac_current(int n1, int n2, double magnitude, double phase_deg);
    void stamp_vccs(int n1, int n2, int nc1, int nc2, double gm);
    void stamp_vcvs(int n1, int n2, int nc1, int nc2, double gain, int branch);
    void stamp_cccs(int n1, int n2, int sense_branch, double beta);
    void stamp_ccvs(int n1, int n2, int sense_branch, double r, int branch);

    // ── Simulation setup ─────────────────────────────────────────────────────
    // Wires solver, API and VM to the stamped system. Call once, after
    // stamping.
    void init_vm(double time_step, double end_time, bool zcd, bool op);
    void set_phases(Phase structural, Phase digital, Phase analog, Phase drive_sources,
                    Phase log, Phase save_state, Phase restore_state);

    // A clock domain (`.clock(name, period);`) and the phase holding the
    // digital modules bound to it, which then runs once per tick at
    // t = k * period. While it runs, dt() is the period. Call after init_vm.
    void add_clock(const char *name, double period, Phase phase);

    // Number of ddt()/idt() call sites (one slot each). Call before init_vm.
    void init_state_slots(int n);

    // The phase that evaluates the non-constant `state` initialisers; run once,
    // at t = 0 (after .op when there is one). Call after set_phases.
    void set_state_init(Phase init);

    // What the logger records: MNA node voltages, then VM signals (values
    // published with set_value). init_logger fixes the column set.
    void log_node(const char *name);
    void log_signal(const char *name);
    void init_logger();

    // .ac small-signal sweep: sweep 0 = lin, 1 = dec, 2 = oct.
    void set_ac(int sweep, int points, double f_start, double f_stop, int skipped_signals);
    void add_ac_node(const char *name);
    void add_ac_branch(const char *name);

    // Selects the solver by its Hover name (.solver(ndf2) -> "ndf2"). Returns
    // false for a name the runtime does not know.
    bool set_solver(const char *name);
    // Tuning overrides for the selected solver. A field the solver does not
    // have is ignored — codegen only emits the ones that exist.
    void set_rtol(double v);
    void set_atol(double v);
    void set_abstol(double v);
    void set_max_iter(int v);
    void set_max_dt(double v);

    // The transient, start to end_time; writes simulation_output.csv (also when
    // it stops early). False if the solver gave up before end_time.
    bool run();
    bool run_ac();  // the .ac sweep; writes ac_output.csv

    // ── Inside phases ────────────────────────────────────────────────────────
    double time() const;
    double dt() const;  // the clock period inside a clocked phase, else the solver step
    double V(const char *net) const;
    double V_prev(const char *net) const;  // previous Newton iterate
    double I(const char *element) const;
    double I_prev(const char *element) const;
    void   set_voltage_source(const char *name, double voltage);
    void   set_current_source(const char *name, double current);

    // ── Handles: the same reads and writes without a name lookup ─────────────
    // Resolve a name once, after finalize_system(), and pass the handle in the
    // phases. Generated code does exactly this (resolve_handles in sim.cpp):
    // the by-name calls above build a std::string and hash it on EVERY call,
    // and a numerical Jacobian runs the phases n+2 times per formation — on
    // examples/MOSFET/h_bridge_inverter.hvr the lookups were 91 % of the run.
    // A handle of -1 (ground, or a name that does not exist — warned about
    // once, here) reads 0 and ignores writes, as the by-name calls do.
    int    node_handle(const char *net) const;
    int    branch_handle(const char *element) const;       // I(), I_prev()
    int    voltage_source_handle(const char *name) const;
    int    current_source_handle(const char *name);
    double V(int node) const;
    double V_prev(int node) const;
    double I(int branch) const;
    double I_prev(int branch) const;
    void   set_voltage_source(int handle, double voltage);
    void   set_current_source(int handle, double current);

    // ── Automatic differentiation (the AD Jacobian phase) ────────────────────
    // The compiler's second copy of the analog blocks (phase_jacobian) reads
    // the unknowns as Duals and writes each driven source's derivative row
    // into the Jacobian instead of a value into B. Nothing here changes
    // simulation state: ddt_ad/idt_ad return the slot formula's value and
    // derivative without recording anything.
    Dual   V_ad(int node) const;
    Dual   I_ad(int branch) const;
    Dual   ddt_ad(int slot, const Dual &q) const;
    Dual   idt_ad(int slot, const Dual &x) const;
    void   ad_current_source(int handle, const Dual &current);
    void   ad_voltage_source(int handle, const Dual &voltage);
    // Registers the AD phase; without one the Jacobian is numerical.
    void   set_jacobian_phase(Phase jacobian);

    // ddt(q) / idt(x) at call site `slot`, integrated by the active solver's
    // own formula (runtime/vm/state_slots.hpp). Analog phases only.
    double ddt(int slot, double q);
    double idt(int slot, double x);

    // The VM's named-value table: the logged signals.
    void   set_value(const char *key, double v);
    double value(const char *key) const;
    // `state` variables for snapshots (save_state_vars / restore_state_vars),
    // by index. The table grows on demand; an index never written reads 0.
    void   set_state_var(int k, double v);
    double state_var(int k) const;

private:
    struct Impl;
    Impl *impl_;

    friend VM &internal::vm(Sim &sim);
    template <int Slot> static void run_phase(VM *vm);
    static void run_clock(VM *vm, int index);
};

}  // namespace hvr
