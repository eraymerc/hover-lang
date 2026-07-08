// ── HOVERCRAFT RUNTIME GLUE ──────────────────────────────────────────────────
// Implements the hvr_rt_* helpers declared in ../hvr_runtime.hpp, which the
// generated HVR_* C ABI (compiler/codegen/hovercraft_emit.go) forwards to.
//
// *** VERIFICATION NOTE ***
// This file's use of Logger's fields (time_steps, results, nodes_to_save,
// extra_signals) reflects the Logger layout as previously seen for this
// repo, but has NOT been re-confirmed against your current
// runtime/vm/logger.hpp / logger.cpp — unlike vm.cpp, main.go, main_emit.go,
// and generator.go, which this patch edits against your freshly uploaded,
// verified sources. If Logger's actual field names differ, this is the one
// file that will fail to compile; send me logger.hpp and I'll fix it in one
// pass rather than another round of guessing.
// ─────────────────────────────────────────────────────────────────────────────
#include "../hvr_runtime.hpp"

#include <algorithm>
#include <cstdio>
#include <cstdlib>
#include <cstring>
#include <string>
#include <unordered_map>
#include <vector>

// Per-VM row index where the current run()/step() batch started appending,
// for HVR_get_log_latest(). A plain map keyed by VM* since there is exactly
// one VM instance per generated library today (see the file-scope `static
// VM vm;` hovercraft_emit.go emits) — this still works correctly if that
// ever changes to multiple VMs, it just isn't exercised that way yet.
static std::unordered_map<const VM *, size_t> g_batch_start;

void hvr_rt_mark_batch(VM *vm) {
    g_batch_start[vm] = vm->logger.time_steps.size();
}

void hvr_rt_reset_log(VM *vm) {
    vm->logger.time_steps.clear();
    for (auto &kv : vm->logger.results) {
        kv.second.clear();
    }
    g_batch_start[vm] = 0;
}

int hvr_rt_save_log_csv(VM *vm, const char *filename) {
    FILE *f = fopen(filename, "w");
    if (!f) {
        return HVR_ERR_IO;
    }
    fclose(f);
    logger_export_csv(&vm->logger, filename);
    return HVR_OK;
}

// build_result assembles the flat, malloc'd HVRLogResult for the row range
// [row_start, row_end) of vm->logger — the single point every hvr_rt_query_*
// function below funnels through, so the POD-construction and
// column-ordering logic (nodes_to_save, then extra_signals — matching the
// order logger_export_csv has always written) lives in exactly one place.
static HVRLogResult build_result(VM *vm, size_t row_start, size_t row_end) {
    HVRLogResult r{};
    Logger &log = vm->logger;

    if (row_end < row_start) {
        row_end = row_start;
    }
    size_t n_rows = row_end - row_start;

    std::vector<std::string> col_names;
    col_names.reserve(log.nodes_to_save.size() + log.extra_signals.size());
    for (auto &n : log.nodes_to_save) col_names.push_back(n);
    for (auto &s : log.extra_signals) col_names.push_back(s);

    r.n_rows = (long)n_rows;
    r.n_cols = (long)col_names.size();

    r.time = (double *)malloc(sizeof(double) * (n_rows ? n_rows : 1));
    for (size_t i = 0; i < n_rows; i++) {
        r.time[i] = log.time_steps[row_start + i];
    }

    r.columns = (double **)malloc(sizeof(double *) * (col_names.size() ? col_names.size() : 1));
    r.names   = (char **)malloc(sizeof(char *) * (col_names.size() ? col_names.size() : 1));

    for (size_t c = 0; c < col_names.size(); c++) {
        const std::string &name = col_names[c];
        r.names[c] = strdup(name.c_str());

        double *col = (double *)malloc(sizeof(double) * (n_rows ? n_rows : 1));
        auto it = log.results.find(name);
        for (size_t i = 0; i < n_rows; i++) {
            col[i] = (it != log.results.end() && row_start + i < it->second.size())
                         ? it->second[row_start + i]
                         : 0.0;
        }
        r.columns[c] = col;
    }

    return r;
}

HVRLogResult hvr_rt_query_all(VM *vm) {
    return build_result(vm, 0, vm->logger.time_steps.size());
}

HVRLogResult hvr_rt_query_range(VM *vm, double t0, double t1) {
    auto &ts = vm->logger.time_steps;
    size_t lo = std::lower_bound(ts.begin(), ts.end(), t0) - ts.begin();
    size_t hi = std::upper_bound(ts.begin(), ts.end(), t1) - ts.begin();
    return build_result(vm, lo, hi);
}

HVRLogResult hvr_rt_query_latest(VM *vm) {
    size_t start = 0;
    auto it = g_batch_start.find(vm);
    if (it != g_batch_start.end()) {
        start = it->second;
    }
    if (start > vm->logger.time_steps.size()) {
        start = vm->logger.time_steps.size();
    }
    return build_result(vm, start, vm->logger.time_steps.size());
}

HVRLogResult hvr_rt_query_last_step(VM *vm) {
    size_t n = vm->logger.time_steps.size();
    if (n == 0) {
        return build_result(vm, 0, 0);
    }
    return build_result(vm, n - 1, n);
}

void hvr_rt_clear_before(VM *vm, double t) {
    Logger &log = vm->logger;
    auto &ts = log.time_steps;
    size_t cut = std::lower_bound(ts.begin(), ts.end(), t) - ts.begin();
    if (cut == 0) {
        return;
    }
    ts.erase(ts.begin(), ts.begin() + cut);
    for (auto &kv : log.results) {
        auto &col = kv.second;
        size_t n = std::min(cut, col.size());
        col.erase(col.begin(), col.begin() + n);
    }
    auto it = g_batch_start.find(vm);
    if (it != g_batch_start.end()) {
        it->second = (it->second > cut) ? it->second - cut : 0;
    }
}

void hvr_rt_free_result(HVRLogResult *r) {
    if (!r) return;
    free(r->time);
    for (long c = 0; c < r->n_cols; c++) {
        free(r->columns[c]);
        free(r->names[c]);
    }
    free(r->columns);
    free(r->names);
    r->time = nullptr;
    r->columns = nullptr;
    r->names = nullptr;
    r->n_rows = 0;
    r->n_cols = 0;
}
