"""Hovercraft — one Python object per simulation instance."""

import ctypes
import json
import os
import shutil
import tempfile
import weakref
from pathlib import Path

from . import _abi
from ._abi import HVR_ERR_TIME, HVR_OK, HVRLogResult, ERROR_NAMES
from .compiler import is_source, resolve_library
from .errors import (
    ManifestError,
    ParamLockedError,
    SimulationError,
    UnknownSignalError,
)
from .log import Log


class _Namespace:
    """Attribute/mapping view over one group of named ABI entries.

    Subclasses supply _read/_write. Attribute access is the ergonomic form
    (`hc.inputs.vin = 5`); item access (`hc.inputs["vin"] = 5`) exists for
    names that aren't valid Python identifiers and for programmatic use.
    """

    __slots__ = ("_entries", "_owner")

    def __init__(self, owner, entries):
        object.__setattr__(self, "_owner", owner)
        object.__setattr__(self, "_entries", entries)

    # -- to be provided by subclasses --
    def _read(self, entry):
        raise NotImplementedError

    def _write(self, entry, value):
        raise NotImplementedError

    def _resolve(self, name):
        try:
            return self._entries[name]
        except KeyError:
            raise UnknownSignalError(
                "{} has no {} {!r}. Available: {}".format(
                    self._owner, self._kind, name,
                    ", ".join(sorted(self._entries)) or "(none)")
            ) from None

    def __getattr__(self, name):
        if name.startswith("_"):
            raise AttributeError(name)
        return self._read(self._resolve(name))

    def __setattr__(self, name, value):
        if name.startswith("_"):
            object.__setattr__(self, name, value)
            return
        self._write(self._resolve(name), value)

    def __getitem__(self, name):
        return self._read(self._resolve(name))

    def __setitem__(self, name, value):
        self._write(self._resolve(name), value)

    def __contains__(self, name):
        return name in self._entries

    def __iter__(self):
        return iter(sorted(self._entries))

    def __len__(self):
        return len(self._entries)

    def keys(self):
        return sorted(self._entries)

    def to_dict(self):
        return {n: self._read(e) for n, e in sorted(self._entries.items())}

    def __dir__(self):
        # Makes names tab-completable in a REPL, which is most of the point
        # of exposing them as attributes at all.
        return sorted(self._entries) + ["keys", "to_dict"]

    def __repr__(self):
        if not self._entries:
            return "<no {}s>".format(self._kind)
        return "<{}s: {}>".format(
            self._kind,
            ", ".join("{}={!r}".format(n, self._read(e))
                      for n, e in sorted(self._entries.items())))


class _Params(_Namespace):
    """main's `<>` static args.

    Reads return the last value written (or the compiled-in default): the
    ABI has setters only, and the library is the authority on nothing a
    caller didn't already tell it.
    """

    __slots__ = ()
    _kind = "param"

    def _read(self, entry):
        return entry["value"]

    def _write(self, entry, value):
        owner = self._owner
        rc = entry["fn"](ctypes.c_double(float(value)))
        if rc == HVR_ERR_TIME:
            raise ParamLockedError(
                "cannot set param {!r}: the simulation has already advanced "
                "(t = {:g}s). `<>` params are folded into element values and "
                "the operating-point solve at boot, so they are only settable "
                "at t = 0. Call .reset() first.".format(entry["name"], owner.time)
            )
        if rc != HVR_OK:
            raise SimulationError(
                "setting param {!r} failed: {}".format(
                    entry["name"], ERROR_NAMES.get(rc, rc)))
        entry["value"] = float(value)


class _Inputs(_Namespace):
    """main's `()` logic args -- writable at any time.

    Scalars and arrays dispatch to different C signatures, chosen from the
    manifest rather than from the value passed in. That distinction cannot
    be guessed safely: calling the two-argument array setter as if it took
    a single double segfaults instead of raising.
    """

    __slots__ = ()
    _kind = "input"

    def _read(self, entry):
        return entry["value"]

    def _write(self, entry, value):
        if entry["kind"] == "array":
            self._write_array(entry, value)
            return
        if isinstance(value, (list, tuple)):
            raise TypeError(
                "input {!r} is a scalar {}, got a sequence of {} values".format(
                    entry["name"], entry["ctype"], len(value)))
        cvalue = entry["cvalue"](value)
        entry["fn"](cvalue)
        entry["value"] = cvalue.value

    def _write_array(self, entry, value):
        if isinstance(value, (int, float)):
            raise TypeError(
                "input {!r} is an array of {} {}, not a scalar -- pass a "
                "sequence (a single value can be written as [{}])".format(
                    entry["name"], entry["length"], entry["ctype"], value))
        values = list(value)
        capacity = entry["length"]
        if len(values) > capacity:
            raise ValueError(
                "input {!r} holds {} elements, got {}. The C setter would "
                "silently drop the extras; refusing rather than losing "
                "data quietly.".format(entry["name"], capacity, len(values)))
        buf = (entry["cvalue"] * len(values))(*values)
        entry["fn"](buf, ctypes.c_long(len(values)))
        # A short write updates only the leading elements, so mirror that
        # into the tracked value rather than replacing it wholesale.
        current = list(entry["value"])
        current[:len(values)] = values
        entry["value"] = current


