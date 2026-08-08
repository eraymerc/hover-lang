package hpm

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
)

// ─────────────────────────────────────────────────────────────────────────────
// RESOLUTION
//
// Per-project, recorded in a lockfile. This is the one structural difference
// from a distro package manager, and everything else follows from it: pacman
// can ship exactly one version of a library because it curates the whole set
// to be co-installable, and a language package manager cannot — two projects
// on one machine will want different versions of the same thing.
//
// The algorithm is intentionally not a solver. Requirements accumulate per
// package name, and a name is re-resolved when a new requirement arrives
// that its current pick does not satisfy. If nothing published satisfies
// everything asked of it, that is reported as a conflict naming who asked
// for what — rather than backtracking through the graph hunting for some
// other combination. A failure a person can read beats one an algorithm
// found clever.
//
// Resolution runs in WAVES. Every package whose requirements are known is
// fetched concurrently; unpacking each one reveals its own dependencies,
// which form the next wave. This is where install time actually lives: with
// many dependencies the cost is dominated by connection setup, so N
// concurrent fetches over a shared HTTP transport is worth orders of
// magnitude more than any codec or algorithm choice in this file.
// ─────────────────────────────────────────────────────────────────────────────

// defaultConcurrency bounds simultaneous fetches. Bounded rather than
// unlimited: a hundred parallel requests to one host is indistinguishable
// from an attack and gets rate-limited accordingly.
var defaultConcurrency = func() int {
	n := runtime.NumCPU() * 2
	if n < 4 {
		n = 4
	}
	if n > 16 {
		n = 16
	}
	return n
}()

// Options control a resolve/install run.
type Options struct {
	// Offline forbids all network access. Anything not already in the cache
	// is an error.
	Offline bool

	// Locked forbids changing the lockfile. For CI: a build that would have
	// drifted fails instead of silently drifting.
	Locked bool

	// Update forces re-resolution against the index even when the lockfile
	// already has an answer. This is what `hover hpm update` sets.
	Update bool

	// UpdateOnly, when non-empty, limits Update to these package names.
	UpdateOnly []string

	// Concurrency caps simultaneous fetches; 0 means the default.
	Concurrency int

	Out io.Writer
}

func (o *Options) out() io.Writer {
	if o.Out == nil {
		return os.Stdout
	}
	return o.Out
}

func (o *Options) concurrency() int {
	if o.Concurrency > 0 {
		return o.Concurrency
	}
	return defaultConcurrency
}

func (o *Options) shouldUpdate(name string) bool {
	if !o.Update {
		return false
	}
	if len(o.UpdateOnly) == 0 {
		return true
	}
	for _, n := range o.UpdateOnly {
		if n == name {
			return true
		}
	}
	return false
}

// requirement records who asked for what, so a conflict can name them.
type requirement struct {
	spec        string
	requestedBy string // "" for a direct dependency of this project
}

// resolved is one package's outcome.
type resolved struct {
	pkg  LockedPackage
	dir  string
	reqs []requirement
}

// Resolver walks the dependency graph for one project.
type Resolver struct {
	manifest *Manifest
	lock     *Lockfile
	opts     Options

	mu      sync.Mutex // guards indexes, done and printing
	indexes map[string]*indexState
	done    map[string]*resolved
}

// indexState is one index plus its once-only sync outcome. The sync.Once is
// the whole point: see (*Resolver).index.
type indexState struct {
	ix   *Index
	once sync.Once
	err  error
}

// NewResolver prepares a resolver for a project.
func NewResolver(m *Manifest, lock *Lockfile, opts Options) *Resolver {
	return &Resolver{
		manifest: m,
		lock:     lock,
		opts:     opts,
		indexes:  map[string]*indexState{},
		done:     map[string]*resolved{},
	}
}

// pending is one dependency waiting to be resolved.
type pending struct {
	dep         Dependency
	requestedBy string
}

