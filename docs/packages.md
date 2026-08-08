# Hover Packages — A Practical Guide

How imports work, how `hpm` works, and how to make a package other people can
install.

This is the task-oriented guide. Two companions:

- [imports.md](imports.md) — the complete import reference (every form, every
  rule, every error).
- [package-manager-design.md](package-manager-design.md) — *why* the package
  manager is built this way, including the approaches that were tried and
  rejected.

---

## 1. The mental model

Three things, and keeping them straight makes everything else obvious:

| | what it is |
|---|---|
| **Directory** | The unit of *naming*. All `.hvr` files in one directory share a namespace. This is what an import names. |
| **Package** | The unit of *distribution*. A directory tree, published as one archive, identified by the hash of its contents. |
| **Index** | A list of *pointers*. Maps a short name to an archive URL and a hash. Stores no code. |

The thing that surprises people coming from other languages: **hover-lang.org
never stores your package.** It publishes a static index; the bytes always
come from wherever you put them. There is no account to create and nothing to
upload.

---

## 2. Imports

**An import names a directory, not a file.** Every `.hvr` file directly
inside it is imported together.

```hover
import <math>;                          // math.sin(x)
import <math> as m;                     // m.sin(x)
from <math> import sin;                 // sin(x)
from <math> import sin as wave, cos;    // wave(x), cos(x)
```

Whole-directory imports are **qualified**, always. Nothing an import brings
in can shadow anything you declared, so there is never a collision to resolve.
When the qualifier is noise — a math-dense analog body, say — `from ... import`
binds the names directly:

```hover
from <math> import exp, limexp;

analog module Diode<double Is, double n, double Vt>() [wire a, wire c] {
    double i = Is * (limexp(V(a,c) / (n * Vt)) - 1.0);
    // ...
}
```

The qualifier defaults to the last path segment, with hyphens turned into
underscores (`<hvr-rc>` binds `hvr_rc`). Use `as` to pick something else.

### Where a directory comes from

```hover
import "./devices";              // relative to THIS file
import <math>;                   // stdlib, or a package named "math"
import <semiconductors/bjt>;     // a subdirectory of either
import <myindex:vendor-parts>;   // a package from an index you added
```

For angle imports, **the first segment is a package name**, resolved
lockfile-first and bundled-`stdlib/`-second. There is no sigil
separating "package" from "standard library" — deliberately, because the
standard library is meant to become an installable package itself, and
`import <math>` should keep working either way.

### Three rules that follow

- **Siblings see each other.** Files in one directory need no imports between
  them. Splitting a package across files is never a breaking change.
- **Two files in one directory may not declare the same name.** There would
  be no way to say which one a reference meant.
- **Imports are not transitive.** If A imports B and B imports C, A does not
  see C. Every directory declares what it needs.

Full details, including every error message and a migration checklist from
the old file-based imports, are in [imports.md](imports.md).

---

## 3. Starting a project

```sh
mkdir my-circuit && cd my-circuit
hover hpm init
```

That writes a `hover.toml`:

```toml
[package]
name = "my-circuit"
version = "0.1.0"

[dependencies]
```

`hover.toml` is found by walking **up** from wherever you are, so every
command below works from anywhere inside the project. It is declarative —
hover never executes it.

### Adding dependencies

```sh
hover hpm install hvr-rc              # latest from the official index
hover hpm install hvr-rc@^0.1.0       # with a version requirement
hover hpm install https://example.com/parts-1.0.0.tar.gz
hover hpm install git@github.internal:eng/models.git --git --rev v0.3.0
```

Each writes the dependency into `hover.toml`, resolves it, downloads it, and
records the result in `hover.lock`. If anything fails, `hover.toml` is left
exactly as it was — a half-added dependency that can never install would
break the next person's build for a reason nobody chose.

The three forms in the manifest:

