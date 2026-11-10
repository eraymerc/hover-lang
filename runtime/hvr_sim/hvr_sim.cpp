// Implementation of hvr::Sim (hover_sim.hpp): the objects a generated main()
// used to build on its own stack — System, Solver, API, VM, the solver
// strategy — owned here instead, behind the pimpl, so generated code never
// sees their layout or the Eigen and STL types inside them.

#include "hvr_sim_internal.hpp"

#include <cstdio>
#include <cstring>
#include <memory>
#include <string>
#include <vector>

#include "../hover_runtime.h"

namespace hvr {

namespace {
enum PhaseSlot { kStructural, kDigital, kAnalog, kDriveSources, kLog, kSaveState, kRestoreState, kStateInit, kJacobian, kSlots };
enum class Field;
}

struct Sim::Impl {
    System sys;
    Solver solver;
    API api;
    VM vm;
    std::unique_ptr<SolverStrategy> strategy;
    void (*tune)(SolverStrategy *, Field, double) = nullptr;  // set with the strategy
    std::vector<std::string> log_nodes;
    std::vector<std::string> log_signals;
    Phase phases[kSlots] = {};
    std::vector<Phase> clock_phases;  // parallel to vm.clocks
    double tick_dt = 0.0;             // period of the clock whose phase is running, else 0
    std::vector<CurrentSourceRecord *> isrc;  // current_source_handle -> record
};

// The VM calls its phase hooks as void(VM*). Each hook is wired to one
// instantiation of this, which finds the owning Sim through VM::host and
// calls the generated phase with it.
template <int Slot>
void Sim::run_phase(VM *vm) {
    Sim &sim = *static_cast<Sim *>(vm->host);
    sim.impl_->phases[Slot](sim);
}

Sim::Sim() : impl_(new Impl()) { impl_->vm.host = this; }
Sim::~Sim() { delete impl_; }

VM &internal::vm(Sim &sim) { return sim.impl_->vm; }

// ── Netlist ──────────────────────────────────────────────────────────────────

void Sim::init_system(double dt) { system_init_empty(&impl_->sys, dt); }
int Sim::register_node(const char *name) { return impl_->sys.register_node(name); }
int Sim::register_branch(const char *name) { return impl_->sys.register_branch(name); }
void Sim::finalize_system() { system_finalize(&impl_->sys); }
int Sim::resolve_node(const char *name) const { return impl_->sys.resolve_node(name); }
int Sim::resolve_branch(const char *name) const { return impl_->sys.resolve_branch(name); }

void Sim::stamp_resistor(int n1, int n2, double r) { ::stamp_resistor(&impl_->sys, n1, n2, r); }
void Sim::stamp_capacitor(int n1, int n2, double c) { ::stamp_capacitor(&impl_->sys, n1, n2, c); }
void Sim::stamp_inductor(int n1, int n2, double l, int br) { ::stamp_inductor(&impl_->sys, n1, n2, l, br); }
void Sim::stamp_voltage_source(int n1, int n2, double v, int br) { ::stamp_voltage_source(&impl_->sys, n1, n2, v, br); }
void Sim::stamp_current_source(int n1, int n2, double i, const char *name) {
    ::stamp_current_source(&impl_->sys, n1, n2, i, name);
}
void Sim::stamp_ac_voltage(int br, double mag, double ph) { ::stamp_ac_voltage(&impl_->sys, br, mag, ph); }
void Sim::stamp_ac_current(int n1, int n2, double mag, double ph) { ::stamp_ac_current(&impl_->sys, n1, n2, mag, ph); }
void Sim::stamp_vccs(int n1, int n2, int nc1, int nc2, double gm) { ::stamp_vccs(&impl_->sys, n1, n2, nc1, nc2, gm); }
void Sim::stamp_vcvs(int n1, int n2, int nc1, int nc2, double k, int br) {
    ::stamp_vcvs(&impl_->sys, n1, n2, nc1, nc2, k, br);
}
void Sim::stamp_cccs(int n1, int n2, int sens, double beta) { ::stamp_cccs(&impl_->sys, n1, n2, sens, beta); }
void Sim::stamp_ccvs(int n1, int n2, int sens, double r, int br) { ::stamp_ccvs(&impl_->sys, n1, n2, sens, r, br); }

// ── Setup ────────────────────────────────────────────────────────────────────

void Sim::init_vm(double time_step, double end_time, bool zcd, bool op) {
    Impl &m = *impl_;
    solver_init(&m.solver, &m.sys);
    api_init(&m.api, &m.sys, &m.solver);
    m.vm.api = &m.api;
    m.vm.solver = &m.solver;
    m.vm.time = 0.0;
    m.vm.time_step = time_step;
    m.vm.end_time = end_time;
    m.vm.zcd_enabled = zcd ? 1 : 0;
    m.vm.op_enabled = op ? 1 : 0;
    m.vm.jacobian_scratch = Eigen::MatrixXd::Zero(m.sys.size, m.sys.size);
    m.vm.base_b_scratch = Eigen::VectorXd::Zero(m.sys.size);
}

void Sim::set_phases(Phase structural, Phase digital, Phase analog, Phase drive_sources,
                     Phase log, Phase save_state, Phase restore_state) {
    Impl &m = *impl_;
    m.phases[kStructural] = structural;
    m.phases[kDigital] = digital;
    m.phases[kAnalog] = analog;
    m.phases[kDriveSources] = drive_sources;
    m.phases[kLog] = log;
    m.phases[kSaveState] = save_state;
    m.phases[kRestoreState] = restore_state;
    m.vm.phase_structural = &run_phase<kStructural>;
    m.vm.phase_digital = &run_phase<kDigital>;
    m.vm.phase_analog = &run_phase<kAnalog>;
    m.vm.phase_b = &run_phase<kDriveSources>;
    m.vm.phase_log = &run_phase<kLog>;
    m.vm.save_state_vars = &run_phase<kSaveState>;
    m.vm.restore_state_vars = &run_phase<kRestoreState>;
}

void Sim::add_clock(const char *, double period, Phase phase) {
    impl_->clock_phases.push_back(phase);
    VMClock c;
    c.period = period;
    impl_->vm.clocks.push_back(c);
    impl_->vm.run_clock = &Sim::run_clock;
}

// The VM's clock hook: run domain `index`'s phase with dt() reporting its
// period — so `dt` means the sample period in a clocked module and in every
// function it calls.
void Sim::run_clock(VM *vm, int index) {
    Sim &sim = *static_cast<Sim *>(vm->host);
    Impl &m = *sim.impl_;
    const double saved = m.tick_dt;
    m.tick_dt = vm->clocks[index].period;
    m.clock_phases[index](sim);
    m.tick_dt = saved;
}

void Sim::log_node(const char *name) { impl_->log_nodes.emplace_back(name); }
void Sim::log_signal(const char *name) { impl_->log_signals.emplace_back(name); }
void Sim::init_logger() { logger_init(&impl_->vm.logger, impl_->log_nodes, impl_->log_signals); }

void Sim::set_ac(int sweep, int points, double f_start, double f_stop, int skipped_signals) {
    VM &vm = impl_->vm;
    vm.ac_sweep = sweep;
    vm.ac_points = points;
    vm.ac_fstart = f_start;
    vm.ac_fstop = f_stop;
    vm.ac_skipped_signals = skipped_signals;
}
void Sim::add_ac_node(const char *name) { impl_->vm.ac_nodes.emplace_back(name); }
void Sim::add_ac_branch(const char *name) { impl_->vm.ac_branches.emplace_back(name); }

// ── Solver ───────────────────────────────────────────────────────────────────
// Keep in step with knownSolvers / solverFields in compiler/codegen/utils.go:
// codegen validates the name and emits only the tuning fields a solver has.

namespace {
enum class Field { rtol, atol, abstol, max_iter, max_dt };

// Sets one tuning field on a T. A field T does not have is skipped at compile
// time, so every strategy shares one setter signature.
template <class T>
void tune(SolverStrategy *base, Field f, double v) {
    T &s = *static_cast<T *>(base);
    switch (f) {
    case Field::rtol:
        if constexpr (requires(T &x) { x.rtol; }) s.rtol = v;
        break;
    case Field::atol:
        if constexpr (requires(T &x) { x.atol; }) s.atol = v;
        break;
    case Field::abstol:
        if constexpr (requires(T &x) { x.abstol; }) s.abstol = v;
        break;
    case Field::max_iter:
        if constexpr (requires(T &x) { x.max_iter; }) s.max_iter = static_cast<int>(v);
        break;
    case Field::max_dt:
        if constexpr (requires(T &x) { x.max_dt; }) s.max_dt = v;
        break;
    }
}
}  // namespace

bool Sim::set_solver(const char *name) {
    Impl &m = *impl_;
    auto select = [&m]<class T>() {
        m.strategy = std::make_unique<T>();
        m.tune = &tune<T>;
        m.vm.strategy = m.strategy.get();
    };
    if (!std::strcmp(name, "euler_fixed")) select.operator()<EulerFixed>();
    else if (!std::strcmp(name, "euler_adaptive")) select.operator()<EulerAdaptive>();
    else if (!std::strcmp(name, "gauss_siedel")) select.operator()<GaussSiedel>();
    else if (!std::strcmp(name, "trapezoidal")) select.operator()<Trapezoidal>();
    else if (!std::strcmp(name, "trapezoidal_fixed")) select.operator()<TrapezoidalFixed>();
    else if (!std::strcmp(name, "bdf2")) select.operator()<BDF2>();
    else if (!std::strcmp(name, "ndf2")) select.operator()<NDF2>();
    else {
        // Codegen validates .solver names, so this means the compiler and
        // runtime disagree about which solvers exist.
        std::fprintf(stderr, "[Sim] unknown solver \"%s\" — compiler and runtime are out of sync\n", name);
        return false;
    }
    return true;
}

void Sim::set_rtol(double v) { if (impl_->tune) impl_->tune(impl_->strategy.get(), Field::rtol, v); }
void Sim::set_atol(double v) { if (impl_->tune) impl_->tune(impl_->strategy.get(), Field::atol, v); }
void Sim::set_abstol(double v) { if (impl_->tune) impl_->tune(impl_->strategy.get(), Field::abstol, v); }
void Sim::set_max_iter(int v) { if (impl_->tune) impl_->tune(impl_->strategy.get(), Field::max_iter, v); }
void Sim::set_max_dt(double v) { if (impl_->tune) impl_->tune(impl_->strategy.get(), Field::max_dt, v); }

bool Sim::run() { vm_run(&impl_->vm); return !impl_->vm.failed; }
bool Sim::run_ac() { return vm_run_ac(&impl_->vm); }

// ── Inside phases ────────────────────────────────────────────────────────────

double Sim::time() const { return impl_->vm.time; }
double Sim::dt() const { return impl_->tick_dt > 0.0 ? impl_->tick_dt : impl_->vm.time_step; }
double Sim::V(const char *net) const { return api_V(&impl_->api, net); }
double Sim::V_prev(const char *net) const { return api_V_prev(&impl_->api, net); }
double Sim::I(const char *element) const { return api_I(&impl_->api, element); }
double Sim::I_prev(const char *element) const { return api_I_prev(&impl_->api, element); }
void Sim::set_voltage_source(const char *name, double v) { api_set_voltage_source(&impl_->api, name, v); }
void Sim::set_current_source(const char *name, double i) { api_set_current_source(&impl_->api, name, i); }
void Sim::set_value(const char *key, double v) { impl_->vm.values[key] = v; }
void Sim::set_state_var(int k, double v) {
    std::vector<double> &s = impl_->vm.state_vals;
    if (k >= (int)s.size()) s.resize(k + 1, 0.0);
    s[k] = v;
}
double Sim::state_var(int k) const {
    const std::vector<double> &s = impl_->vm.state_vals;
    return (k >= 0 && k < (int)s.size()) ? s[k] : 0.0;
}

int Sim::node_handle(const char *net) const {
    const int idx = impl_->sys.resolve_node(net);
    if (idx == -2) fprintf(stderr, "[MNA API] warning: V(\"%s\") — net not found, reads 0\n", net);
    return idx < 0 ? -1 : idx;
}
int Sim::branch_handle(const char *element) const {
    const int idx = impl_->sys.resolve_branch(element);
    if (idx < 0) fprintf(stderr, "[MNA API] warning: I(\"%s\") — element has no branch current, reads 0\n", element);
    return idx < 0 ? -1 : idx;
}
int Sim::voltage_source_handle(const char *name) const {
    const int idx = impl_->sys.resolve_branch(name);
    if (idx < 0) fprintf(stderr, "[MNA API] error: SetVoltageSource(\"%s\") — element not found\n", name);
    return idx < 0 ? -1 : idx;
}
int Sim::current_source_handle(const char *name) {
    CurrentSourceRecord *rec = impl_->sys.find_current_source(name);
    if (rec == nullptr) {
        fprintf(stderr, "[MNA API] error: SetCurrentSource(\"%s\") — current source not found\n", name);
        return -1;
    }
    // Pointers into the unordered_map stay valid: sources are never erased.
    impl_->isrc.push_back(rec);
    return (int)impl_->isrc.size() - 1;
}
double Sim::V(int node) const { return node < 0 ? 0.0 : impl_->api.last_solution(node); }
double Sim::V_prev(int node) const {
    const Eigen::VectorXd &p = impl_->api.prev_solution;
    return (node < 0 || node >= p.size()) ? 0.0 : p(node);
}
double Sim::I(int branch) const { return branch < 0 ? 0.0 : impl_->api.last_solution(branch); }
double Sim::I_prev(int branch) const {
    const Eigen::VectorXd &p = impl_->api.prev_solution;
    return (branch < 0 || branch >= p.size()) ? 0.0 : p(branch);
}
Dual Sim::V_ad(int node) const {
    return node < 0 ? Dual(0.0) : Dual::var(impl_->api.last_solution(node), node);
}
Dual Sim::I_ad(int branch) const {
    return branch < 0 ? Dual(0.0) : Dual::var(impl_->api.last_solution(branch), branch);
}
// The same formulas as slots_ddt / slots_idt (vm/state_slots.cpp), as values
// and derivatives, without writing s_now/d_now.
Dual Sim::ddt_ad(int k, const Dual &q) const {
    const StateSlots &st = impl_->vm.slots;
    Dual r(st.alpha * (q.v - st.blend[k]) - st.beta * st.d_prev[k]);
    for (int j = 0; j < q.n; j++) { r.i[j] = q.i[j]; r.d[j] = st.alpha * q.d[j]; }
    r.n = q.n;
    return r;
}
Dual Sim::idt_ad(int k, const Dual &x) const {
    const StateSlots &st = impl_->vm.slots;
    Dual r(st.blend[k] + (x.v + st.beta * st.d_prev[k]) * st.inv_alpha);
    for (int j = 0; j < x.n; j++) { r.i[j] = x.i[j]; r.d[j] = st.inv_alpha * x.d[j]; }
    r.n = x.n;
    return r;
}
// The Jacobian is -dB/dx (vm_compute_jacobian's convention). A current source
// stamps B(n1) -= I, B(n2) += I; a driven voltage source B(branch) = v*alpha.
void Sim::ad_current_source(int handle, const Dual &c) {
    if (handle < 0 || c.n == 0) return;
    const CurrentSourceRecord *rec = impl_->isrc[handle];
    Eigen::MatrixXd &J = impl_->vm.jacobian_scratch;
    for (int j = 0; j < c.n; j++) {
        if (rec->n1 >= 0) J(rec->n1, c.i[j]) += c.d[j];
        if (rec->n2 >= 0) J(rec->n2, c.i[j]) -= c.d[j];
    }
}
void Sim::ad_voltage_source(int handle, const Dual &v) {
    if (handle < 0 || v.n == 0) return;
    Eigen::MatrixXd &J = impl_->vm.jacobian_scratch;
    const double a = impl_->sys.source_alpha;
    for (int j = 0; j < v.n; j++) J(handle, v.i[j]) -= a * v.d[j];
}
void Sim::set_jacobian_phase(Phase jacobian) {
    impl_->phases[kJacobian] = jacobian;
    impl_->vm.phase_jacobian = &run_phase<kJacobian>;
}

void Sim::set_voltage_source(int handle, double v) {
    if (handle >= 0) impl_->sys.B_static(handle) = v * impl_->sys.source_alpha;
}
void Sim::set_current_source(int handle, double i) {
    if (handle >= 0) api_set_current_source_rec(&impl_->api, impl_->isrc[handle], i);
}
void Sim::init_state_slots(int n) { slots_resize(impl_->vm.slots, n); }
void Sim::set_state_init(Phase init) {
    impl_->phases[kStateInit] = init;
    impl_->vm.state_init = &run_phase<kStateInit>;
}
double Sim::ddt(int slot, double q) { return slots_ddt(impl_->vm.slots, slot, q); }
double Sim::idt(int slot, double x) { return slots_idt(impl_->vm.slots, slot, x); }
double Sim::value(const char *key) const { return impl_->vm.values[key]; }

}  // namespace hvr
