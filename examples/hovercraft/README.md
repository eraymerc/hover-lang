# --hovercraft library mode

Build with:

```
./hover mysim.hvr --hovercraft
./hover mysim.hvr --hovercraft -o build/libmysim.so   # custom output name
```

This produces `libhovercraft.so` (Linux) or `hovercraft.dll` (Windows) —
or whatever `-o` names, used verbatim with no extension fixups, like
clang — instead of a one-shot binary, plus `sim.cpp` in the working
directory if you want to inspect what was generated. The library exposes a small C ABI — see
`runtime/hovercraft.h` — that a host program in any language with a C FFI
can drive: reset the simulation, advance it by a duration or a number of
steps, pull back logged data as it accumulates, and write a CSV on demand.

Two drivers are included here:

- `driver.py` — Python via `ctypes`, no build step needed.
- `driver.cpp` — a direct C++ consumer, showing the same calls without FFI
  marshalling.

## Setting inputs

`()` logic args on your main module become `HVR_set_input_<name>(value)`,
writable at any time. An array-typed `()` arg gets an explicit-length
setter instead — `HVR_set_input_<name>(const T *values, long n)` — which
writes the leading `min(n, capacity)` elements, so a short `n` updates
only a prefix and an oversized one is clamped rather than overrunning.

`<>` static args become `HVR_set_param_<name>(value)`, settable only
before the simulation has advanced: they return `HVR_ERR_TIME` once
`t > 0`, and become settable again after `HVR_reset_sim()`.

One limit worth knowing: a `<>` arg reaches equations written in main's
own body, but **not** element values or the static args main passes down
to submodules (`R<rload>()`, `module s = Src<amp, freq>()`). Those are
folded to constants during elaboration and stamped into the netlist once,
so changing the param afterwards cannot move them. If you need a
runtime-tunable component value, drive it from a `()` input instead.

## Reading outputs

Every `.save()`d column is readable directly, without going through the
log:

- `double HVR_get_output_<name>(void)` — zero-overhead, one per column.
  Non-identifier characters in the column name become `_`
  (`main.vout` → `HVR_get_output_main_vout`,
  `I(main.vsense)` → `HVR_get_output_I_main_vsense`).
- `int HVR_get_output(const char *name, double *out)` — name-keyed, using
  the exact column strings `HVRLogResult::names` reports. Returns
  `HVR_ERR_UNKNOWN` for an unknown name and leaves `*out` alone, so "no
  such signal" is distinguishable from a value that really is 0. This is
  the one to use when columns are discovered at runtime.

Both report the *current* value — the live node voltage, branch current,
or logic signal — so they stay correct between logged steps, and neither
allocates anything to free. The `HVR_get_log*` family is still the way to
pull back history in bulk.

## Notes

- `HVR_step(n)` advances by `n` minimum timesteps (`n * dt` seconds).
  `HVR_run(duration)` advances by an arbitrary duration in simulation
  seconds — internally just `HVR_step`'s underlying primitive with a time
  target instead of a step count.
- Every `HVR_get_*` call returns memory you must release with
  `HVR_free_log_result()`.
- `HVR_save_log(filename)` is opt-in now — the standalone binary's automatic
  `simulation_output.csv` export at the end of a run has no equivalent here.