```toml
[dependencies]
# Indexed — the normal case. A short name plus a version requirement.
hvr-rc = "^0.1.0"

# Unindexed — one exact archive URL. For vendor or private packages that
# will never be in a public index.
[dependencies.vendor-parts]
url = "https://example.com/vendor-parts-0.3.1.tar.zst"

# Private — the opt-in git transport, for a repo needing credentials.
# This is the ONLY thing in hpm that requires git installed.
[dependencies.internal-models]
git = "git@github.internal:eng/models.git"
rev = "v0.3.0"
```

Version requirements: an exact version (`"1.2.0"`), `^1.2.0` (compatible),
`~1.2.0` (patch only), `>=1.2.0`, or `*`. Anything else is rejected by name
rather than silently reinterpreted.

### Then use it

```hover
import <hvr-rc>;
from <hvr-rc> import Divider;

module main<>() [] {
    module f = hvr_rc.RCLowPass<1k, 100n>() [vin, vout];
    module d = Divider<10k, 10k>() [vin, tap];
}
```

```sh
hover main.hvr
```

**Compiling never installs anything.** Package paths resolve through
`hover.lock` only. A compile that reached for the network because a manifest
changed is how a build works on one machine and not another.

### Commit the lockfile

`hover.lock` is generated but **belongs in version control**:

```toml
# Generated by `hover hpm`. Do not edit by hand.
# Commit this file: it is what makes a build reproducible.
version = "1"

[[package]]
name = "hvr-rc"
version = "0.1.0"
index = "official"
url = "https://github.com/you/hvr-rc/archive/refs/tags/v0.1.0.tar.gz"
hash = "sha256:183d452a336d96eef4bb13c212b313f09e915d543a8d14b3fbbe93892219e8b0"
```

It is regenerated whole and sorted by name, so two machines resolving the
same dependencies produce byte-identical lockfiles and its diffs stay
readable.

In CI, use `--locked`: a dependency that would have drifted fails the build
instead of quietly changing it.

```sh
hover hpm install --locked
```

---

## 4. `hpm` command reference

Everything lives under `hover hpm`. Releases also ship an `hpm` symlink to
the same binary (`hpm.bat` on Windows), so `hpm install foo` and
`hover hpm install foo` are the same words in the same order.

| Command | What it does |
|---|---|
| `hpm init [name]` | Create a `hover.toml`. |
| `hpm install` | Restore from manifest + lockfile. No resolution, no index sync, no network if everything is cached. |
| `hpm install <pkg>` | Add an indexed package. `pkg@^1.2` pins a requirement. |
| `hpm install <url>` | Add an unindexed package by archive URL. |
| `hpm install <repo> --git` | Add via the optional git transport. |
| `hpm update [pkg...]` | Sync indexes, move to newer versions, rewrite the lockfile. |
| `hpm remove <pkg>` | Drop a dependency, then re-resolve so anything it alone pulled in goes too. |
| `hpm list` | Direct dependencies, then what they pulled in. |
| `hpm verify` | Re-check every locked package (see below). |
| `hpm index add <url>` | Trust an additional index. Prompts. |
| `hpm index list` | Configured indexes and how stale each is. |
| `hpm index remove <name>` | Stop using an index. |
| `hpm clean` | Delete cached packages this project no longer references. |

Flags:

| Flag | Meaning |
|---|---|
| `--offline` | Never touch the network; fail if something is missing. |
| `--locked` | Fail rather than change `hover.lock`. Use in CI. |
| `--name <n>` | With `install <url>`, the name to install it as. |
| `--git` | With `install <url>`, use the git transport. |
| `--rev <ref>` | With `--git`, the tag/branch/commit. |
| `--manifest <p>` | Use this `hover.toml` instead of searching upward. |
| `-j <n>` | Maximum simultaneous downloads. |

### `verify`

```sh
hover hpm verify
```

Three checks per package, which is why it is its own command rather than a
flag on install (install must stay fast and offline-capable; verify
deliberately reaches the network for everything):