// Resolve walks every dependency, fetching what is missing, and returns the
// full set in name order.
func (r *Resolver) Resolve(ctx context.Context) ([]LockedPackage, error) {
	wave := make([]pending, 0, len(r.manifest.Deps))
	for _, d := range r.manifest.Deps {
		wave = append(wave, pending{dep: d})
	}

	for len(wave) > 0 {
		next, err := r.runWave(ctx, wave)
		if err != nil {
			return nil, err
		}
		wave = next
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]LockedPackage, 0, len(r.done))
	for _, res := range r.done {
		out = append(out, res.pkg)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// runWave resolves one wave concurrently and returns the next one.
//
// Errors are collected rather than raced for: with several broken
// dependencies, reporting the first one to fail on a given run makes the
// failure depend on goroutine scheduling, and a build that reports a
// different error each time is much harder to fix than one that reports the
// same list every time. The list is sorted before printing for the same
// reason.
func (r *Resolver) runWave(ctx context.Context, wave []pending) ([]pending, error) {
	sem := make(chan struct{}, r.opts.concurrency())
	var wg sync.WaitGroup

	var mu sync.Mutex
	var errs []string
	var next []pending

	for _, item := range wave {
		wg.Add(1)
		go func(p pending) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			res, err := r.resolveOne(ctx, p.dep, p.requestedBy)
			if err != nil {
				mu.Lock()
				errs = append(errs, err.Error())
				mu.Unlock()
				return
			}
			if res == nil {
				return // already resolved and still satisfied
			}

			// A package's own manifest declares its dependencies. Missing is
			// not an error: a package is just .hvr source, and a leaf
			// package legitimately has no manifest at all.
			sub, err := readPackageDeps(res.dir)
			if err != nil {
				mu.Lock()
				errs = append(errs, fmt.Sprintf("dependency %q: %v", p.dep.Name, err))
				mu.Unlock()
				return
			}
			mu.Lock()
			for _, sd := range sub {
				next = append(next, pending{dep: sd, requestedBy: p.dep.Name})
			}
			mu.Unlock()
		}(item)
	}
	wg.Wait()

	if len(errs) > 0 {
		sort.Strings(errs)
		// One cause, many victims: an unreachable index fails every package
		// in the wave with the identical message, and printing "4
		// dependencies failed" above four copies of one sentence describes
		// the blast radius instead of the problem. Deduplicated, the
		// network-down case reads as the single failure it is.
		errs = dedupe(errs)
		if len(errs) == 1 {
			return nil, fmt.Errorf("%s", errs[0])
		}
		return nil, fmt.Errorf("%d dependencies failed to resolve:\n  - %s",
			len(errs), strings.Join(errs, "\n  - "))
	}

	sort.Slice(next, func(i, j int) bool { return next[i].dep.Name < next[j].dep.Name })
	return next, nil
}

// dedupe removes repeated strings from a sorted slice, preserving order.
func dedupe(sorted []string) []string {
	out := sorted[:0]
	for i, s := range sorted {
		if i == 0 || s != sorted[i-1] {
			out = append(out, s)
		}
	}
	return out
}

