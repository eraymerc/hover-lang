// ── HOVERCRAFT PUBLIC C ABI ─────────────────────────────────────────────────
// This is the only header a host program (Python via ctypes, C++, cgo, ...)
// needs to drive a --hovercraft-built libhovercraft.so / hovercraft.dll.
// Every HVR_* symbol below is extern "C" — no C++ types cross this boundary.
//
// The per-signal setters HVR_set_input_<name>() / HVR_set_param_<name>()
// are declared per-project by the generated library itself (their names
// depend on the .hvr source), not here — see the generated header comment
// at the top of sim.cpp, or examples/hovercraft/.
//
// Every HVRLogResult handed back by an HVR_get_* call must be released with
// HVR_free_log_result() — the arrays are malloc'd on the C++ side and a
// caller in another language (Python ctypes, Go/cgo) has no way to free
// them correctly itself.
// ─────────────────────────────────────────────────────────────────────────────
#pragma once

#ifdef __cplusplus
extern "C" {
#endif

typedef struct {
    double  *time;     /* n_rows entries */
    double **columns;  /* n_cols arrays, each n_rows entries */
    char   **names;    /* n_cols entries — column names, nodes then signals */
    long     n_rows;
    long     n_cols;
} HVRLogResult;

#define HVR_OK           0
#define HVR_ERR_TIME    -1  /* HVR_set_param_* called with sim time != 0   */
#define HVR_ERR_IO      -2  /* HVR_save_log could not open the file        */
#define HVR_ERR_UNKNOWN -3  /* invalid argument (e.g. negative duration)   */

// ── LIFECYCLE ────────────────────────────────────────────────────────────────

// Resets simulation state (state variables -> their initial values, time
// -> 0, OP re-solved if the .hvr source enables it). The log is left
// untouched — call HVR_reset_log() separately if you also want that cleared.
int  HVR_reset_sim(void);

// Clears logged data. Independent of simulation state.
void HVR_reset_log(void);

// Opt-in CSV dump — same column layout the standalone binary used to write
// automatically. Returns HVR_ERR_IO if the file couldn't be opened.
int  HVR_save_log(const char *filename);

// Advance the simulation. HVR_step(n) advances by n minimum timesteps
// (n * dt seconds); HVR_run advances by an arbitrary duration in
// simulation seconds. Both return HVR_ERR_UNKNOWN for a negative argument.
int  HVR_step(long n);
int  HVR_run(double duration_seconds);

// ── RETRIEVAL (caller frees every result with HVR_free_log_result) ──────────

HVRLogResult HVR_get_log(void);                               /* everything                 */
HVRLogResult HVR_get_log_range(double t_start, double t_end);  /* [t_start, t_end]            */
HVRLogResult HVR_get_log_latest(void);                         /* only since the last run/step*/
HVRLogResult HVR_get_last_step(void);                          /* single most-recent row      */

void HVR_clear_log_before(double t);

void HVR_free_log_result(HVRLogResult *r);

#ifdef __cplusplus
}
#endif