1. The archive is still reachable — catches link rot.
2. It still hashes to the locked value — catches a re-uploaded release asset
   or tampering.
3. The **cached copy** hasn't been modified since install.

```
  FAIL  hvr-rc    UPSTREAM CHANGED
      expected sha256:183d452a...
      actual   sha256:14c894fb...
```

A hash mismatch during a normal install is a hard failure, not a warning —
"the code you are about to compile is not the code that was reviewed" has no
sensible continue-anyway path.

---

## 5. Creating a package

A package is a directory of `.hvr` files. That is the entire requirement —
no build step, no compiled artifacts, no per-platform variants.

```
hvr-rc/
    filters.hvr
    dividers.hvr
    hover.toml      # optional
```

There is a complete, runnable version of this at
[../examples/package/](../examples/package/).

### Write the source

```hover
// hvr-rc/filters.hvr
analog module RCLowPass<double r, double c>() [wire in, wire out] {
    R<r>() [in, out];
    C<c>() [out, gnd];
}

func double cutoff(double r, double c) {
    return 1.0 / (2.0 * 3.14159265359 * r * c);
}
```

```hover
// hvr-rc/dividers.hvr — no import of filters.hvr needed
analog module FilteredDivider<double r_top, double r_bottom, double c>()
    [wire in, wire out]
{
    wire mid;
    module div = Divider<r_top, r_bottom>() [in, mid];
    module lpf = RCLowPass<r_bottom, c>() [mid, out];   // sibling, just works
}
```

Split across as many files as reads well. Consumers write one
`import <hvr-rc>;` and see all of it; they never learn your file layout, and
adding a file later is not a breaking change.

Two things to get right:

- **Declare your own dependencies.** Imports are not transitive, so if your
  code calls `exp`, your directory says `from <math> import exp;` — even
  though every plausible consumer already imported math. Relying on the
  consumer means your package compiles or not depending on what *they* wrote.
- **Don't reuse a name across files in the same directory.** It is an error,
  reported with both source locations.

### The optional manifest

Only needed if your package has dependencies of its own:

```toml
[package]
name = "hvr-rc"
version = "0.1.0"

[dependencies]
some-other-package = "^2.0"
```

A transitive dependency may name the official index or a URL, but **not an
added index** — that would let your package pull in an index the consuming
project never agreed to trust.

### Test it locally before publishing

You need no server and no index:

```sh
tar -czf /tmp/hvr-rc-0.1.0.tar.gz hvr-rc
cd /tmp && python3 -m http.server 8000 &

cd my-consumer-project
HOVER_ALLOW_INSECURE_HTTP=1 \
  hover hpm install http://127.0.0.1:8000/hvr-rc-0.1.0.tar.gz
hover main.hvr
```

`HOVER_ALLOW_INSECURE_HTTP=1` exists for exactly this. Real URLs must be
`https`, because for a first install there is no recorded hash yet — which is
precisely when the content is most trusted.

---

## 6. Publishing

### Step 1 — tag a release

GitHub, GitLab and Codeberg all generate a `.tar.gz` for every tag at a
stable URL. That URL is a complete, working package. Anyone can depend on it
immediately:

```toml
[dependencies.hvr-rc]
url = "https://github.com/you/hvr-rc/archive/refs/tags/v0.1.0.tar.gz"
```

Nothing was uploaded to anyone. This step alone is a published package.

Accepted formats are `.tar.gz` and `.tar.zst`. Not `.zip` — `.tar.gz` is
always available from the same hosts, and a second extractor is more
attack surface for no gain.

### Step 2 — get a short name (optional)

To let people write `hvr-rc = "^0.1.0"`, submit one TOML file to the index:

```toml
# packages/hvr-rc.toml
name = "hvr-rc"
description = "Passive RC filter building blocks"
repository = "https://github.com/you/hvr-rc"

[[version]]
version = "0.1.0"
url = "https://github.com/you/hvr-rc/archive/refs/tags/v0.1.0.tar.gz"
hash = "sha256:183d452a336d96eef4bb13c212b313f09e915d543a8d14b3fbbe93892219e8b0"
```

