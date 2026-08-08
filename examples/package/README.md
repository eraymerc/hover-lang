# Example: publishing and consuming a Hover package

Two halves of the same story:

- [`hvr-rc/`](hvr-rc/) — a publishable package: RC filter modules and helper
  functions, split across two files that see each other without importing.
- [`consumer/`](consumer/) — a project that depends on it and imports from
  it.

See [../../docs/imports.md](../../docs/imports.md) for the import forms and
[../../docs/package-manager-design.md](../../docs/package-manager-design.md)
for why the package manager works the way it does.

## What a package actually is

A directory of `.hvr` files, published as a `.tar.gz` or `.tar.zst` archive
at a URL, identified by the hash of its contents.

That's the whole thing. No build step, no compiled artifacts, no
per-platform variants — Hover packages are pure source, and the only
compiled thing in the system (the runtime) ships with the compiler. A
`hover.toml` inside a package is optional and only needed if the package has
dependencies of its own.

Because **an import names a directory**, every `.hvr` file in the package is
imported together by a single `import <hvr-rc>;`. This package is two files —
`filters.hvr` and `dividers.hvr` — and `dividers.hvr` uses `RCLowPass` from
its sibling with no import between them. How an author splits a package is
invisible to consumers, and adding a file is never a breaking change.

Nothing is uploaded anywhere. hover-lang.org publishes an *index* of pointers
and hashes; the bytes always come from wherever you put them.

## Try it end to end, locally

You don't need the official index, a server, or even a network. Everything
below runs against a file you build yourself.

### 1. Build the archive

```sh
cd examples/package
tar -czf /tmp/hvr-rc-0.1.0.tar.gz hvr-rc
```

`.tar.zst` works identically if you have `tar --zstd`. The archive hashes the
same either way — the hash is over the unpacked tree, so it is independent of
how the package was transported.

### 2. Install it into the consumer project

`hpm` requires `https` for real URLs. For a local file, serve it and allow
plain HTTP explicitly:

```sh
cd /tmp && python3 -m http.server 8000 &

cd examples/package/consumer
HOVER_ALLOW_INSECURE_HTTP=1 \
  hover hpm install http://127.0.0.1:8000/hvr-rc-0.1.0.tar.gz
```

That writes a `[dependencies.hvr-rc]` block into `hover.toml` and a
`hover.lock` recording the URL and content hash. The package itself is
unpacked into `~/.hover/hpm/<hash>/`, shared across every project on the
machine.

### 3. Compile against it

```sh
hover rc_filter.hvr
```

`import <hvr-rc>` resolves through `hover.lock`. **Compiling
never installs anything** — the manifest says which versions are acceptable,
the lockfile says which were chosen, and a compile that reached for the
network because a manifest changed is how you get a build that works on one
machine and not another.

### 4. Prove the integrity check is real

```sh
echo "// tampered" >> hvr-rc/filters.hvr
tar -czf /tmp/hvr-rc-0.1.0.tar.gz hvr-rc
hover hpm verify
```

```
  FAIL  hvr-rc    UPSTREAM CHANGED
      expected sha256:183d452a...
      actual   sha256:14c894fb...
```

`verify` checks three things: the archive is still reachable (link rot), it
still hashes to the locked value (a re-uploaded release, or tampering), and
the local cached copy hasn't been modified since install. Re-installing at
this point fails outright rather than warning — "the code you are about to
compile is not the code that was reviewed" has no sensible continue-anyway
path.

## Publishing for real

1. **Tag a release** on your repository host. GitHub and GitLab generate a
   `.tar.gz` for every tag automatically, at a stable URL — that URL is all a
   consumer needs.
2. Anyone can already depend on it:
   ```toml
   [dependencies.hvr-rc]
   url = "https://github.com/you/hvr-rc/archive/refs/tags/v0.1.0.tar.gz"
   ```
3. **To get a short name** (`hvr-rc = "^0.1.0"`), submit an entry to the
   index — a single TOML file. See [../index/](../index/) for a working
   index containing exactly this package:
   ```toml
   name = "hvr-rc"
   description = "Passive RC filter building blocks"
   repository = "https://github.com/you/hvr-rc"

   [[version]]
   version = "0.1.0"
   url = "https://github.com/you/hvr-rc/archive/refs/tags/v0.1.0.tar.gz"
   hash = "sha256:..."
   ```
   The index stores pointers and hashes only. Being listed is a
   discoverability convenience, not a hosting arrangement — nobody but you
   holds your code.

## Useful commands

```sh
hover hpm install              # restore from hover.toml + hover.lock
hover hpm install foo@^1.2     # add an indexed dependency
hover hpm list                 # direct dependencies, then what they pulled in
hover hpm update [foo]         # move to newer versions, rewrite the lockfile
hover hpm verify               # re-check every locked package
hover hpm install --locked     # CI: fail rather than change hover.lock
hover hpm install --offline    # never touch the network
hover hpm clean                # drop cached packages this project dropped
```

Releases also ship an `hpm` symlink to the same binary, so `hpm install foo`
and `hover hpm install foo` are the same words in the same order.

## Commit the lockfile

`hover.lock` is generated but **belongs in version control**. It is what
makes a build reproducible, it is sorted so its diffs stay readable, and
`hover hpm install --locked` in CI turns a dependency that quietly drifted
into a failed build instead of a mystery.
