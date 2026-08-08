# Hover Package Manager — Design

Status: **implemented** in [`hpm/`](../hpm/), wired into the compiler through
`loader.SetPackageRoots` and `hover hpm` / the `hpm` symlink. This records
decisions and the reasoning behind them so they don't have to be re-derived,
including the ones that were reversed.

## Summary

Hover gets a package manager, but **not a package registry**. Those are
separable, and the registry is the expensive half.

- The index is a **static archive**, downloaded and owned locally by each
  user.
- Index entries are **archive URLs plus content hashes** — the actual package
  bytes are served by upstream (GitHub etc.), never by hover-lang.org.
- Resolution is **per-project**, recorded in a committed lockfile.
- Users may add **additional indexes**; names from them are qualified, so a
  third-party index can never shadow an official package.

hover-lang.org therefore serves a file. It runs no service, stores no user
code, owns no accounts, and has no uptime obligation on anybody's build.

### Reversed: git is not the transport

An earlier draft of this document fetched both the index and every package by
cloning git repositories. That was wrong, and the reasoning that killed it is
worth keeping:

1. **It made git a runtime dependency for end users.** Hover's distribution
   story is unzip-and-run — Windows packages bundle Zig precisely so nothing
   else has to be installed. "Unzip and run, except the package manager needs
   git" is a real regression.
2. **The hash already does the pinning.** A commit sha and a content hash are
   both immutable names for a tree, and the content hash is the one that must
   be computed anyway. Recording both was two answers to one question, with
   nothing deciding which wins if they disagreed. The `commit` field is gone
   from both the index schema and the lockfile.
3. **Host-specific URL shapes become data, not logic.** With an archive URL
   in the index entry, GitHub, GitLab, a corporate Gitea and a plain file on
   a web server are indistinguishable to the client.
4. **It contradicted this document's own stated precedent** — "Hover already
   depends on Zig, whose URL-plus-hash scheme we can copy directly". Zig's
   scheme has no git in it.

crates.io made the same move in 2022, abandoning its git index for HTTP once
git sync became the bottleneck. The index can still *be* a public git repo
that people review pull requests against; only its distribution is HTTP.

Git survives as an **opt-in transport for a single dependency**
(`git = "..."` in the manifest), where the actual value is credential
handling for private repositories. A user with no git installed can still
install everything else.

The cost accepted in exchange: we now own the archive extractor, and the
traversal/symlink/bomb checks git gave us for free. That is
[`hpm/archive.go`](../hpm/archive.go), and it is the most security-relevant
code in the feature.

### Archive formats

`.tar.gz` and `.tar.zst`. Not `.zip` — `.tar.gz` is always available from the
same hosts, and a second extractor has its own sharp edges (zip-slip, central
directory vs. local header mismatch) for no gain.

`.tar.zst` is **not** there for speed. At Hover's package sizes — pure `.hvr`
text, kilobytes to a few hundred KB — zstd saves perhaps 20 KB and a
millisecond over gzip, against connection setup that dominates by orders of
magnitude. It is there so hover binaries shipped today can read packages
published years from now: adding an archive format after an ecosystem exists
strands every older binary, exactly like adding a field to the index schema
late. Install speed comes from parallel fetching over a shared HTTP
transport, not from the codec.

## Why not a registry

A "package manager" is really three components, and languages differ mainly
in how many they chose to own:

| | Index (owns names) | Host (stores bytes) | Client |
|---|---|---|---|
| Python | PyPI | PyPI | pip |
| Rust | crates.io | crates.io | cargo |
| npm | registry.npmjs.org | same | npm |
| Go | *nobody* — import path is the URL | origin repo (+ optional proxy) | `go` |
| Zig | *nobody* | origin URL, content-addressed | `zig fetch` |
| **Hover** | **static index of pointers** | **origin URL, content-addressed** | **`hpm`** |

Owning all three buys `install foo` UX and costs, permanently: name-squatting
disputes, moderation and takedowns, account-compromise supply-chain attacks,
bandwidth, and an uptime obligation that can never be retired. PyPI runs on
donated CDN capacity. npm's left-pad incident happened *because* npm owned
unpublish.