// resolveOne resolves a single dependency, returning nil when the existing
// resolution already covers it (so the caller does not re-walk its subtree).
func (r *Resolver) resolveOne(ctx context.Context, d Dependency, requestedBy string) (*resolved, error) {
	if idx, _, qualified := splitQualified(d.Name); qualified && d.Indexed() {
		if !r.manifest.hasIndex(idx) {
			return nil, fmt.Errorf("%q refers to index %q, which this project does not declare — add it with `hover hpm index add <url> --name %s`%s",
				d.Name, idx, idx, requestedBySuffix(requestedBy))
		}
	}

	req := requirement{spec: d.Version, requestedBy: requestedBy}

	r.mu.Lock()
	existing, already := r.done[d.Name]
	if already {
		existing.reqs = append(existing.reqs, req)
		satisfiedAlready := !d.Indexed() || existing.pkg.Version == "" || satisfies(existing.pkg.Version, d.Version)
		reqs := append([]requirement(nil), existing.reqs...)
		r.mu.Unlock()
		if satisfiedAlready {
			return nil, nil
		}
		// Re-resolve against every requirement seen so far.
		res, err := r.resolveIndexed(ctx, d, reqs)
		if err != nil {
			return nil, err
		}
		r.mu.Lock()
		r.done[d.Name] = res
		r.mu.Unlock()
		return res, nil
	}
	// Claim the name before releasing the lock, so two goroutines reaching
	// the same transitive dependency in one wave cannot both fetch it.
	r.done[d.Name] = &resolved{reqs: []requirement{req}}
	r.mu.Unlock()

	var res *resolved
	var err error
	switch {
	case d.Git != "":
		res, err = r.resolveGit(ctx, d, []requirement{req})
	case d.URL != "":
		res, err = r.resolveURL(ctx, d, []requirement{req})
	default:
		res, err = r.resolveIndexed(ctx, d, []requirement{req})
	}
	if err != nil {
		r.mu.Lock()
		delete(r.done, d.Name) // failed: don't leave a placeholder behind
		r.mu.Unlock()
		return nil, err
	}

	r.mu.Lock()
	res.reqs = append(res.reqs, r.done[d.Name].reqs[1:]...) // anything queued while we fetched
	r.done[d.Name] = res
	r.mu.Unlock()
	return res, nil
}

// resolveURL handles a direct archive URL dependency. There is no version
// selection: the URL names one exact archive, and the hash confirms it.
func (r *Resolver) resolveURL(ctx context.Context, d Dependency, reqs []requirement) (*resolved, error) {
	locked, hasLock := r.lock.Get(d.Name)

	expect := ""
	if hasLock && !r.opts.shouldUpdate(d.Name) && locked.URL == d.URL {
		expect = locked.Hash
	}

	if r.opts.Offline {
		dir, err := r.cachedDir(d.Name, expect)
		if err != nil {
			return nil, err
		}
		return &resolved{pkg: locked, dir: dir, reqs: reqs}, nil
	}
	if r.opts.Locked && expect == "" {
		return nil, fmt.Errorf("--locked was given but %q is not resolved in %s", d.Name, LockName)
	}

	res, err := Fetch(ctx, d.URL, expect)
	if err != nil {
		return nil, err
	}
	r.report(res.Cached, "%s from %s", d.Name, d.URL)

	return &resolved{
		pkg:  LockedPackage{Name: d.Name, Version: d.Version, URL: d.URL, Hash: res.Hash},
		dir:  res.Dir,
		reqs: reqs,
	}, nil
}

// resolveGit handles the opt-in git transport.
func (r *Resolver) resolveGit(ctx context.Context, d Dependency, reqs []requirement) (*resolved, error) {
	locked, hasLock := r.lock.Get(d.Name)

	expect := ""
	if hasLock && !r.opts.shouldUpdate(d.Name) && locked.URL == d.Git {
		expect = locked.Hash
	}

	if r.opts.Offline {
		dir, err := r.cachedDir(d.Name, expect)
		if err != nil {
			return nil, err
		}
		return &resolved{pkg: locked, dir: dir, reqs: reqs}, nil
	}
	if r.opts.Locked && expect == "" {
		return nil, fmt.Errorf("--locked was given but %q is not resolved in %s", d.Name, LockName)
	}

	res, err := FetchGit(ctx, d.Name, d.Git, d.Rev, expect)
	if err != nil {
		return nil, err
	}
	r.report(res.Cached, "%s from %s%s", d.Name, d.Git, refSuffix(d.Rev))

	return &resolved{
		pkg:  LockedPackage{Name: d.Name, Version: d.Version, URL: d.Git, Git: true, Rev: d.Rev, Hash: res.Hash},
		dir:  res.Dir,
		reqs: reqs,
	}, nil
}

