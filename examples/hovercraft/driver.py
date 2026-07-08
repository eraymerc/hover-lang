#!/usr/bin/env python3
"""
Minimal ctypes driver for a --hovercraft-built Hover library.

Usage:
    python3 driver.py ./libhovercraft.so
"""
import ctypes as C
import sys


class HVRLogResult(C.Structure):
    _fields_ = [
        ("time", C.POINTER(C.c_double)),
        ("columns", C.POINTER(C.POINTER(C.c_double))),
        ("names", C.POINTER(C.c_char_p)),
        ("n_rows", C.c_long),
        ("n_cols", C.c_long),
    ]


HVR_OK = 0


def load(path):
    lib = C.CDLL(path)

    lib.HVR_reset_sim.restype = C.c_int
    lib.HVR_reset_log.restype = None
    lib.HVR_save_log.argtypes = [C.c_char_p]
    lib.HVR_save_log.restype = C.c_int

    lib.HVR_step.argtypes = [C.c_long]
    lib.HVR_step.restype = C.c_int
    lib.HVR_run.argtypes = [C.c_double]
    lib.HVR_run.restype = C.c_int

    for name in ("HVR_get_log", "HVR_get_log_latest", "HVR_get_last_step"):
        fn = getattr(lib, name)
        fn.argtypes = []
        fn.restype = HVRLogResult
    lib.HVR_get_log_range.argtypes = [C.c_double, C.c_double]
    lib.HVR_get_log_range.restype = HVRLogResult

    lib.HVR_clear_log_before.argtypes = [C.c_double]
    lib.HVR_clear_log_before.restype = None
    lib.HVR_free_log_result.argtypes = [C.POINTER(HVRLogResult)]
    lib.HVR_free_log_result.restype = None

    return lib


def to_dict(lib, res):
    """Copy an HVRLogResult into plain Python lists, then free the C memory."""
    n_rows, n_cols = res.n_rows, res.n_cols
    out = {"time": [res.time[i] for i in range(n_rows)]}
    for c in range(n_cols):
        name = res.names[c].decode()
        col = res.columns[c]
        out[name] = [col[i] for i in range(n_rows)]
    lib.HVR_free_log_result(C.byref(res))
    return out


def main():
    if len(sys.argv) != 2:
        print("usage: driver.py <path-to-libhovercraft.so>")
        sys.exit(1)

    lib = load(sys.argv[1])

    # Set inputs before the first step/run if your model has any:
    #   lib.HVR_set_input_vref.argtypes = [C.c_double]
    #   lib.HVR_set_input_vref(5.0)

    for _ in range(10):
        lib.HVR_run(1)  # 1 ms of simulated time
        batch = to_dict(lib, lib.HVR_get_log_latest())
        if batch["time"]:
            print(f"t={batch['time'][-1]:.6f}  "
                  + "  ".join(f"{k}={v[-1]:.4g}" for k, v in batch.items() if k != "time"))

    full = to_dict(lib, lib.HVR_get_log())
    print(f"\n{len(full['time'])} total rows logged.")

    if lib.HVR_save_log(b"from_python.csv") == HVR_OK:
        print("saved from_python.csv")


if __name__ == "__main__":
    main()