Three reasons the decentralized model fits Hover specifically:

1. **Hover already depends on Zig**, whose users expect exactly this model,
   and whose URL-plus-hash scheme we can copy directly.
2. **Hover packages are pure `.hvr` source.** No build artifacts, no ABI, no
   per-platform variants — the runtime is the only compiled thing and it
   ships with the compiler. Installing is fetch, verify, record. The client
   is cheap; only the registry would have been expensive.
3. **The domain cuts against central hosting.** Hover packages will largely
   be component and device models, and many valuable ones are vendor SPICE
   models with redistribution terms. A URL-based scheme leaves that liability
   where it belongs.

## Command surface

All package commands live under a single `hpm` group, keeping them namespaced
so `install` never competes with future top-level verbs.

| Command | Behavior |
|---|---|
| `hover hpm init [name]` | Create a `hover.toml`. |
| `hover hpm install` | Restore from manifest + lockfile. No resolution, no index sync. |
| `hover hpm install foo` | Sync index, resolve, add to manifest, install, update lockfile. `foo@^1.2` pins a requirement. |
| `hover hpm install <archive-url>` | Same, unindexed. Escape hatch for private/vendor packages. |
| `hover hpm install <repo> --git` | Opt-in git transport, for private repos needing credentials. |
| `hover hpm update [foo]` | Sync, move to newer versions, rewrite lockfile. |
| `hover hpm remove foo` | Drop a dependency, then re-resolve so its orphans go too. |
| `hover hpm list` | Show direct dependencies, then what they pulled in. |
| `hover hpm index add <url>` | Add an index source. Prompts — a real trust decision. |
| `hover hpm index list` | Show configured indexes and how stale each is. |
| `hover hpm index remove <name>` | Remove by name (nobody remembers the URL). |
| `hover hpm verify` | Check every locked dependency still fetches and still hash-matches. |
| `hover hpm clean` | Drop cached packages this project no longer references. |

Flags: `--offline`, `--locked` (CI), `--name`, `--git`, `--rev`,
`--manifest`, `-j <n>`.

Notes on the shape:

- **`hover hpm install` with no argument is the "install from dependency
  file" command.** Same verb, no argument — matching `npm install`, `cargo
  fetch`, `go mod download`. It takes no filename: the manifest is discovered
  by walking up from cwd (`--manifest` overrides), so it works from anywhere
  in a project.
- **Subcommands, not flags,** for `index`. `list` and `remove` are actions,
  not modifiers; leaving flags free for actual modifiers.
- **`verify` is its own command,** not a flag on install. It does two related
  checks — is the URL still reachable (link rot), does the content still
  hash-match (re-uploaded release asset, tamper) — plus a third the local
  cache makes possible: has the *cached copy* been modified since it was
  installed. Primarily a CI and maintainer tool, which is why it is not on
  the install path: install must stay fast and offline-capable, verify
  deliberately reaches the network for every dependency.
- **A failed `install <thing>` restores the manifest.** Adding a dependency
  and installing it is one action to the user, so a failure must not leave
  `hover.toml` naming something that can never be installed — the next plain
  `hpm install`, in CI or on a colleague's machine, would then fail for a
  reason nobody chose.
- `hover <file>.hvr` remains the primary positional form, so no subcommand
  may ever be named like a file. The `.hvr` extension disambiguates.

### Naming: `hpm` as a subcommand group, plus a symlink

Package commands are grouped under `hpm` rather than sitting at top level.
The group name is redundant when spelled in full (`hover hpm ...` — the `h`
is the `hover` already typed), which is the cost paid for keeping the package
namespace separate from the compiler's own verbs.

That cost is recovered by shipping `hpm` as a **symlink to the `hover`
binary**, dispatching on `argv[0]`, so both spellings are the same words in
the same order:

```
hover hpm install foo
hpm install foo            # identical
```

This is why `hpm` is a better group name than `pkg` would have been: `pkg`
could not also serve as the standalone entry point without introducing a
second vocabulary.

A **separate `hpm` binary** was considered and rejected. Every
separate-binary design predates 2010 (`pip`, `gem`, `npm`); every modern one
is unified (`go`, `zig`, `deno`, `bun`), and Rust only looks like a
counterexample since users treat `cargo` as *the* tool and `rustc` as an
implementation detail. The specific hazard is pip's: separate binaries create
a **version-pairing problem** ("which `hover` does this `hpm` install
for?"), which Python never fully solved — hence `python -m pip` as the
recommended invocation. It would bite harder here, because Hover resolves
resources relative to its own executable (`loader.ExeDir()`); a standalone
binary would have to independently locate the matching hover install just to
find the stdlib, runtime and cache conventions.

The symlink avoids that entirely: one binary across all 18 platforms, and it
already works with resource resolution, since `ExeDir()` calls
`EvalSymlinks` and so resolves back to the real hover directory.

## Files and layout

- **`hover.toml`** — manifest. Declarative only. Holds dependencies *and* the
  index list, so adding an index appears in a reviewable diff rather than
  mutating hidden global state.
- **`hover.lock`** — generated, committed. Pins version, archive URL, content
  hash and originating index per dependency. Regenerated whole and sorted by
  name, so two machines resolving the same dependencies produce byte-identical
  lockfiles and `git diff hover.lock` stays readable.
- **`~/.hover/index/<name>/`** — unpacked index archives.
- **`~/.hover/hpm/<hash>/`** — content-addressed package cache, shared across
  projects. Not vendored per-project; a `hover vendor` command can come later
  if air-gapped builds need it.

`HOVER_HOME` relocates all of it; `HOVER_INDEX_URL` overrides the official
index (for testing and air-gapped mirrors — replacing the official index for
real should be a named `[[index]]`, so it stays qualified and visible).

The cache is deliberately **user-scoped, unlike `stdlib`, which is
executable-relative.** Dependencies must not land in a potentially root-owned
install directory — the same sharp edge `hover --setup` already has.

The manifest is edited **line by line, not re-serialized.** Comments, blank
lines and key order survive an `hpm install`; a file people maintain by hand
must not be reformatted by the tool that reads it. The TOML subset that makes
this possible is deliberately tiny ([`hpm/toml.go`](../hpm/toml.go)) — no
inline tables, no arrays, no dotted keys — and hand-written rather than
pulled from a dependency, because a package manager is a bad place to start
growing a dependency tree.

### The manifest must not be executable

`setup.hvr_lib` as a runnable Hover file was considered and rejected. That is
`setup.py`, which Python has spent a decade migrating away from (to
`pyproject.toml`) because an executable manifest means installing a package
executes arbitrary code before anyone has read a line of it, and means you
cannot learn anything about a package without running it.

A `.toml` name is preferred over a `.hvr_lib` extension for the same reason:
the extension should not suggest the file is Hover code.

## Behavior decisions

### Index entries pin archives and hashes

Mapping `name → repo URL` alone is the design PyPI ran until ~2013 and
abandoned *because it did not work* — repos deleted, tags moved, force
pushes, branch renames, constant breakage. Homebrew survives the same model
only because formulae carry a version and checksum plus a large maintainer
team fixing rot quickly.

So the mapping is `name → {version → {url, hash}}`, where the URL names one
exact archive. Content is verified on fetch; the result is written to the
lockfile. This converts "upstream changed under me" from a silent wrong-code
bug into a clean error, and it is what makes the same package fetched over
HTTP and over the git transport hash identically.

Index entries also carry `signature` / `signed_by` fields that nothing checks
yet, and a `yanked` flag. Yanking is crates.io's model: a yanked version is
skipped for new resolutions but still resolves for anyone who already locked
it. Deleting a version outright is npm's unpublish, which is how left-pad
broke thousands of builds.

It also preserves the upgrade path: if link rot starts hurting, a caching
mirror can be added later (the `proxy.golang.org` move) as a pure
availability improvement that breaks nothing — but only because everything
was content-addressed from the start.

### Sync on install, fall back to cache on failure

`hover hpm install foo` refreshes the index first, and on network failure warns
and proceeds against the local copy. Refusing to install because a refresh
failed would discard the entire reason for keeping the index local.

Sync is skipped entirely when the lockfile fully determines resolution (a
plain `hover hpm install` restoring a checked-out project) — that case needs no
network at all. `hover hpm update` always syncs.

Explicit-sync (pacman's `-Sy`) was considered and rejected: its friction is
pacman's most common new-user complaint, where `pacman -S foo` reports
"target not found" for a package that plainly exists, with nothing in the
error indicating a stale db.

Flags: `--offline` forces cache-only; `--locked` fails rather than drifting
(for CI).

If sync latency ever becomes noticeable, the cheap mitigation is Homebrew's:
refresh at most once every N hours rather than on every command.

### Collisions resolve by qualification, not priority

Ordered source lists where the first match wins — apt's `sources.list`,
pacman's repo order — have a specific exploited failure mode: **dependency
confusion**. A low-trust index defines a name, sorts ahead of the official
one, and silently shadows it with no error and no signal. This is the 2021
attack class that landed working payloads inside Apple, Microsoft and dozens
of others via internal package names.

Therefore: unqualified names resolve **only** against the official index.
Anything from an added index is reachable only as `indexname/package`. No
ordering, no shadowing, no config-order-dependent builds.

### Prompts

Only `hover hpm index add` prompts. That is a genuine, consequential decision
about whose code you will accept.

Nothing else does — notably not "the index may be stale, update? [Y/n]".
`hover hpm install` will run from Dockerfiles, CI jobs and Makefiles; with no TTY
a prompt either hangs or forces `--yes` into every invocation (why `apt-get`
needed both `-y` and `DEBIAN_FRONTEND=noninteractive`). This mirrors the
reasoning already recorded in `zigfetch.go` for not downloading Zig inline.

A prompt whose answer is always yes does not inform anyone — it trains people
to hit enter unread, spending attention that should be reserved for the
prompts that matter. Prompts earn their place when an action is expensive,
irreversible, or a trust decision; an index sync is none of the three.

The alternative to interactivity is **transparency** — say what was done
rather than asking permission:

```
Syncing index... 3 new packages
Installing foo 1.2.0 from github.com/user/foo
```

## Comparison with pacman

The structural difference driving everything else: **pacman manages one
global system state; this design manages per-project dependency sets.** A
distro can ship exactly one version of a library because it curates the whole
set to be co-installable. A language package manager cannot — two projects on
one machine will want different versions. That single fact is why there is a
lockfile, and why config is a per-project `hover.toml` rather than a
root-owned `/etc/pacman.conf`.

| | pacman | Hover |
|---|---|---|
| Index location | `/var/lib/pacman/sync/`, system-wide | `~/.hover/index/`, user-scoped |
| Source config | `/etc/pacman.conf`, global, root | `hover.toml`, per-project, in diffs |
| Sync | Explicit `-Sy` | Implicit on install, cache fallback |
| Distributes | Prebuilt binaries from its own mirrors | Source from upstream repos |
| Integrity | PGP signatures | Content hashes |
| Collisions | Repo priority → silent shadowing | Qualified names |
| State | One global system | Per-project + lockfile |

This design takes pacman's best property — the **locally-owned database**, so
the network is never on the critical path — and rejects its repo-priority
resolution.

The closer analogue is **AUR plus a helper**: an index of pointers to
upstream sources, fetched locally, community-submitted, trust established by
review rather than by hosting.

### Where pacman is genuinely stronger

**Signatures vs hashes.** A hash proves "these are the same bytes as when
indexed." A signature proves "the right person made this." Hash-pinning gives
integrity and reproducibility but **not authenticity**: if an upstream GitHub
account is compromised and the attacker publishes a new tag, the hash for
that version is simply whatever they pushed. The trust anchor here is index
PR review plus upstream account security — roughly AUR's level, and AUR's
"read the PKGBUILD first" culture exists for exactly this reason. Signing is
a plausible later addition; **leave room for it in the index schema now.**

**Mirrors vs upstream.** Pacman's volunteer mirror network keeps a package
available even if upstream vanishes. This design accepts link-rot exposure
that pacman does not have, mitigated but not eliminated by hashes making a
later caching mirror a drop-in addition.

### Not copied

Pacman's flag composition (`-Syu`, `-Rns`, `-Qdt`) is excellent for experts
and opaque to newcomers. Verb subcommands are the right call for a language
tool.

## The import namespace

Everything above is tooling. The **only language change** is the import
namespace, and it is the piece that becomes expensive to revise once
third-party Hover code exists.

Full details in [imports.md](imports.md); what matters here is the shape.

**An import names a directory, not a file** — every `.hvr` file inside it is
one unit sharing one namespace, as in Go and Python. Sibling files see each
other without importing, and how an author split a package across files is
invisible to consumers.

```
import <math>;                       whole directory, as math.x
import <math> as m;                  ... as m.x
import "./local";                    relative to the importing file
from <math> import sin;              just sin, unqualified
from <math> import sin as wave, cos; just those, renamed
```

Whole-directory imports are **qualified by default**. Merging into a flat
namespace would mean adding a declaration to a library could break its
consumers, and a reader could not tell where a name came from without
checking every import in the file. The cost is noise in math-dense analog
bodies, which is exactly what the `from ... import` form buys back — one line
at the top and `sin(x)` reads as it always did.

### Reversed: no sigil for packages

An earlier draft marked packages with `@`:

```
import <@foo/bar>;                   rejected
```

That was wrong, for a reason that only shows up when you look one step ahead:
**the standard library is intended to become an installable package itself.**
With a sigil, `import <math>` and `import <@math>` would be two spellings of
the same thing depending on where math happened to come from, and pinning a
stdlib version would mean editing every import in a project.

So the first segment of an angle import is simply a package name:

```
import <math>;               "math" — from the standard library, or a pinned package
import <foo>;                "foo" — an installed package
import <myindex:foo>;        "foo" from index "myindex" — never stdlib
from <foo> import Thing;     works with every form
```

### Resolved: the standard library is a package, and there is only one of it

The intention above is now the implementation. Releases bundle no `stdlib/`
at all; `hover --setup` downloads it, and the compiler reaches it through the
ordinary package table.

It ships as **one** package named `stdlib`, not four. Four was the more
literal reading of "the first segment is the package name" — `import <math>`
naming a package called `math` — and it was the first implementation. One
package won on the things that decide it in practice:

- The four directories are versioned, released and tested as one artifact
  built from one source tree. Four independent versions could only ever be in
  lockstep or wrong.
- `--setup` makes one request instead of four, on the one code path every
  new install has to survive.
- Internal dependencies stop needing declaration. `semiconductors` uses
  `math`; shipping them together makes that a fact rather than a constraint
  to resolve.

The import spelling is unaffected, because the installed tree is **expanded**
into one package root per top-level directory. `<math>`, `<semiconductors>`
and `<stdlib/math>` all name real directories inside the single cached
package. Nothing in the compiler knows that `math` arrived inside something
called `stdlib`.

Resolution order is: the project's `hover.lock`, then the machine-wide
`~/.hover/stdlib.lock`. A project that pins `stdlib = "0.8.1"` gets its own
standard library expanded the same way, so every `import <math>` in it
resolves there — pinning a stdlib version still means editing no imports,
which was the whole point of dropping the sigil.

The hazard this accepts: a *transitive* dependency named `math` also lands in
the project lockfile and overrides project-wide. It is visible in
`hover.lock` and `hover hpm list`, which is the mitigation, and it is the
price of the uniform spelling. Index-qualified names are exempt — they never
fall back to stdlib, so an added index cannot satisfy an import meant for the
standard library.

The cost of not bundling: a fresh install needs network access once, and the
trust anchor for every user becomes the index URL rather than the binary they
already downloaded. Both were accepted deliberately.

The index qualifier travels with the package name because that pair *is* the
identity — two indexes may legitimately publish the same name, and they are
not the same package.

### `from` is a contextual keyword

Not reserved. It is an import only when it starts a statement and a path
follows; `from = to;` and `R d(from, to);` still parse as they always did.
This is not hypothetical — the standard library already declares
`nextafter(double from, double to)`, and two-terminal circuit code naming its
nodes `from`/`to` is entirely natural. The one-token lookahead that
disambiguates it is cheaper than breaking that.

### Reversed: an import names a directory, not a file

The first implementation imported a FILE. That was replaced, because it is
the model every modern language has moved away from and it forced three
things nobody wants: a library had to import its own siblings, splitting a
package across files was a breaking change for consumers, and a consumer had
to know the author's file layout.

The unit is now a directory. All `.hvr` files directly inside one share a
namespace; subdirectories are separate units. Consequences worth stating:

- Two files in one directory may not declare the same name — there would be
  no way to say which one a reference meant. Reported with both locations.
- Importing your own directory is an error with its own message, not a cycle:
  the names are already visible.
- **The entry file is not merged with its siblings.** You compile a file, and
  it is its own scope. Without that exception a directory of independent
  testbenches — which `examples/BJT/` is — would have several `main` modules
  colliding.

### Selective imports bind names, not directories

`from <dir> import A, B as C;` puts only the listed declarations into the
importing file's flat namespace, under their local spelling. `as` there
renames one declaration, where `import ... as` qualifies a whole directory —
the two meanings are mutually exclusive by construction.

A selected name may be a module *or* a function, so "this file declares no
such thing" is reported by the function pass, the only one that can see both
namespaces — and it lists what the file does declare, since a typo or a
renamed declaration is overwhelmingly the cause.

### Resolution has exactly one implementation

The elaborator used to keep a private copy of the loader's path-resolution
rule, kept local so it wouldn't depend on filesystem layout. That stopped
being tenable with packages: resolution now consults a per-project table
built from the lockfile, and two copies of the rule would resolve the same
import to two different files — the elaborator reporting "import could not be
resolved" for a file the loader had just read successfully. It calls
`loader.ResolveImportPath` now.

Package roots are read from the **lockfile**, never the manifest, and
compiling never resolves or installs anything. The manifest says which
versions are acceptable; the lockfile says which were chosen. A compile that
reached for the network because a manifest changed is how you get a build
that succeeds on one machine and not another.

## Staging

Stages 1–3 are done; 4 is deliberately not.

1. ~~Import namespace + `hover.toml` manifest, resolving from the local
   cache.~~
2. ~~`hover hpm install <url>` — fetch, hash-verify, lockfile.~~
3. ~~The index: static archive, `hover hpm install foo`, `hover hpm index
   add`.~~
4. Only if the ecosystem ever grows enough that discovery genuinely hurts:
   reconsider hosting, or add a caching mirror. Note that retiring a registry
   people depend on is not possible, whereas adding one later is.

### Still open

- **The official index does not exist yet.** `DefaultOfficialIndexURL` points
  at `github.com/hover-lang/hover-index`, which has to be created and made to
  publish an `index.tar.gz`. Until then only URL and git dependencies work.
- **Signatures are reserved, not checked.** The trust anchor is index review
  plus upstream account security — roughly AUR's level.
- **Version requirements are a small subset**: exact, `^`, `~`, `>=`, `*`.
  No unions, no ranges, no pre-release ordering beyond "a release outranks
  its own pre-releases". Anything else is rejected by name rather than
  silently reinterpreted.
- **Resolution does not backtrack.** Requirements accumulate per name and a
  conflict is reported naming who asked for what. A failure a person can read
  beats one an algorithm found clever — but a graph that a solver could
  satisfy and this cannot will report a conflict.
- **`--offline` does not cover a first install**, by construction: there is
  nothing cached to work from.