// resolveIndexed handles a dependency resolved through an index.
func (r *Resolver) resolveIndexed(ctx context.Context, d Dependency, reqs []requirement) (*resolved, error) {
	locked, hasLock := r.lock.Get(d.Name)

	// The lockfile short-circuits everything when it already answers the
	// question: no index sync, no version selection, and no network at all
	// if the content is cached. This is the plain `hover hpm install` in a
	// freshly cloned project, and it is the path that has to be fast.
	if hasLock && !r.opts.shouldUpdate(d.Name) && allSatisfiedBy(locked.Version, reqs) {
		dir, err := r.materialize(ctx, locked)
		if err != nil {
			return nil, err
		}
		return &resolved{pkg: locked, dir: dir, reqs: reqs}, nil
	}

	if r.opts.Locked {
		return nil, fmt.Errorf("--locked was given but %q needs re-resolving (%s) — run `hover hpm update %s` and commit the lockfile",
			d.Name, describeReqs(reqs), d.Name)
	}
	if r.opts.Offline {
		return nil, fmt.Errorf("--offline was given but %q is not resolved in %s", d.Name, LockName)
	}

	ix, err := r.index(ctx, d.IndexName())
	if err != nil {
		return nil, err
	}
	entry, err := ix.Lookup(d.BareName())
	if err != nil {
		return nil, err
	}

	pick, err := selectForAll(entry, reqs)
	if err != nil {
		return nil, err
	}

	res, err := Fetch(ctx, pick.URL, pick.Hash)
	if err != nil {
		return nil, err
	}
	r.report(res.Cached, "%s %s from %s", d.Name, pick.Version, pick.URL)

	return &resolved{
		pkg: LockedPackage{
			Name: d.Name, Version: pick.Version, Index: d.IndexName(),
			URL: pick.URL, Hash: pick.Hash,
		},
		dir:  res.Dir,
		reqs: reqs,
	}, nil
}

