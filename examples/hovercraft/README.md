# --hovercraft library mode

Build with:

```
./hover mysim.hvr --hovercraft
```

This produces `libhovercraft.so` (Linux) or `hovercraft.dll` (Windows)
instead of a one-shot binary, plus `sim.cpp` alongside it if you want to
inspect what was generated. The library exposes a small C ABI — see
`runtime/hovercraft.h` — that a host program in any language with a C FFI
can drive: reset the simulation, advance it by a duration or a number of
steps, pull back logged data as it accumulates, and write a CSV on demand.

Two drivers are included here:

- `driver.py` — Python via `ctypes`, no build step needed.
- `driver.cpp` — a direct C++ consumer, showing the same calls without FFI
  marshalling.

## Setting inputs

`()` logic args on your main module become `HVR_set_input_<name>(value)`,
writable at any time. `<>` static args become `HVR_set_param_<name>(value)`,
settable only before the simulation has advanced (`t == 0`) — see the
CAVEAT comment in the generated library's source: today this only writes
the backing variable, it does not yet feed back into the equations (that
needs an elaborator-side change to stop inlining `<>` args as compile-time
literals for the main module specifically).

## Notes

- `HVR_step(n)` advances by `n` minimum timesteps (`n * dt` seconds).
  `HVR_run(duration)` advances by an arbitrary duration in simulation
  seconds — internally just `HVR_step`'s underlying primitive with a time
  target instead of a step count.
- Every `HVR_get_*` call returns memory you must release with
  `HVR_free_log_result()`.
- `HVR_save_log(filename)` is opt-in now — the standalone binary's automatic
  `simulation_output.csv` export at the end of a run has no equivalent here.
