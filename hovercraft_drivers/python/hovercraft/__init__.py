"""hovercraft — drive a Hover-generated simulation library from Python.

    from hovercraft import Hovercraft

    hc = Hovercraft("rc.hvr")      # compiles to ./rc, loads it
    hc.params.gain = 3.0           # <> static args (t == 0 only)
    hc.inputs.vin = 5.0            # () logic args, any time
    hc.run(1e-3)
    print(hc.outputs.main_vout)    # live value
    print(hc.log().to_dict())      # logged history

Each Hovercraft is an independent simulation, even when several are made
from the same file -- see the class docstring for why that isn't free.
"""

from ._abi import HVR_ERR_IO, HVR_ERR_TIME, HVR_ERR_UNKNOWN, HVR_OK
from .compiler import compile_hvr, find_compiler, output_path_for
from .core import Hovercraft, load
from .errors import (
    CompileError,
    CompilerNotFoundError,
    HovercraftError,
    ManifestError,
    ParamLockedError,
    SimulationError,
    UnknownSignalError,
)
from .log import Log

__version__ = "0.1.0"

__all__ = [
    "Hovercraft",
    "load",
    "Log",
    "compile_hvr",
    "find_compiler",
    "output_path_for",
    "HovercraftError",
    "CompileError",
    "CompilerNotFoundError",
    "ManifestError",
    "ParamLockedError",
    "SimulationError",
    "UnknownSignalError",
    "HVR_OK",
    "HVR_ERR_TIME",
    "HVR_ERR_IO",
    "HVR_ERR_UNKNOWN",
]
