package hpm

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// ─────────────────────────────────────────────────────────────────────────────
// THE INDEX
//
// An index is an archive of TOML files, one per package:
//
//	packages/<name>.toml
//
// It is downloaded over plain HTTPS and unpacked into ~/.hover/index/<name>/.
// It contains pointers and hashes, never package bytes.
//
// Owning it locally is the pacman property worth copying: after the first
// sync, resolution works with the network down, so an index outage costs you
// discovery of NEW packages rather than your build.
//
// Distributed as an archive rather than a git repository for the same reason
// packages are — it removes git as a runtime requirement, and hover-lang.org
// then serves literally one static file with no service behind it. crates.io
// made the same move in 2022 (git index → HTTP) once git sync became the
// bottleneck. The index can still BE a public git repo that people review
// pull requests against; only its distribution is HTTP.
//
// Each entry pins, per version, the archive URL and the content hash.
// Mapping a name to a bare repository URL is what PyPI did until ~2013 and
// abandoned because it did not work: repos get deleted, tags get moved,
// branches get renamed. The hash turns all of those from "silently installed
// something else" into a clean error.
// ─────────────────────────────────────────────────────────────────────────────

// IndexEntry is one package's index file.
type IndexEntry struct {
	Name        string
	Description string
	Repository  string // informational: where to read the source and file issues
	Versions    []IndexVersion
}

// IndexVersion is one published version of a package.
type IndexVersion struct {
	Version string
	URL     string // archive URL — .tar.gz or .tar.zst
	Hash    string // "sha256:..." over the unpacked tree

	// Signature and SignedBy are parsed and carried but not yet checked.
	//
	// The fields exist now on purpose. Hashes give integrity and
	// reproducibility but NOT authenticity: if an upstream account is
	// compromised, the hash recorded for a new version is simply whatever
	// the attacker published. Signing is the fix, and adding fields to a
	// format after people depend on it is far more painful than reserving
	// them before anyone does.
	Signature string
	SignedBy  string

	// Yanked marks a version that should no longer be selected for new
	// installs but must keep resolving for anyone who already locked it —
	// crates.io's model. Deleting a version outright is npm's unpublish,
	// which is how left-pad broke thousands of builds.
	Yanked bool
}

// Index is one configured index and its local copy.
type Index struct {
	Name string
	URL  string
	Dir  string
}

// OpenIndex returns a handle to a named index's local copy, without touching
// the network.
func OpenIndex(name, url string) (*Index, error) {
	dir, err := IndexDir(name)
	if err != nil {
		return nil, err
	}
	return &Index{Name: name, URL: url, Dir: dir}, nil
}

// Synced reports whether this index has ever been downloaded.
func (ix *Index) Synced() bool { return dirExists(filepath.Join(ix.Dir, "packages")) }

// Age returns how long ago this index was last synced.
func (ix *Index) Age() (time.Duration, bool) {
	info, err := os.Stat(filepath.Join(ix.Dir, ".synced"))
	if err != nil {
		return 0, false
	}
	return time.Since(info.ModTime()), true
}

