"""ctypes bindings for the fixed part of the hovercraft C ABI.

"Fixed" means everything declared in runtime/hovercraft.h -- the calls whose
names and signatures are identical for every generated library. The
per-project half (HVR_set_input_*, HVR_set_param_*, HVR_get_output_*) is
bound at runtime from the manifest instead; see core.py.
"""

import ctypes

# ── Return codes (runtime/hovercraft.h) ──────────────────────────────────────
HVR_OK = 0
HVR_ERR_TIME = -1
HVR_ERR_IO = -2
HVR_ERR_UNKNOWN = -3

ERROR_NAMES = {
    HVR_OK: "HVR_OK",
    HVR_ERR_TIME: "HVR_ERR_TIME",
    HVR_ERR_IO: "HVR_ERR_IO",
    HVR_ERR_UNKNOWN: "HVR_ERR_UNKNOWN",
}

# Maps the manifest's "ctype" strings to the ctypes type of the same width.
# These are the spellings codegen actually declares its setters with
# (CType.String() in compiler/codegen/ctype.go) -- note int64_t, NOT int:
# using c_int for an int64_t parameter would truncate on the way in.
CTYPES_BY_NAME = {
    "double": ctypes.c_double,
    "float": ctypes.c_float,
    "int64_t": ctypes.c_int64,
    "uint64_t": ctypes.c_uint64,
}


class HVRLogResult(ctypes.Structure):
    """Mirror of the HVRLogResult struct in runtime/hovercraft.h.

    Every instance handed back by an HVR_get_* call owns malloc'd memory
    and must be released with HVR_free_log_result. log.py copies the
    contents into Python objects and frees immediately, so no HVRLogResult
    ever outlives the call that produced it.
    """

    _fields_ = [
        ("time", ctypes.POINTER(ctypes.c_double)),
        ("columns", ctypes.POINTER(ctypes.POINTER(ctypes.c_double))),
        ("names", ctypes.POINTER(ctypes.c_char_p)),
        ("n_rows", ctypes.c_long),
        ("n_cols", ctypes.c_long),
    ]


# name -> (restype, argtypes). Applied by bind_fixed_abi below.
_FIXED_SIGNATURES = {
    "HVR_reset_sim": (ctypes.c_int, []),
    "HVR_reset_log": (None, []),
    "HVR_save_log": (ctypes.c_int, [ctypes.c_char_p]),
    "HVR_step": (ctypes.c_int, [ctypes.c_long]),
    "HVR_run": (ctypes.c_int, [ctypes.c_double]),
    "HVR_get_time": (ctypes.c_double, []),
    "HVR_get_log": (HVRLogResult, []),
    "HVR_get_log_range": (HVRLogResult, [ctypes.c_double, ctypes.c_double]),
    "HVR_get_log_latest": (HVRLogResult, []),
    "HVR_get_last_step": (HVRLogResult, []),
    "HVR_clear_log_before": (None, [ctypes.c_double]),
    "HVR_free_log_result": (None, [ctypes.POINTER(HVRLogResult)]),
    "HVR_get_output": (ctypes.c_int, [ctypes.c_char_p, ctypes.POINTER(ctypes.c_double)]),
    "HVR_manifest": (ctypes.c_char_p, []),
}

# Symbols added after the feature's first release. A library built by an
# older compiler simply won't export these, which is not an error -- the
# package degrades to the subset that is present rather than refusing to
# load a perfectly usable .so.
_OPTIONAL = {"HVR_get_output", "HVR_manifest", "HVR_get_time"}


def bind_fixed_abi(lib):
    """Apply argtypes/restype to every fixed ABI symbol present in `lib`.

    Returns the set of names that were found. Declaring these matters for
    more than tidiness: without an explicit restype, ctypes assumes int,
    which silently truncates the doubles HVR_get_time and the getters
    return, and mis-handles the by-value HVRLogResult structs entirely.
    """
    found = set()
    for name, (restype, argtypes) in _FIXED_SIGNATURES.items():
        try:
            fn = getattr(lib, name)
        except AttributeError:
            if name in _OPTIONAL:
                continue
            raise
        fn.restype = restype
        fn.argtypes = argtypes
        found.add(name)
    return found
