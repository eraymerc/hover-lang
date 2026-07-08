// Minimal C++ driver for a --hovercraft-built Hover library.
//
// Build (link directly against the generated shared library):
//   g++ -std=c++17 driver.cpp -L. -lhovercraft -Wl,-rpath,. -o driver
//   ./driver
//
// Or dlopen() it at runtime instead, if you don't want a build-time link
// dependency on a project-specific library name — ctypes-style, but in C++.
#include "../../runtime/hovercraft.h"

#include <cstdio>
#include <vector>

static void print_latest(const HVRLogResult &r) {
    if (r.n_rows == 0) return;
    long last = r.n_rows - 1;
    std::printf("t=%.6f", r.time[last]);
    for (long c = 0; c < r.n_cols; c++) {
        std::printf("  %s=%.4g", r.names[c], r.columns[c][last]);
    }
    std::printf("\n");
}

int main() {
    // Inputs, if your model has any:
    //   HVR_set_input_vref(5.0);

    for (int i = 0; i < 10; i++) {
        HVR_run(1e-3); // 1 ms of simulated time

        HVRLogResult batch = HVR_get_log_latest();
        print_latest(batch);
        HVR_free_log_result(&batch);
    }

    HVRLogResult full = HVR_get_log();
    std::printf("\n%ld total rows logged.\n", full.n_rows);
    HVR_free_log_result(&full);

    if (HVR_save_log("from_cpp.csv") == HVR_OK) {
        std::printf("saved from_cpp.csv\n");
    }

    return 0;
}
