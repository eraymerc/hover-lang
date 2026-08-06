"""Exception types raised by the hovercraft package.

Every failure the C ABI can report as a negative return code surfaces here
as an exception instead, so a caller never has to check a return value by
hand -- a silently ignored -1 from HVR_set_param_* is exactly the class of
bug this package exists to prevent.
"""


class HovercraftError(Exception):
    """Base class for everything this package raises."""


class CompileError(HovercraftError):
    """The .hvr source failed to compile.

    Carries the compiler's own stdout/stderr, since the Hover compiler's
    diagnostics (semantic errors, missing imports, and so on) are far more
    useful than anything this package could say about the failure.
    """

    def __init__(self, command, returncode, output):
        self.command = command
        self.returncode = returncode
        self.output = output
        super().__init__(
            "hover compilation failed (exit {}):\n"
            "  command: {}\n\n{}".format(returncode, " ".join(command), output)
        )


class CompilerNotFoundError(HovercraftError, FileNotFoundError):
    """The `hover` compiler binary could not be located."""


class ManifestError(HovercraftError):
    """The library does not describe itself, or describes itself unusably.

    Raised when introspection (`.params`, `.inputs`, `.outputs`) is used on
    a library built before HVR_manifest() existed. The library still works
    through the fixed ABI -- run/step/log/read all remain available.
    """


class ParamLockedError(HovercraftError):
    """A `<>` static param was set after the simulation had advanced.

    `<>` params are only settable while t == 0, because they are folded
    into element values and the OP solve at boot. Call `reset()` to return
    to t == 0 and make them settable again.
    """


class UnknownSignalError(HovercraftError, KeyError):
    """A name that isn't a .save()d column was read.

    Subclasses KeyError so `hc["typo"]` behaves like a normal failed
    mapping lookup.
    """

    def __str__(self):
        # KeyError.__str__ repr()s its argument, which turns a helpful
        # multi-line message into one quoted blob.
        return self.args[0] if self.args else ""


class SimulationError(HovercraftError):
    """A lifecycle call (run/step/reset/save) returned a failure code."""
