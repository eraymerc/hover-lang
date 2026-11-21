#include "logger.hpp"
#include "vm.hpp"

#include <cstdio>
#include <cstring>

// ─────────────────────────────────────────────────────────────────────────────
// INIT
// ─────────────────────────────────────────────────────────────────────────────

void logger_init(Logger *log,
                 const std::vector<std::string> &nodes_to_save,
                 const std::vector<std::string> &extra_signals)
{
    log->nodes_to_save = nodes_to_save;
    log->extra_signals = extra_signals;

    log->results.clear();
    for (const auto &n : nodes_to_save) log->results[n] = {};
    for (const auto &s : extra_signals)  log->results[s] = {};
}

// ─────────────────────────────────────────────────────────────────────────────
// LOG STEP — MNA node voltages
// Mirrors Go:
//   l.TimeSteps = append(l.TimeSteps, t)
//   for _, node := range l.NodesToSave {
//       if idx, exists := sys.NodeMap[node]; exists {
//           l.Results[node] = append(l.Results[node], solution[idx])
//       }
//   }
// ─────────────────────────────────────────────────────────────────────────────

void logger_log_step(Logger *log, double t,
                     const System *sys,
                     const Eigen::VectorXd &solution)
{
    log->time_steps.push_back(t);

    for (const auto &node : log->nodes_to_save) {
        if (node == "gnd" || node == "0") {
            log->results[node].push_back(0.0);
            continue;
        }
        int idx = sys->resolve_node(node);
        if (idx >= 0) {
            log->results[node].push_back(solution(idx));
        } else {
            log->results[node].push_back(0.0);
        }
    }
}

// ─────────────────────────────────────────────────────────────────────────────
// LOG SIGNALS — VM runtime values
// Mirrors Go:
//   for _, sig := range l.ExtraSignals {
//       if val, ok := values[sig]; ok { l.Results[sig] = append(..., val) }
//       else { l.Results[sig] = append(..., 0.0) }
//   }
// ─────────────────────────────────────────────────────────────────────────────

void logger_log_signals(Logger *log,
                        const std::unordered_map<std::string, double> &values)
{
    for (const auto &sig : log->extra_signals) {
        auto it = values.find(sig);
        double val = (it != values.end()) ? it->second : 0.0;
        log->results[sig].push_back(val);
    }
}

// ─────────────────────────────────────────────────────────────────────────────
// EXPORT CSV
// Mirrors Go: func (l *Logger) ExportCSV(filename string)
// Columns: Time, [mna nodes...], [vm signals...]
// ─────────────────────────────────────────────────────────────────────────────

void logger_export_csv(const Logger *log, const char *filename) {
    FILE *f = fopen(filename, "w");
    if (!f) {
        fprintf(stderr, "[Logger] Failed to create CSV: %s\n", filename);
        return;
    }

    // Build ordered column list: MNA nodes first, then VM signals
    std::vector<std::string> all_cols;
    all_cols.insert(all_cols.end(), log->nodes_to_save.begin(), log->nodes_to_save.end());
    all_cols.insert(all_cols.end(), log->extra_signals.begin(), log->extra_signals.end());

    // Header row
    fprintf(f, "Time");
    for (const auto &col : all_cols) fprintf(f, ",%s", col.c_str());
    fprintf(f, "\n");

    // Data rows
    size_t n_steps = log->time_steps.size();
    for (size_t i = 0; i < n_steps; i++) {
        fprintf(f, "%.6e", log->time_steps[i]);
        for (const auto &col : all_cols) {
            const auto &series = log->results.at(col);
            double val = (i < series.size()) ? series[i] : 0.0;
            fprintf(f, ",%.6e", val);
        }
        fprintf(f, "\n");
    }

    fclose(f);
    fprintf(stdout, "--- Simulation results saved to %s (%zu columns, %zu timesteps) ---\n",
            filename, all_cols.size() + 1, n_steps);
}

// ─────────────────────────────────────────────────────────────────────────────
// LOG AN ACCEPTED SOLVER STEP (vm.hpp)
// The node columns come from the solution itself; the I() columns and the
// signals phase_log computes from V()/I() come from the read-view. Both must
// describe the same point. Every solver's last evaluation is at an iterate,
// not at the accepted solution: the iterate before the final solve for the
// Newton solvers (the previous time point when the step converged in one
// iteration — a 1 A R-L step's I(L) then lagged its own node voltage by a
// step, 1.1e-4 A), the step's start for euler_fixed. The read-view is put
// back afterwards: it is what the next step's first install shifts into
// nr_prev(), and logging must not change the run.
// ─────────────────────────────────────────────────────────────────────────────

void vm_log_accepted(VM *vm) {
    const Eigen::VectorXd &x = vm->solver->last_solution;
    const Eigen::VectorXd view = vm->api->last_solution;
    api_peek_solution(vm->api, x);
    logger_log_step(&vm->logger, vm->time, vm->solver->sys, x);
    if (vm->phase_log) vm->phase_log(vm);
    logger_log_signals(&vm->logger, vm->values);
    api_peek_solution(vm->api, view);
}
