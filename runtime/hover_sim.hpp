#pragma once

// ─────────────────────────────────────────────────────────────────────────────
// HOVER SIMULATION INTERFACE — the only runtime header a generated sim.cpp sees
//
// Generated code talks to the runtime exclusively through hvr::Sim. Its
// implementation (System, Solver, API, VM, the solver strategy — everything
// built on Eigen and the STL) lives behind a pointer in runtime/hvr_sim/hvr_sim.cpp,
// so this header includes nothing at all. Two consequences, both deliberate:
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

    // ddt(q) / idt(x) at call site `slot`, integrated by the active solver's
    // own formula (runtime/vm/state_slots.hpp). Analog phases only.
    double ddt(int slot, double q);
    double idt(int slot, double x);

    // The VM's named-value table: logged signals and state snapshots.
    void   set_value(const char *key, double v);
    double value(const char *key) const;

private:
    struct Impl;
    Impl *impl_;

    friend VM &internal::vm(Sim &sim);
    template <int Slot> static void run_phase(VM *vm);
    static void run_clock(VM *vm, int index);
};

}  // namespace hvr
