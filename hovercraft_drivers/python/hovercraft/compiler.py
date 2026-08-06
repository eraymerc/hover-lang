"""Locating the `hover` compiler and building .hvr sources into libraries."""

import os
import shutil
import subprocess
from pathlib import Path

from .errors import CompileError, CompilerNotFoundError

# Suffixes treated as "already built" rather than as Hover source.
LIBRARY_SUFFIXES = {".so", ".dll", ".dylib"}


def find_compiler(explicit=None):
    """Locate the `hover` binary.

    Search order: an explicit path, $HOVER, the repository this package is
    vendored in, ./hover, then $PATH.

    The repository copy is tried before $PATH deliberately. The compiler
    resolves `standard_library/` relative to its own location and passes
    `-I./runtime` plus `./runtime/libhover_runtime.a` to the C++ compiler,
    so a `hover` binary that isn't sitting in a checkout cannot build
    anything -- preferring the in-repo one makes the common case (this
    package used from its own checkout) work with no configuration.
    """
    candidates = []
    if explicit:
        candidates.append(Path(explicit))
    if os.environ.get("HOVER"):
        candidates.append(Path(os.environ["HOVER"]))

    # <repo>/hovercraft_drivers/python/hovercraft/compiler.py -> <repo>
    repo_root = Path(__file__).resolve().parents[3]
    candidates.append(repo_root / "hover")
    candidates.append(Path.cwd() / "hover")

    on_path = shutil.which("hover")
    if on_path:
        candidates.append(Path(on_path))

    for cand in candidates:
        if cand.is_file() and os.access(cand, os.X_OK):
            return cand.resolve()

    raise CompilerNotFoundError(
        "could not find the 'hover' compiler.\n"
        "Looked in: {}\n"
        "Pass compiler='/path/to/hover', set $HOVER, or build it with "
        "`go build -o hover .` in the repository root.".format(
            ", ".join(str(c) for c in candidates)
        )
    )


def output_path_for(source):
    """The library path a .hvr source builds to: the source without .hvr.

    `motor.hvr` -> `motor`. No platform extension is appended: the name is
    handed to the compiler's -o verbatim, and ctypes loads a library by
    path regardless of what it is called.
    """
    return Path(source).resolve().with_suffix("")


def is_source(path):
    """True if `path` looks like Hover source rather than a built library."""
    return Path(path).suffix.lower() == ".hvr"


def needs_rebuild(source, output):
    """True if `output` is missing or older than `source`.

    Only the entry file's timestamp is consulted -- a modified *imported*
    .hvr will not be noticed, because the import graph is only known to the
    compiler. Use rebuild="always" when editing libraries.
    """
    source, output = Path(source), Path(output)
    if not output.exists():
        return True
    return source.stat().st_mtime > output.stat().st_mtime


def compile_hvr(source, output=None, compiler=None, extra_args=()):
    """Compile a .hvr file into a hovercraft shared library.

    Returns the path to the built library. Raises CompileError with the
    compiler's own diagnostics on failure.
    """
    source = Path(source).resolve()
    if not source.is_file():
        raise FileNotFoundError("no such Hover source file: {}".format(source))

    output = Path(output).resolve() if output else output_path_for(source)
    hover = find_compiler(compiler)

    cmd = [str(hover), str(source), "--hovercraft", "-o", str(output)]
    cmd.extend(str(a) for a in extra_args)

    # cwd must be the compiler's own directory: the build step passes
    # `-I./runtime` and `./runtime/libhover_runtime.a` to zig, both
    # relative to the working directory, and drops sim.cpp there too.
    proc = subprocess.run(
        cmd,
        cwd=str(hover.parent),
        stdout=subprocess.PIPE,
        stderr=subprocess.STDOUT,
        text=True,
    )
    if proc.returncode != 0:
        raise CompileError(cmd, proc.returncode, proc.stdout)
    if not output.exists():
        raise CompileError(
            cmd, proc.returncode,
            "compiler reported success but produced no file at {}\n\n{}".format(
                output, proc.stdout),
        )
    return output


def resolve_library(source, rebuild="auto", compiler=None, output=None,
                    extra_args=()):
    """Turn a user-supplied path into a built library path.

    Accepts either Hover source (compiled as needed) or an already-built
    library (used as-is). Returns (library_path, was_compiled).
    """
    path = Path(source)
    if not is_source(path):
        if not path.exists():
            hint = ""
            if path.suffix == "":
                hint = ("\nIf you meant to compile a source file, pass the "
                        "path ending in .hvr.")
            raise FileNotFoundError(
                "no such hovercraft library: {}{}".format(path, hint))
        return path.resolve(), False

    out = Path(output).resolve() if output else output_path_for(path)
    if rebuild == "never":
        if not out.exists():
            raise FileNotFoundError(
                "rebuild='never' but no built library at {}".format(out))
        return out, False
    if rebuild == "always" or needs_rebuild(path, out):
        return compile_hvr(path, output=out, compiler=compiler,
                           extra_args=extra_args), True
    return out, False