// Sync downloads the index archive and replaces the local copy.
//
// The caller decides how to treat a failure. `install` warns and carries on
// against the existing copy, because refusing to install over a failed
// refresh throws away the entire reason for keeping the index local; a
// first-ever sync has nothing to fall back to and does fail.
func (ix *Index) Sync(ctx context.Context) error {
	format, err := FormatForURL(ix.URL)
	if err != nil {
		return fmt.Errorf("index %q: %w", ix.Name, err)
	}
	if err := checkURLScheme(ix.URL); err != nil {
		return fmt.Errorf("index %q: %w", ix.Name, err)
	}

	body, err := httpGet(ctx, ix.URL)
	if err != nil {
		return fmt.Errorf("could not sync index %q: %w", ix.Name, err)
	}
	defer body.Close()

	if err := os.MkdirAll(filepath.Dir(ix.Dir), 0755); err != nil {
		return err
	}
	staging, err := os.MkdirTemp(filepath.Dir(ix.Dir), ".sync-"+ix.Name+"-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(staging)

	root, err := ExtractArchive(body, format, staging)
	if err != nil {
		return fmt.Errorf("index %q: %w", ix.Name, err)
	}
	if !dirExists(filepath.Join(root, "packages")) {
		return fmt.Errorf("index %q does not look like an index — %s contains no packages/ directory", ix.Name, ix.URL)
	}

	// Swap the new copy in only after it has been fully extracted and
	// validated. An interrupted sync must not be able to leave a half-
	// populated index that then resolves some packages and mysteriously
	// fails to find others.
	old := ix.Dir + ".old"
	os.RemoveAll(old)
	if dirExists(ix.Dir) {
		if err := os.Rename(ix.Dir, old); err != nil {
			return err
		}
	}
	if err := os.Rename(root, ix.Dir); err != nil {
		os.Rename(old, ix.Dir) // put the previous copy back
		return err
	}
	os.RemoveAll(old)

	// A marker file, so "how stale is this?" is answerable without a
	// network round trip.
	os.WriteFile(filepath.Join(ix.Dir, ".synced"), []byte(time.Now().UTC().Format(time.RFC3339)+"\n"), 0644)
	return nil
}

// entryPath is where a package's index file lives.
//
// Flat rather than sharded (crates.io shards by name length). Sharding keeps
// directory sizes workable at six figures of packages; hover is nowhere near
// that, and a flat layout is one a human can browse — which matters when the
// trust model is "someone reads the index entry".
func (ix *Index) entryPath(pkg string) string {
	return filepath.Join(ix.Dir, "packages", pkg+".toml")
}

// Lookup reads one package's entry from the local copy.
func (ix *Index) Lookup(pkg string) (*IndexEntry, error) {
	if !ix.Synced() {
		return nil, fmt.Errorf("index %q has never been synced — run `hover hpm update` first", ix.Name)
	}
	if err := validPackageName(pkg); err != nil {
		return nil, err
	}

	src, err := os.ReadFile(ix.entryPath(pkg))
	if os.IsNotExist(err) {
		return nil, fmt.Errorf("package %q is not in index %q%s", pkg, ix.Name, ix.suggest(pkg))
	}
	if err != nil {
		return nil, err
	}

	doc, err := ParseTOML(string(src))
	if err != nil {
		return nil, fmt.Errorf("index %q has a malformed entry for %q: %w", ix.Name, pkg, err)
	}

	e := &IndexEntry{Name: pkg}
	if root := doc.Table(); root != nil {
		if v, ok := root.Get("name"); ok {
			e.Name = v
		}
		e.Description, _ = root.Get("description")
		e.Repository, _ = root.Get("repository")
	}
	if e.Name != pkg {
		return nil, fmt.Errorf("index %q: the entry file for %q declares name %q — the index is inconsistent",
			ix.Name, pkg, e.Name)
	}

	for _, t := range doc.Tables("version") {
		var v IndexVersion
		v.Version, _ = t.Get("version")
		v.URL, _ = t.Get("url")
		v.Hash, _ = t.Get("hash")
		v.Signature, _ = t.Get("signature")
		v.SignedBy, _ = t.Get("signed_by")
		if y, ok := t.Get("yanked"); ok {
			v.Yanked = y == "true"
		}
		if v.Version == "" || v.URL == "" || v.Hash == "" {
			return nil, fmt.Errorf("index %q: a [[version]] block for %q is missing version, url or hash", ix.Name, pkg)
		}
		if _, err := FormatForURL(v.URL); err != nil {
			return nil, fmt.Errorf("index %q, %s %s: %w", ix.Name, pkg, v.Version, err)
		}
		e.Versions = append(e.Versions, v)
	}
	if len(e.Versions) == 0 {
		return nil, fmt.Errorf("index %q lists %q but publishes no versions of it", ix.Name, pkg)
	}

	return e, nil
}

// suggest offers near-miss package names, since a typo is by far the most
// common reason a lookup fails.
func (ix *Index) suggest(pkg string) string {
	names, err := ix.List()
	if err != nil {
		return ""
	}
	var near []string
	for _, n := range names {
		if strings.Contains(n, pkg) || strings.Contains(pkg, n) || editDistanceAtMost(n, pkg, 2) {
			near = append(near, n)
		}
	}
	if len(near) == 0 {
		return ""
	}
	sort.Strings(near)
	if len(near) > 5 {
		near = near[:5]
	}
	return " — did you mean: " + strings.Join(near, ", ") + "?"
}

// List returns every package name in the index.
func (ix *Index) List() ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(ix.Dir, "packages"))
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".toml") {
			continue
		}
		names = append(names, strings.TrimSuffix(e.Name(), ".toml"))
	}
	sort.Strings(names)
	return names, nil
}

// validPackageName rejects anything that could escape the packages/
// directory or otherwise turn a name into a path. Package names arrive from
// manifests, and a manifest can come from a dependency — so this is a
// path-traversal boundary, not a style check.
func validPackageName(name string) error {
	if name == "" {
		return fmt.Errorf("empty package name")
	}
	if len(name) > 64 {
		return fmt.Errorf("package name %q is too long (max 64 characters)", name)
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		ok := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
			(c >= '0' && c <= '9') || c == '-' || c == '_'
		if !ok {
			return fmt.Errorf("invalid package name %q: only letters, digits, '-' and '_' are allowed", name)
		}
	}
	return nil
}

// validIndexName applies the same rule to index names, which also become
// directory names under ~/.hover/index/.
func validIndexName(name string) error {
	if err := validPackageName(name); err != nil {
		return fmt.Errorf("%s", strings.ReplaceAll(err.Error(), "package name", "index name"))
	}
	if name == OfficialIndex {
		return fmt.Errorf("%q is reserved for the index shipped with hover", OfficialIndex)
	}
	return nil
}

// editDistanceAtMost reports whether a and b are within max edits, bailing
// out early rather than computing the full matrix.
func editDistanceAtMost(a, b string, max int) bool {
	if abs(len(a)-len(b)) > max {
		return false
	}
	prev := make([]int, len(b)+1)
	cur := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur[0] = i
		rowMin := cur[0]
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min3(cur[j-1]+1, prev[j]+1, prev[j-1]+cost)
			if cur[j] < rowMin {
				rowMin = cur[j]
			}
		}
		if rowMin > max {
			return false
		}
		prev, cur = cur, prev
	}
	return prev[len(b)] <= max
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

func min3(a, b, c int) int {
	m := a
	if b < m {
		m = b
	}
	if c < m {
		m = c
	}
	return m
}