Get the hash by installing it once by URL and reading `hover.lock`.

Publishing a new version appends a `[[version]]` block. To retire one, set
`yanked = true` rather than deleting it — a yanked version is skipped for new
resolutions but still resolves for anyone who already locked it. Deleting a
version outright is npm's unpublish, which is how left-pad broke thousands of
builds.

> The official index does not exist yet. Until it is created and publishing
> an `index.tar.gz`, URL and git dependencies are the way to share packages.

---

## 7. Indexes

An index is an archive containing a `packages/` directory of those TOML
files, downloaded and unpacked into `~/.hover/index/<name>/`. Once synced,
resolution works with the network down — an index outage costs you discovery
of *new* packages, not your build.

There is a complete, runnable one at [../examples/index/](../examples/index/),
including a script that builds the archive and a walkthrough that installs
through it with no network.

```sh
hover hpm index add https://example.com/my-index.tar.gz --name myindex
hover hpm index list
hover hpm index remove myindex
```

`index add` is **the only command in hpm that prompts**, because it is a real
trust decision — you are agreeing to accept code it points at. Nothing else
prompts, since `install` runs in Dockerfiles and CI where a prompt either
hangs or forces `--yes` into every invocation.

With no terminal attached it declines. To add an index unattended, write the
block into `hover.toml` yourself, where it shows up in a diff:

```toml
[[index]]
name = "myindex"
url = "https://example.com/my-index.tar.gz"
```

**Names from an added index are always qualified.** `myindex:vendor-parts` in
the manifest, `<myindex:vendor-parts>` in an import. An added index can
therefore never shadow an official package name — the 2021 dependency-
confusion attack class simply has nowhere to land here.

---

## 8. Files and environment

| Path | What |
|---|---|
| `hover.toml` | Manifest. Yours, hand-edited, committed. `hpm` edits it line by line, so your comments and ordering survive. |
| `hover.lock` | Generated, committed. Pins version, URL and content hash. |
| `~/.hover/index/<name>/` | Unpacked index archives. |
| `~/.hover/hpm/<hash>/` | Content-addressed package cache, shared across every project on the machine. |

The cache is user-scoped, unlike `stdlib/`, which lives next to the
hover binary. Dependencies must never land in a possibly root-owned install
directory.

| Variable | Effect |
|---|---|
| `HOVER_HOME` | Relocate `~/.hover`. |
| `HOVER_INDEX_URL` | Override the official index — for testing and air-gapped mirrors. |
| `HOVER_ALLOW_INSECURE_HTTP=1` | Permit plain `http`. For a local test server only. |

---

## 9. When something goes wrong

**`package "hvr-rc" is not installed`** — you imported it but never installed
it, or you cloned a project and haven't run `hover hpm install` yet. If you
meant a standard library directory, check the spelling.

**`an import names a directory, not a file`** — the old `<math/math.hvr>`
spelling. The message names the replacement. See the migration checklist in
[imports.md](imports.md).

**`undeclared module 'Diode'`** — whole-directory imports are qualified.
Either write `semiconductors.Diode`, or `from <semiconductors> import Diode;`.
The error lists everything currently visible.

**`module 'X' is declared twice in <dir>`** — two files in one directory
declare the same name. They share a namespace, so one has to change.

**`this file's own directory`** — a leftover import of a sibling. Delete it;
the names are already there.

**`content ... does not match the expected hash`** — upstream changed after
the version was recorded. Nothing was installed. If the change is legitimate,
`hover hpm update` re-resolves and re-records it.

**`--locked was given but ... needs re-resolving`** — the manifest and
lockfile disagree. Run `hover hpm update <pkg>` locally and commit the
lockfile.

**`would be imported as '3d_parts', which starts with a digit`** — the
directory name doesn't make a legal identifier. Add `as <name>`.
