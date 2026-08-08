# Imports in Hover

**An import names a directory, not a file.** Every `.hvr` file directly
inside it is imported together, as one unit sharing one namespace — the same
rule Go uses for packages and Python uses for packages.

Two things to choose independently:

- **Which directory** — the standard library, a path relative to the
  importing file, or an installed package.
- **How the names arrive** — behind a qualifier (the default), or bound
  directly.

## The four forms

```hover
import <math>;                          // math.sin(x)
import <math> as m;                     // m.sin(x)
from <math> import sin;                 // sin(x)
from <math> import sin as wave, cos;    // wave(x), cos(x)
```

Every form works with every path style below.

### 1. Whole directory, qualified

```hover
import <semiconductors>;

module main<>() [] {
    module d = semiconductors.Diode<1p, 1.0, 0.026>() [a, b];
}
```

The default, and always qualified. The binding name is the **last path
segment** — `import <semiconductors/bjt>;` binds `bjt` — so `bjt.NPN` reads
naturally without anyone having to name it.

Qualified-by-default is deliberate. Merging into a flat namespace would mean
adding a declaration to a library could break its consumers, and a reader
could not tell where a name came from without checking every import in the
file. Nothing an import brings in can shadow anything, so there is no
collision to resolve.

Hyphens become underscores, since a package directory may be called anything:
`import <hvr-rc>;` binds `hvr_rc`. If the automatic name is ugly or illegal,
`as` overrides it — and a name that could never be an identifier is an error
naming the import, not a syntax error at the first use:

```
line 1: "./3d-parts" would be imported as '3d_parts', which starts with a
digit — add `as <name>`
```

### 2. Whole directory, renamed

```hover
import <semiconductors> as devices;

module main<>() [] {
    module d = devices.Diode<1p, 1.0, 0.026>() [a, b];
}
```

Qualifiers are **per-file and one level deep**. Your name for a directory is
yours alone — another file importing the same directory under a different
name never interacts with it — and there is no `A.B.Thing`, because imports
are not transitive.

### 3 & 4. Specific declarations

```hover
from <semiconductors> import Diode;
from <math> import sin as wave, cos;
```

Only the listed names become visible, under their local spelling, with no
qualifier. This is the escape hatch that keeps math-dense analog bodies
readable: one line at the top and `sin(x)` reads exactly as it always did.

Unlike a whole-directory import, these names *do* land in your flat
namespace, so they can collide — with each other or with your own
declarations. That is an error, and `as` resolves it by renaming just the one
name that clashes.

A selected name may be a module or a function. If the directory declares
neither, you are told what it does declare:

```
line 5: <semiconductors> declares no module or function named 'Dioed'
— it declares: Diode
```

Note the two meanings of `as`, which never mix:

| | meaning |
|---|---|
| `import <d> as N;` | qualify the **whole directory** as `N` |
| `from <d> import X as N;` | rename the **one declaration** `X` to `N` |

### `from` is not a reserved word

It starts an import only when a path follows it, so `from` remains usable as
an identifier:

```hover
extern func double nextafter(double from, double to);   // still fine
```

That declaration is in the standard library today, and naming the terminals
of a two-port `from`/`to` is natural enough that reserving the word was not
worth it.

## Which directory

### Relative — `"./quoted"`

```hover
import "./devices";
import "../shared/rails";
```

Resolved relative to the directory of the file **that wrote the import** —
not the entry file, and not the current directory.

### Angle brackets — `<name/path>`

**The first segment is a package name.**

```hover
import <math>;                        // "math" — the standard library
import <hvr-rc>;                      // "hvr-rc" — an installed package
import <semiconductors/bjt>;          // a subdirectory of the stdlib
import <myindex:vendor-parts>;        // from an index you added
```

There is no `@` or other marker separating a package from the standard
library, and that is the point: **the standard library is meant to become an
ordinary installable package too.** `import <math>` should keep working
whether `math` is the copy bundled next to the binary or a version pinned in
your lockfile.

Resolution order:

1. **The project's lockfile.** If `hover.lock` has a package by that name, it
   wins.
2. **The bundled `stdlib/`**, in the directory next to the hover
   executable — not the current directory, which is what makes
   `hover foo.hvr` work from anywhere once hover is on `PATH`.

So installing a package named `math` overrides the bundled one. That is how
you pin a standard-library version, and it is auditable rather than silent —
everything consulted comes from a committed `hover.lock`.

> **Worth knowing:** a *transitive* dependency named `math` also lands in
> that lockfile and would override it project-wide. It is visible in
> `hover.lock` and `hover hpm list`, but it is a real reason to look at what
> a new dependency drags in.

An **index-qualified** name (`myindex:vendor-parts`) is only ever a package —
it never falls back to the standard library, so an added index cannot satisfy
an import meant for stdlib. It is also a *different package* from an
unqualified one with the same name: the qualifier is part of the identity,
which is why a third-party index can never shadow an official name.