// report prints one progress line. Serialized under the resolver lock so
// concurrent fetches do not interleave mid-line.
func (r *Resolver) report(cached bool, format string, args ...any) {
	verb := "installed"
	if cached {
		verb = "cached   "
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	fmt.Fprintf(r.opts.out(), "  %s %s\n", verb, fmt.Sprintf(format, args...))
}

// materialize makes sure a locked package's content is present in the cache,
// fetching it if it isn't.
func (r *Resolver) materialize(ctx context.Context, p LockedPackage) (string, error) {
	dir, err := CacheDir(p.Hash)
	if err != nil {
		return "", err
	}
	if dirExists(dir) {
		return dir, nil
	}
	if r.opts.Offline {
		return "", fmt.Errorf("--offline was given but %q (%s) is not in the local cache", p.Name, p.Hash)
	}
	var res FetchResult
	if p.Git {
		res, err = FetchGit(ctx, p.Name, p.URL, p.Rev, p.Hash)
	} else {
		res, err = Fetch(ctx, p.URL, p.Hash)
	}
	if err != nil {
		return "", err
	}
	r.report(res.Cached, "%s %s", p.Name, p.Version)
	return res.Dir, nil
}

func (r *Resolver) cachedDir(name, hash string) (string, error) {
	if hash == "" {
		return "", fmt.Errorf("--offline was given but %q has never been resolved", name)
	}
	dir, err := CacheDir(hash)
	if err != nil {
		return "", err
	}
	if !dirExists(dir) {
		return "", fmt.Errorf("--offline was given but %q is not in the local cache", name)
	}
	return dir, nil
}

// index returns a synced handle to a named index, syncing at most once per
// run.
//
// A refresh failure against an existing copy is a warning, not an error:
// refusing to install because a refresh failed would throw away the entire
// reason for keeping the index local. A first-ever sync has nothing to fall
// back to and does fail.
func (r *Resolver) index(ctx context.Context, name string) (*Index, error) {
	r.mu.Lock()
	st, have := r.indexes[name]
	if !have {
		url, ok := r.manifest.IndexURL(name)
		if !ok {
			r.mu.Unlock()
			return nil, fmt.Errorf("index %q is not declared in %s", name, ManifestName)
		}
		ix, err := OpenIndex(name, url)
		if err != nil {
			r.mu.Unlock()
			return nil, err
		}
		st = &indexState{ix: ix}
		r.indexes[name] = st
	}
	r.mu.Unlock()

	if r.opts.Offline {
		return st.ix, nil
	}

	// Sync exactly once per index, with everyone else WAITING for it rather
	// than proceeding. A wave resolves several packages from one index
	// concurrently, and an earlier version marked the index synced before
	// the sync had actually run — so the other goroutines raced ahead and
	// looked up names in an index that was still empty, failing with
	// "index has never been synced" while the sync was in flight beside
	// them. sync.Once is what makes "first caller does the work, the rest
	// block until it is done" the only possible ordering.
	st.once.Do(func() {
		if err := st.ix.Sync(ctx); err != nil {
			if !st.ix.Synced() {
				st.err = err
				return
			}
			r.mu.Lock()
			fmt.Fprintf(r.opts.out(), "  warning: could not refresh index %q (%v) — using the local copy\n", name, err)
			r.mu.Unlock()
		}
	})
	if st.err != nil {
		return nil, st.err
	}
	return st.ix, nil
}

// selectForAll picks the highest version satisfying every accumulated
// requirement, or explains who made that impossible.
func selectForAll(entry *IndexEntry, reqs []requirement) (IndexVersion, error) {
	var best *IndexVersion
	for i := range entry.Versions {
		v := entry.Versions[i]
		if v.Yanked && !exactlyRequested(v.Version, reqs) {
			continue
		}
		ok := true
		for _, req := range reqs {
			if !satisfies(v.Version, req.spec) {
				ok = false
				break
			}
		}
		if !ok {
			continue
		}
		if best == nil || compareVersions(v.Version, best.Version) > 0 {
			best = &entry.Versions[i]
		}
	}
	if best != nil {
		return *best, nil
	}

	var have []string
	for _, v := range entry.Versions {
		if !v.Yanked {
			have = append(have, v.Version)
		}
	}
	if len(reqs) == 1 {
		return IndexVersion{}, fmt.Errorf("no version of %q satisfies %q (published: %s)",
			entry.Name, reqs[0].spec, strings.Join(have, ", "))
	}
	return IndexVersion{}, fmt.Errorf("version conflict on %q — no published version satisfies every requirement\n  %s\n  published: %s",
		entry.Name, describeReqs(reqs), strings.Join(have, ", "))
}

func exactlyRequested(version string, reqs []requirement) bool {
	for _, r := range reqs {
		if r.spec == version {
			return true
		}
	}
	return false
}

func allSatisfiedBy(version string, reqs []requirement) bool {
	if version == "" {
		return false
	}
	for _, r := range reqs {
		if !satisfies(version, r.spec) {
			return false
		}
	}
	return true
}

func describeReqs(reqs []requirement) string {
	seen := map[string]bool{}
	var parts []string
	for _, r := range reqs {
		who := "this project"
		if r.requestedBy != "" {
			who = r.requestedBy
		}
		s := fmt.Sprintf("%s requires %q", who, r.spec)
		if seen[s] {
			continue
		}
		seen[s] = true
		parts = append(parts, s)
	}
	sort.Strings(parts)
	return strings.Join(parts, "\n  ")
}

func requestedBySuffix(by string) string {
	if by == "" {
		return ""
	}
	return " (required by " + by + ")"
}

// readPackageDeps reads a fetched package's own hover.toml. A package with
// no manifest has no dependencies, which is the normal case for a leaf
// package of pure .hvr source.
//
// A transitive dependency may not name an index: that would let a package
// pull in an index the consuming project never agreed to trust, which is
// exactly what qualified names exist to prevent. Such an entry is reported
// rather than silently dropped.
func readPackageDeps(dir string) ([]Dependency, error) {
	if dir == "" || !fileExists(filepath.Join(dir, ManifestName)) {
		return nil, nil
	}
	m, err := LoadManifest(dir)
	if err != nil {
		return nil, err
	}
	for _, d := range m.Deps {
		if idx, _, qualified := splitQualified(d.Name); qualified {
			return nil, fmt.Errorf("its %s depends on %q, from index %q — a dependency cannot bring in an index the project has not declared. Add it explicitly with `hover hpm index add`",
				ManifestName, d.Name, idx)
		}
	}
	return m.Deps, nil
}
