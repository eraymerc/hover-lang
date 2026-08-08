# Example: a Hover package index

An index maps short names to archive URLs and content hashes. **It stores no
code.** hover-lang.org will publish exactly one file that looks like this,
and it is why the project runs no service, owns no accounts, and has no
uptime obligation on anybody's build.

```
examples/index/
    packages/
        hvr-rc.toml         real — installable, points at ../package/hvr-rc/
        example-parts.toml  illustrative — every supported field
        stdlib.toml         illustrative — the standard library as a package
    make-index.sh           builds index.tar.gz from packages/
```

`stdlib.toml` is worth reading: the standard library is not bundled with
releases, so an entry exactly like it is what makes `import <math>` work at
all. It is **one** package whose four top-level directories each become an
importable package root — its manifest is in the source tree at
`stdlib/hover.toml`, and `make stdlib-archives` generates the real archive,
hash and index entry.

See [../../docs/packages.md](../../docs/packages.md) for the whole workflow
and [../../docs/package-manager-design.md](../../docs/package-manager-design.md)
for why it works this way.

## The format

An index is an **archive whose root contains a `packages/` directory**, one
`<name>.toml` per package. That's it.

```toml
name = "hvr-rc"
description = "Passive RC filter building blocks"
repository = "https://github.com/example/hvr-rc"

[[version]]
version = "0.1.0"
url = "https://github.com/example/hvr-rc/archive/refs/tags/v0.1.0.tar.gz"
hash = "sha256:c2d3785003..."
```

| Field | |
|---|---|
| `name` | Must match the filename. Letters, digits, `-`, `_`. |
| `description` | Free text, shown when a lookup misses. |
| `repository` | Informational — where to read the source and file issues. |
| `[[version]].version` | Semver-shaped. One block per published version. |
| `[[version]].url` | The archive: `.tar.gz` or `.tar.zst`. |
| `[[version]].hash` | `sha256:` over the **unpacked tree**. |
| `[[version]].yanked` | Optional. Skip for new resolutions; keep working for anyone already locked. |
| `[[version]].signature`, `signed_by` | Reserved. Parsed and carried, **not yet verified**. |

Layout notes:

- **Flat, not sharded.** crates.io shards by name length to keep directories
  manageable at six figures of packages. Hover is nowhere near that, and a
  flat layout is one a human can browse on a repository host — which matters
  when the trust model is "someone reads the entry before merging it".
- **The archive may wrap everything in one top-level directory.** `hpm`
  strips a single wrapper, so a hand-built archive and a GitHub tag archive
  of the same index repo both work.

## Run it end to end, locally

No network, no server you don't control, no official index needed.

### 1. Serve the package and the index from one directory

```sh
cd /path/to/hover

# The package this index describes.
tar -C examples/package -czf /tmp/hvr-rc-0.1.0.tar.gz hvr-rc

# The index itself.
examples/index/make-index.sh /tmp/index.tar.gz

cd /tmp && python3 -m http.server 8000 &
```

`packages/hvr-rc.toml` already points at
`http://127.0.0.1:8000/hvr-rc-0.1.0.tar.gz`, which is what that server is
now serving.

### 2. Point hover at it and install by name

```sh
mkdir /tmp/demo && cd /tmp/demo
hover hpm init demo

export HOVER_INDEX_URL=http://127.0.0.1:8000/index.tar.gz
export HOVER_ALLOW_INSECURE_HTTP=1

hover hpm install hvr-rc
```

```
  installed hvr-rc 0.1.0 from http://127.0.0.1:8000/hvr-rc-0.1.0.tar.gz
1 package(s) ready.
```

`hover.lock` now records which index answered, alongside the URL and hash:

```toml
[[package]]
name = "hvr-rc"
version = "0.1.0"
index = "official"
url = "http://127.0.0.1:8000/hvr-rc-0.1.0.tar.gz"
hash = "sha256:c2d3785003..."
```

### 3. Use it

```hover
import <hvr-rc>;
from <hvr-rc> import Divider;
```

### 4. Watch the hash do its job

```sh
echo "// tampered" >> /path/to/hover/examples/package/hvr-rc/filters.hvr
tar -C /path/to/hover/examples/package -czf /tmp/hvr-rc-0.1.0.tar.gz hvr-rc

rm -rf ~/.hover/hpm && hover hpm install
```

```
hpm: content of http://127.0.0.1:8000/hvr-rc-0.1.0.tar.gz does not match the
expected hash — refusing to install
  expected: sha256:c2d3785003...
  actual:   sha256:...
```

Hard failure, not a warning. The index said what the bytes should be; they
aren't. (Undo the edit afterwards.)

> `HOVER_INDEX_URL` exists for testing and air-gapped mirrors. To *add* an
> index rather than replace the official one, use `hover hpm index add` —
> its packages then stay qualified as `name:package` and can never shadow an
> official name.

## Publishing an index for real

1. Put `packages/` in a public git repository. Pull requests are the review
   step — this is the trust anchor, roughly AUR's model, so entries should be
   read before they are merged.
2. Publish an archive of it at a stable URL. A tag archive from the same
   repository host works, or run `make-index.sh` in CI and upload the result.
3. Tell people the URL:
   ```sh
   hover hpm index add https://example.com/my-index.tar.gz --name myparts
   ```
   ```hover
   import <myparts:vendor-diodes>;
   ```

The index is distributed over HTTP rather than cloned with git, so nobody
needs git installed to use it — and the repository stays available for the
review workflow. crates.io made the same move in 2022 for the same reason.

## Adding a package to an index

1. Get the hash: install the package once by URL, then read `hover.lock`.
2. Add `packages/<name>.toml`, or append a `[[version]]` block to an existing
   entry.
3. Rebuild and republish the archive.

Nothing is uploaded. You are publishing a **pointer**, and the package author
keeps hosting their own bytes — which is deliberate, since many valuable
Hover packages will be vendor SPICE models whose redistribution terms belong
with whoever agreed to them.
