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

// Current simulation time in seconds. 0 before the first step, and 0 again
// after HVR_reset_sim(). Reading it never starts the simulation, so it is
// safe to consult while deciding whether HVR_set_param_* is still allowed
// (they require t == 0 and return HVR_ERR_TIME otherwise).
double HVR_get_time(void);

// ── RETRIEVAL (caller frees every result with HVR_free_log_result) ──────────

HVRLogResult HVR_get_log(void);                               /* everything                 */
HVRLogResult HVR_get_log_range(double t_start, double t_end);  /* [t_start, t_end]            */
HVRLogResult HVR_get_log_latest(void);                         /* only since the last run/step*/
HVRLogResult HVR_get_last_step(void);                          /* single most-recent row      */

void HVR_clear_log_before(double t);

void HVR_free_log_result(HVRLogResult *r);

// ── PER-SIGNAL READBACK (no allocation, no log buffer) ─────────────────────

// Reads one .save()d quantity's CURRENT value — the live node voltage,
// branch current, or logic-signal value, not a logged row. Intended for a
// host polling a single signal per step, where an HVR_get_log* call would
// allocate and free a whole HVRLogResult just to look at one number.
//
// 'name' must match a log column exactly (the same strings HVRLogResult
// hands back in 'names'). Returns HVR_OK and writes *out, or
// HVR_ERR_UNKNOWN for an unknown name / NULL argument, leaving *out
// untouched — so "no such signal" is distinguishable from a signal whose
// value really is 0.
//
// The generated library ALSO exposes a zero-overhead getter per column,
// double HVR_get_output_<name>(void), with '.' and other non-identifier
// characters replaced by '_' (main.vout -> HVR_get_output_main_vout,
// I(main.vsense) -> HVR_get_output_I_main_vsense). Those names depend on
// the .hvr source, so like the HVR_set_* setters they are declared by the
// generated sim.cpp rather than here.
int HVR_get_output(const char *name, double *out);

// ── SELF-DESCRIPTION ────────────────────────────────────────────────────────

// Returns a JSON description of this library's generated ABI: every
// HVR_set_param_*, HVR_set_input_* and HVR_get_output_* symbol, with the
// type information needed to call it. Static storage owned by the library
// — do not free, valid for the library's lifetime.
//
// This is what makes a .so self-describing rather than something you need
// the original .hvr next to. It matters most for inputs: an exported
// symbol name carries no signature, and HVR_set_input_<name> is a
// one-argument scalar setter for a scalar input but a two-argument
// (const T *values, long n) setter for an array one. Guessing wrong
// segfaults rather than failing cleanly, so hosts should dispatch on the
// manifest's "kind" field.
//
// Shape (see hovercraft_drivers/python/ for a consumer):
//   {"hovercraft_abi": 1, "time_step": 1e-06, "end_time": 0.001,
//    "params":  [{"name","symbol","default"}],
//    "inputs":  [{"name","symbol","kind":"scalar"|"array","ctype","length"}],
//    "outputs": [{"column","symbol","kind":"node"|"signal"|"current"}]}
//
// An output's "symbol" is "" when its per-signal getter name collided with
// another column's; read that column through HVR_get_output instead.
const char *HVR_manifest(void);

#ifdef __cplusplus
}
#endif
