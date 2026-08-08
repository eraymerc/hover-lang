# Third-Party Licenses

Hover itself is MIT licensed — see [LICENSE](LICENSE).

Hover release packages contain third-party components. Every one of them is
under a permissive licence compatible with MIT; none is copyleft, and none
places any condition on code you write in Hover. What they do require is
attribution, which is what this file provides.

Each component's full licence text ships alongside its own files, at the path
given below.

---

## Eigen

**Licence:** MPL-2.0 (with some files under BSD and other MPL2-compatible
licences — see `COPYING.README`)
**Where:** `runtime/Eigen/`
**Licence text:** `runtime/Eigen/COPYING.MPL2`, `COPYING.BSD`,
`COPYING.APACHE`, `COPYING.MINPACK`, `COPYING.README`
**Upstream:** <https://eigen.tuxfamily.org/>

The C++ linear-algebra library behind the MNA solver in `runtime/mna/` and
`runtime/solvers/`. Shipped as source (headers) in every release package, so
recipients already have the covered source that MPL-2.0 requires be made
available.

---

## klauspost/compress

**Licence:** BSD-3-Clause (portions Apache-2.0 and Go's BSD licence; see the
licence file for the per-directory breakdown)
**Where:** `vendor/github.com/klauspost/compress/`
**Licence text:** `vendor/github.com/klauspost/compress/LICENSE`,
`vendor/github.com/klauspost/compress/internal/snapref/LICENSE`,
`vendor/github.com/klauspost/compress/zstd/internal/xxhash/LICENSE.txt`
**Upstream:** <https://github.com/klauspost/compress>

Provides the zstd decoder used by `hpm` to unpack `.tar.zst` package
archives. Only the `zstd` subpackage is compiled into the `hover` binary; the
Apache-2.0 portion of the module (`gzhttp/`) is not used. Vendored rather
than fetched at build time so the licence travels with the code and so the
18-platform cross-compile needs no network.

---

## Zig

**Licence:** MIT
**Where:** `toolchain/zig/` — **Windows release packages only**
**Licence text:** `toolchain/zig/LICENSE`
**Upstream:** <https://ziglang.org/>

The C++ compiler and linker Hover drives to build simulations. Windows
packages bundle a pinned Zig, because Windows has no package-manager
convention for installing one; Linux and BSD packages use the Zig already on
the user's PATH and bundle nothing.

---

## Go standard library

**Licence:** BSD-3-Clause
**Upstream:** <https://go.dev/LICENSE>

Statically linked into the `hover` binary, as with any Go program.