**Compiling never installs anything.** If a package is named in source but
missing:

```
in /path/to/main.hvr, line 4: package "hvr-rc" is not installed —
run `hover hpm install hvr-rc` (or check the path, if you meant a standard
library directory)
```

See [package-manager-design.md](package-manager-design.md) for how packages
get there, and [../examples/package/](../examples/package/) for one you can
build and install.

## Files in one directory share a namespace

This is what makes an import name a directory. Sibling files see each other's
declarations **without importing anything**:

```hover
// hvr-rc/filters.hvr
analog module RCLowPass<double r, double c>() [wire in, wire out] { ... }
```

```hover
// hvr-rc/dividers.hvr — no import of filters.hvr
analog module FilteredDivider<...>() [wire in, wire out] {
    module lpf = RCLowPass<r_bottom, c>() [mid, out];   // just works
}
```

A consumer writes one `import <hvr-rc>;` and sees both. How the author split
their package across files is not something consumers know or should care
about.

The flip side: **two files in one directory may not declare the same name.**
There would be no way to say which one a reference meant, so it is an error
with both source locations:

```
module 'Twice' is declared twice in /path/to/dup — x.hvr line 1 and
y.hvr line 1. Files in one directory share a namespace, so the two names
have to differ
```

Importing your own directory is likewise an error, not a cycle — the names
are already there:

```
line 1: "./" is this file's own directory — files in the same directory
already share a namespace, so remove the import
```

**Subdirectories are separate units.** `import <semiconductors>;` gives you
`diode.hvr` and nothing from `semiconductors/bjt/`; that needs its own
`import <semiconductors/bjt>;`. Recursing would make one import silently pull
in an entire tree.

**The entry file is not merged with its siblings.** You compile a *file*, and
that file is its own scope — so a directory of independent testbenches, each
declaring `main`, keeps working.

## Imports are not transitive

If `A` imports `B` and `B` imports `C`, then `A` sees `B`'s own declarations
and **not** `C`'s. This is Go's rule, and it means adding an import to a
library never silently changes what its consumers can name.

The practical consequence: **every directory declares what it needs.** A
library that calls `exp` names it, even if every plausible consumer already
has:

```hover
// stdlib/semiconductors/diode.hvr
from <math> import exp;    // needed here, regardless of the consumer

analog module Diode<double Is, double n, double Vt>() [wire a, wire c] { ... }
```

Loading is still transitive — the compiler reads `C` because it must — but
*visibility* is not.

### Cycles are an error

Directory `a` importing directory `b` importing directory `a` is reported
rather than silently tolerated, because there is no valid non-transitive
reading of it:

```
in a.hvr, line 2: import cycle detected: a.hvr -> b.hvr -> a.hvr
```

Note this is a *source* cycle. A module instantiating itself through other
modules is a separate check in the elaborator, and can happen with no imports
at all.

## `importc` is a different thing

```hover
importc "<cmath>";
importc "wrappers.hpp";
```

`importc` is not a Hover import. It injects a `#include` into the generated
C++ and is how `extern func` declarations get their definitions. The target
is never parsed as Hover. Quoted headers resolve relative to the entry
`.hvr` file's directory, and a sibling `.cpp`/`.c`/`.cc`/`.cxx` next to the
header is linked automatically.

## Quick reference

| Written | Means |
|---|---|
| `import <a>;` | package `a` if installed, else stdlib; as `a.x` |
| `import <a> as N;` | same directory, as `N.x` |
| `import <a/b>;` | subdirectory `b`, as `b.x` |
| `import <idx:a>;` | package `a` from index `idx` — never stdlib |
| `import "./b";` | directory `b` next to this file, as `b.x` |
| `from <a> import X;` | just `X`, unqualified |
| `from <a> import X as Y;` | just `X`, called `Y` |
| `from "./b" import X, Y;` | just `X` and `Y` |
| `importc "<cmath>";` | C++ `#include`, not a Hover import |

## Migrating from file imports

Import paths used to name files. The compiler tells you the replacement:

```
line 1: an import names a directory, not a file — write <math> instead of
<math/math.hvr> (every .hvr file in that directory is imported together)
```

Mechanically:

1. Drop the filename: `<math/math.hvr>` → `<math>`.
2. Two files from one directory collapse into one import —
   `<optoelectronics/leds/led.hvr>` plus `<optoelectronics/leds/led_colors.hvr>`
   becomes `<optoelectronics/leds>`.
3. An import of a **sibling** file disappears entirely.
4. A previously bare import is now qualified. Either qualify the uses
   (`Diode` → `semiconductors.Diode`) or name what you need
   (`from <semiconductors> import Diode;`).
5. `import <x/y.hvr> as N;` → `import <x> as N;`, and existing `N.Thing`
   references are unchanged.