class _Outputs(_Namespace):
    """.save()d columns, read live (not from the log)."""

    __slots__ = ()
    _kind = "output"

    def _read(self, entry):
        fn = entry.get("fn")
        if fn is not None:
            return fn()
        return self._owner.read(entry["column"])

    def _write(self, entry, value):
        raise AttributeError(
            "outputs are read-only ({!r} is a simulation result)".format(
                entry["column"]))


class Hovercraft:
    """One hovercraft simulation.

    Accepts either Hover source or an already-built library::

        hc = Hovercraft("motor.hvr")        # compiled to ./motor, then loaded
        hc = Hovercraft("libhovercraft.so") # loaded as-is

    Each instance is an independent simulation. That takes real work: a
    generated library keeps its state in file-scope statics, and dlopen
    returns the *same* handle for a path already loaded -- so two objects
    made from one file would silently share one simulation. By default the
    library file is copied to a private temporary path before loading, so
    each object gets its own copy of those statics. Pass isolated=False to
    load in place when you only ever want one instance.
    """

    def __init__(self, source, *, rebuild="auto", compiler=None, output=None,
                 isolated=True, extra_args=(), name=None):
        if rebuild not in ("auto", "always", "never"):
            raise ValueError(
                "rebuild must be 'auto', 'always' or 'never', got {!r}".format(
                    rebuild))

        self._closed = False
        self._temp_path = None
        self._lib = None

        self.source = Path(source)
        self.compiled = is_source(self.source)
        self.library_path, self.was_compiled = resolve_library(
            source, rebuild=rebuild, compiler=compiler, output=output,
            extra_args=extra_args)
        self.name = name or self.library_path.name
        self.isolated = isolated

        load_path = self.library_path
        if isolated:
            load_path = self._make_private_copy(self.library_path)

        try:
            self._lib = ctypes.CDLL(str(load_path))
        except OSError:
            self._drop_temp()
            raise

        # A private copy has served its purpose once mapped; unlinking now
        # means a crashed process leaves no litter behind. POSIX keeps the
        # mapping valid after unlink; Windows refuses, so it is cleaned up
        # at close() there instead.
        if isolated:
            self._drop_temp(unlink_only=True)

        self._available = _abi.bind_fixed_abi(self._lib)
        self._manifest = self._load_manifest()
        self._bind_generated_abi()

        # Release the private copy (Windows) even if the caller never calls
        # close(), without resurrecting self in a __del__.
        self._finalizer = weakref.finalize(self, _cleanup_temp, self._temp_path)

    # ── construction helpers ─────────────────────────────────────────────

    def _make_private_copy(self, path):
        fd, tmp = tempfile.mkstemp(prefix="hvr_", suffix=path.suffix or ".so")
        os.close(fd)
        shutil.copy2(str(path), tmp)
        self._temp_path = tmp
        return Path(tmp)

    def _drop_temp(self, unlink_only=False):
        if not self._temp_path:
            return
        try:
            os.unlink(self._temp_path)
        except OSError:
            # Windows: still mapped. Leave it for close()/finalize.
            return
        if unlink_only:
            self._temp_path = None
        else:
            self._temp_path = None

    def _load_manifest(self):
        if "HVR_manifest" not in self._available:
            return None
        raw = self._lib.HVR_manifest()
        if not raw:
            return None
        try:
            return json.loads(raw.decode("utf-8"))
        except (ValueError, UnicodeDecodeError) as exc:
            raise ManifestError(
                "{}: HVR_manifest() returned unparseable JSON: {}".format(
                    self.name, exc)) from None

    def _bind_generated_abi(self):
        """Bind the per-project half of the ABI described by the manifest."""
        params, inputs, outputs = {}, {}, {}
        if self._manifest:
            for p in self._manifest.get("params", []):
                fn = self._sym(p["symbol"])
                if fn is None:
                    continue
                fn.restype = ctypes.c_int
                fn.argtypes = [ctypes.c_double]
                params[p["name"]] = {
                    "name": p["name"], "fn": fn,
                    "value": float(p.get("default", 0.0)),
                }

            for i in self._manifest.get("inputs", []):
                fn = self._sym(i["symbol"])
                if fn is None:
                    continue
                cvalue = _abi.CTYPES_BY_NAME.get(i.get("ctype"), ctypes.c_double)
                is_array = i.get("kind") == "array"
                length = int(i.get("length", 1))
                fn.restype = None
                fn.argtypes = ([ctypes.POINTER(cvalue), ctypes.c_long]
                               if is_array else [cvalue])
                inputs[i["name"]] = {
                    "name": i["name"], "fn": fn, "cvalue": cvalue,
                    "kind": "array" if is_array else "scalar",
                    "ctype": i.get("ctype", "double"), "length": length,
                    "value": ([0.0] * length if is_array else cvalue(0).value),
                }

            for o in self._manifest.get("outputs", []):
                column = o["column"]
                symbol = o.get("symbol") or ""
                fn = self._sym(symbol) if symbol else None
                if fn is not None:
                    fn.restype = ctypes.c_double
                    fn.argtypes = []
                # Attribute name comes from the symbol the compiler chose,
                # so the two can't drift apart; a column whose getter name
                # collided is reachable by column name only.
                attr = symbol[len("HVR_get_output_"):] if symbol else None
                entry = {"column": column, "fn": fn, "kind": o.get("kind")}
                outputs[attr or column] = entry

        self.params = _Params(self, params)
        self.inputs = _Inputs(self, inputs)
        self.outputs = _Outputs(self, outputs)

    def _sym(self, name):
        if not name:
            return None
        try:
            return getattr(self._lib, name)
        except AttributeError:
            return None

    # ── state ────────────────────────────────────────────────────────────

    def _check_open(self):
        if self._closed:
            raise RuntimeError(
                "{} is closed".format(self.name))

    def _require(self, symbol, feature):
        if symbol not in self._available:
            raise ManifestError(
                "{} does not export {} -- it was built by a compiler older "
                "than the {} feature. Rebuild it with the current "
                "compiler.".format(self.name, symbol, feature))

    @property
    def lib(self):
        """The raw ctypes handle, for anything this class doesn't wrap."""
        self._check_open()
        return self._lib

    @property
    def manifest(self):
        """The library's self-description, or None if it predates it."""
        return self._manifest

    @property
    def time(self):
        """Current simulation time in seconds."""
        self._check_open()
        if "HVR_get_time" in self._available:
            return float(self._lib.HVR_get_time())
        # Older library: the last logged row is the best available answer.
        last = self.last_step()
        return last.time[-1] if len(last) else 0.0

    @property
    def time_step(self):
        return (self._manifest or {}).get("time_step")

    @property
    def end_time(self):
        return (self._manifest or {}).get("end_time")

    # ── lifecycle ────────────────────────────────────────────────────────

    def run(self, duration_seconds):
        """Advance the simulation by a duration in simulation seconds."""
        self._check_open()
        self._ok(self._lib.HVR_run(ctypes.c_double(float(duration_seconds))),
                 "run({})".format(duration_seconds))
        return self

    def step(self, n=1):
        """Advance by n minimum timesteps (n * dt seconds)."""
        self._check_open()
        self._ok(self._lib.HVR_step(ctypes.c_long(int(n))), "step({})".format(n))
        return self

    def reset(self):
        """Reset simulation state and time to 0. Leaves the log alone.

        Params become settable again afterwards; inputs keep their current
        values, which are re-sampled by the boot that follows.
        """
        self._check_open()
        self._ok(self._lib.HVR_reset_sim(), "reset()")
        return self

    def reset_log(self):
        """Clear logged data. Independent of simulation state."""
        self._check_open()
        self._lib.HVR_reset_log()
        return self

    def clear_log_before(self, t):
        """Drop logged rows before time t -- bounds memory in a long run."""
        self._check_open()
        self._lib.HVR_clear_log_before(ctypes.c_double(float(t)))
        return self

    def save_csv(self, path):
        """Ask the library to write its log as CSV."""
        self._check_open()
        self._ok(self._lib.HVR_save_log(str(path).encode("utf-8")),
                 "save_csv({!r})".format(str(path)))
        return self

    def _ok(self, rc, what):
        if rc != HVR_OK:
            raise SimulationError("{}: {} returned {}".format(
                self.name, what, ERROR_NAMES.get(rc, rc)))

    # ── reading ──────────────────────────────────────────────────────────

    def read(self, column):
        """Current value of one .save()d column, by its exact name."""
        self._check_open()
        self._require("HVR_get_output", "per-signal readback")
        out = ctypes.c_double()
        rc = self._lib.HVR_get_output(str(column).encode("utf-8"),
                                      ctypes.byref(out))
        if rc != HVR_OK:
            raise UnknownSignalError(
                "{} has no output column {!r}. Available: {}".format(
                    self.name, column, ", ".join(self.columns) or "(none)"))
        return out.value

    def __getitem__(self, column):
        return self.read(column)

    @property
    def columns(self):
        """Every .save()d column name, in log order."""
        if self._manifest:
            return [o["column"] for o in self._manifest.get("outputs", [])]
        return list(self.last_step().names)

    def snapshot(self):
        """{column: current value} for every output -- one consistent read."""
        return {c: self.read(c) for c in self.columns}

    # ── log queries ──────────────────────────────────────────────────────

    def log(self):
        """Everything logged so far."""
        return self._query(self._lib.HVR_get_log)

    def log_range(self, t_start, t_end):
        """Logged rows within [t_start, t_end]."""
        self._check_open()
        return Log.from_result(
            self._lib,
            self._lib.HVR_get_log_range(ctypes.c_double(float(t_start)),
                                        ctypes.c_double(float(t_end))))

    def log_latest(self):
        """Only what the most recent run()/step() appended."""
        return self._query(self._lib.HVR_get_log_latest)

    def last_step(self):
        """The single most recent logged row."""
        return self._query(self._lib.HVR_get_last_step)

    def _query(self, fn):
        self._check_open()
        return Log.from_result(self._lib, fn())

    # ── teardown ─────────────────────────────────────────────────────────

    def close(self):
        """Release the private library copy.

        The library itself is not dlclose()d: ctypes offers no portable way
        to do that, and a generated library has no teardown hook. What this
        does guarantee is that the temporary file is gone and that further
        calls fail loudly instead of touching a half-released instance.
        """
        if self._closed:
            return
        self._closed = True
        if getattr(self, "_finalizer", None) is not None:
            self._finalizer()
        self._temp_path = None

    def __enter__(self):
        return self

    def __exit__(self, *exc):
        self.close()
        return False

    # ── presentation ─────────────────────────────────────────────────────

    def describe(self):
        """Human-readable summary of this library's generated ABI."""
        lines = ["{} ({})".format(self.name, self.library_path)]
        if self.was_compiled:
            lines.append("  compiled from {}".format(self.source))
        if self._manifest is None:
            lines.append("  (no manifest -- built before HVR_manifest();"
                         " introspection unavailable)")
            return "\n".join(lines)
        lines.append("  dt = {}  end_time = {}  t = {:g}".format(
            self.time_step, self.end_time, self.time))
        for title, ns in (("params", self.params), ("inputs", self.inputs)):
            lines.append("  {}:".format(title))
            if not len(ns):
                lines.append("    (none)")
            for n in ns:
                e = ns._entries[n]
                shape = ("{}[{}]".format(e["ctype"], e["length"])
                         if e.get("kind") == "array"
                         else e.get("ctype", "double"))
                lines.append("    {:<20} {:<14} = {!r}".format(
                    n, shape, ns._read(e)))
        lines.append("  outputs:")
        outs = (self._manifest or {}).get("outputs", [])
        if not outs:
            lines.append("    (none -- nothing .save()d)")
        for o in outs:
            lines.append("    {:<20} {}".format(o["column"], o.get("kind", "")))
        return "\n".join(lines)

    def __repr__(self):
        if self._closed:
            return "<Hovercraft {!r} closed>".format(self.name)
        return "<Hovercraft {!r} t={:g}s params={} inputs={} outputs={}>".format(
            self.name, self.time, len(self.params), len(self.inputs),
            len(self.outputs))


def _cleanup_temp(path):
    """weakref.finalize callback -- must not capture the Hovercraft."""
    if path:
        try:
            os.unlink(path)
        except OSError:
            pass


def load(source, **kwargs):
    """Convenience alias for Hovercraft(source, **kwargs)."""
    return Hovercraft(source, **kwargs)
