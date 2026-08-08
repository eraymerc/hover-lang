package hpm

import (
	"context"
	"fmt"
	"os"
	"sort"
	"sync"
)

// cmdVerify re-checks every locked package: that the cached copy still
// hashes to what the lockfile says, and that the archive is still reachable.
//
// Two different failures, deliberately in one command because they have the
// same cause and the same fix. A cached copy that no longer hashes correctly
// means something modified it locally. An archive that no longer downloads
// means link rot — the exposure this design accepts in exchange for not
// hosting package bytes, and the one pacman's mirror network does not have.
//
// Primarily a CI and maintainer tool. It is a separate command rather than a
// flag on install because install must stay fast and offline-capable, while
// this deliberately reaches the network for every dependency.
func cmdVerify(ctx context.Context, f cliFlags) int {
	m, lock, err := f.project()
	if err != nil {
		return fail(err)
	}
	_ = m

	if len(lock.Packages) == 0 {
		fmt.Println("Nothing locked — run `hover hpm install` first.")
		return 0
	}

	type finding struct {
		name string
		msg  string
		bad  bool
	}

	var mu sync.Mutex
	var findings []finding
	var wg sync.WaitGroup
	opts := f.options()
	sem := make(chan struct{}, opts.concurrency())

	for _, p := range lock.Packages {
		wg.Add(1)
		go func(p LockedPackage) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			add := func(bad bool, format string, args ...any) {
				mu.Lock()
				findings = append(findings, finding{name: p.Name, msg: fmt.Sprintf(format, args...), bad: bad})
				mu.Unlock()
			}

			dir, err := CacheDir(p.Hash)
			if err != nil {
				add(true, "%v", err)
				return
			}

			// 1. Local integrity: is what we have still what was recorded?
			if dirExists(dir) {
				actual, err := HashTree(dir)
				if err != nil {
					add(true, "could not hash the cached copy: %v", err)
					return
				}
				if actual != p.Hash {
					add(true, "the CACHED COPY has been modified\n      expected %s\n      actual   %s\n      remove it and re-run `hover hpm install`: %s",
						p.Hash, actual, dir)
					return
				}
			} else if f.Offline {
				add(true, "not in the local cache")
				return
			}

			// 2. Upstream availability: is the archive still there, and
			//    still the same bytes?
			if f.Offline {
				add(false, "cached copy is intact (upstream not checked: --offline)")
				return
			}
			if p.Git {
				add(false, "cached copy is intact (upstream not checked: git transport)")
				return
			}

			// Fetch into a throwaway staging area rather than trusting the
			// cache hit, since the point is to test upstream.
			if err := probeUpstream(ctx, p); err != nil {
				add(true, "%v", err)
				return
			}
			add(false, "ok")
		}(p)
	}
	wg.Wait()

	sort.Slice(findings, func(i, j int) bool { return findings[i].name < findings[j].name })

	bad := 0
	for _, fnd := range findings {
		mark := "  ok  "
		if fnd.bad {
			mark = "  FAIL"
			bad++
		}
		fmt.Printf("%s  %-24s %s\n", mark, fnd.name, fnd.msg)
	}

	if bad > 0 {
		fmt.Fprintf(os.Stderr, "\n%d of %d package(s) failed verification.\n", bad, len(lock.Packages))
		return 1
	}
	fmt.Printf("\nAll %d package(s) verified.\n", len(lock.Packages))
	return 0
}

// probeUpstream re-downloads a package and confirms it still hashes to the
// locked value.
//
// This deliberately does not use the cache: a cache hit would prove only
// that we downloaded it once, which is not the question. Fetch already
// verifies the hash and refuses to publish a mismatch, so a nil return means
// upstream is both reachable and unchanged.
func probeUpstream(ctx context.Context, p LockedPackage) error {
	// Fetch short-circuits on a cache hit, so temporarily ask for no
	// expected hash and compare ourselves — that path always downloads.
	staging, err := newStagingDir()
	if err != nil {
		return err
	}
	defer os.RemoveAll(staging)

	format, err := FormatForURL(p.URL)
	if err != nil {
		return err
	}
	body, err := httpGet(ctx, p.URL)
	if err != nil {
		return err
	}
	defer body.Close()

	root, err := ExtractArchive(body, format, staging)
	if err != nil {
		return fmt.Errorf("upstream archive is corrupt: %w", err)
	}
	actual, err := HashTree(root)
	if err != nil {
		return err
	}
	if actual != p.Hash {
		return fmt.Errorf("UPSTREAM CHANGED\n      expected %s\n      actual   %s\n      %s",
			p.Hash, actual, p.URL)
	}
	return nil
}
