# hovercraft — Python driver library

Drive a Hover-generated simulation library from Python.

```python
from hovercraft import Hovercraft

hc = Hovercraft("rc_filter.hvr")   # compiles to ./rc_filter, then loads it

hc.params.gain = 2.0               # <> static args  (only at t == 0)
hc.inputs.vin = 1.0                # () logic args   (any time)
hc.inputs.taps = [0.5, 0.25, 0.25] # array inputs take a sequence

hc.run(1e-3)                       # advance 1 ms
print(hc.outputs.main_vout)        # live value, no log query
print(hc.log().to_dict())          # full logged history
```

No dependencies — `ctypes` only. numpy and pandas are used if present
(`Log.to_numpy()` / `Log.to_pandas()`) but never required.

Run the demos:

```
# closed loop: read the output each step, feed it back into the input
python3 hovercraft_drivers/python/examples/closed_loop_rc/drive_rc.py

# a playable lunar lander whose flight dynamics are a Hover circuit
python3 hovercraft_drivers/python/examples/lunar_lander/lunar_lander.py
```

## Source or library

The constructor takes either, and figures out which from the suffix:

```python
Hovercraft("motor.hvr")              # compiles -> ./motor, loads that
Hovercraft("libhovercraft.so")       # loads as-is
```

A `.hvr` is compiled with `--hovercraft -o <source without .hvr>`, so
`motor.hvr` becomes `motor`. No platform extension is added; ctypes loads a
library by path, not by name.

Recompilation is controlled by `rebuild=`:

| value | behaviour |
|---|---|
| `"auto"` (default) | compile if the output is missing or older than the source |
| `"always"` | compile unconditionally |
| `"never"` | load the existing output, error if absent |

`"auto"` only stats the entry file. A changed **imported** `.hvr` won't be
noticed — the import graph is known only to the compiler — so use
`"always"` while editing libraries.

The compiler is found via `compiler=`, then `$HOVER`, then the enclosing
checkout, then `./hover`, then `$PATH`. Build it with `go build -o hover .`
in the repository root.

## One object per simulation

Each `Hovercraft` is an independent simulation, including several built
from the same file:

```python
a = Hovercraft("rc_filter.hvr"); a.params.gain = 1.0
b = Hovercraft("rc_filter.hvr", rebuild="never"); b.params.gain = 100.0
a.run(1e-4); b.run(1e-4)          # two separate simulations
```

This is not free, and is worth understanding. A generated library keeps all
of its state in **file-scope statics**, and `dlopen` returns the *same*
handle for a path already loaded — so loading one file twice would hand you
two objects quietly sharing one simulation, where writes through `a` show
up in `b`. To prevent that, each instance copies the library to a private
temporary path and loads *that*; a distinct path gets a distinct mapping
with its own statics. The temp file is unlinked as soon as it is mapped, so
nothing is left behind even if the process dies.

Pass `isolated=False` to load in place when you only ever want one instance
(slightly faster startup, no copy).

## Inputs, params, outputs

The library describes its own ABI through `HVR_manifest()`, so these are
discovered rather than hardcoded — `hc.describe()` prints the lot, and
attribute names tab-complete in a REPL.

```python
hc.params.gain = 2.0          hc.params["gain"]      list(hc.params)
hc.inputs.vin = 1.0           hc.inputs["vin"]       hc.inputs.to_dict()
hc.outputs.main_vout          hc["main.vout"]        hc.snapshot()
```

- **`params`** — main's `<>` static args. Settable only at `t == 0`;
  afterwards you get `ParamLockedError` telling you to `reset()` first.
  Reads return the last value written (or the compiled-in default), since
  the ABI has setters only.
- **`inputs`** — main's `()` logic args, settable any time. Arrays take a
  sequence; a short one writes only the leading elements, and an oversized
  one raises rather than being silently truncated.
- **`outputs`** — every `.save()`d column, read live. Attribute names are
  the column with non-identifier characters replaced (`main.vout` →
  `main_vout`, `I(main.vsense)` → `I_main_vsense`); `hc["main.vout"]` takes
  the exact column name.

Scalar and array inputs use *different C signatures*, and the manifest is
what tells them apart. Passing a scalar where the library expects
`(const double *values, long n)` would put a double in a register the
callee reads as a pointer — an immediate segfault, not an exception. Hence
the type checks: `hc.inputs.taps = 1.0` raises `TypeError` instead.

Libraries built before `HVR_manifest()` existed still load; `run`, `step`,
`log`, `read` and `save_csv` all work, and only the introspective
attributes raise `ManifestError`.

## Reading results

```python
hc.outputs.main_vout     # live value, no allocation
hc["main.vout"]          # same, by exact column name
hc.snapshot()            # {column: value} for everything

hc.log()                 # everything logged so far
hc.log_latest()          # only what the last run()/step() appended
hc.log_range(t0, t1)     # a time window
hc.last_step()           # the single most recent row
```

A `Log` holds plain Python lists, already detached from the library (the
C-side result is freed before it is handed back), so it stays valid after
the `Hovercraft` is closed:

```python
log["main.vout"]      # one column
len(log)              # rows
for row in log: ...   # dicts, time included
log.to_dict() / log.to_numpy() / log.to_pandas() / log.to_csv(path)
```

## Lifecycle

```python
hc.run(seconds)       hc.step(n)          # advance
hc.reset()            # state + time -> 0; params settable again; log kept
hc.reset_log()        # clear the log; simulation untouched
hc.clear_log_before(t)  # bound memory during a long run
hc.save_csv(path)     # library-side CSV export
hc.time               # current simulation time
hc.close()            # or use it as a context manager
```

## Gotcha: what `<>` params can actually reach

A `<>` param is only runtime-settable where it is read by **equations in
main's own body**. If main passes it to a submodule (`Src<amp, freq>()`) or
uses it as an element value (`R<rload>()`), elaboration folds it to a
constant and stamps it into the netlist once — the setter will return
`HVR_OK` and change nothing. See `docs/hovercraft-issues.md` #6.

Practically, that means a main with tunable params must be an `analog` (or
`digital`) module, since a plain structural `module main` cannot contain
equations at all. `examples/rc_filter.hvr` shows the working shape: an
`analog module main` holding both the equation that reads `gain` and the
circuit elements it drives.
