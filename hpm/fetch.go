package hpm

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ─────────────────────────────────────────────────────────────────────────────
// FETCHING
//
// A package is an archive at a URL, plus the hash of its contents. That is
// the whole model, and it is Zig's — which the design doc already named as
// the precedent to copy.
//
// There is deliberately no git here. An earlier draft cloned repositories,
// which meant every user needed the git binary installed: that contradicts
// hover's unzip-and-run distribution (Windows packages bundle Zig precisely
// so nothing else is required), cost four subprocesses per package, and
// bought nothing the hash does not already provide. A commit sha is an
// immutable name for a tree; so is a content hash, and the content hash is
// the one we must compute anyway. Git survives only as an opt-in transport
// for private repositories, in git.go, where credential helpers are the
// actual reason to want it.
// ─────────────────────────────────────────────────────────────────────────────

const (
	// fetchTimeout bounds one package download end to end.
	fetchTimeout = 5 * time.Minute

	// maxDownloadBytes caps the compressed transfer, independently of the
	// decompressed cap in archive.go. Both are needed: this one stops a
	// server streaming forever, that one stops a small archive expanding
	// forever.
	maxDownloadBytes = 64 << 20 // 64 MiB

	// maxRedirects follows the usual hop limit. Redirects are followed
	// because release-asset URLs redirect to CDNs as a matter of course.
	maxRedirects = 10
)

// httpClient is shared across the whole process on purpose.
//
// Connection reuse is the single biggest lever on install time with many
// dependencies: most packages come from a handful of hosts, and a shared
// Transport collapses N TLS handshakes into a few. Handshake setup dominates
// everything else here by orders of magnitude — far more than any codec
// choice does.
var httpClient = &http.Client{
	Timeout: fetchTimeout,
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= maxRedirects {
			return fmt.Errorf("stopped after %d redirects", maxRedirects)
		}
		return nil
	},
	Transport: &http.Transport{
		MaxIdleConns:        64,
		MaxIdleConnsPerHost: 16,
		IdleConnTimeout:     90 * time.Second,
		ForceAttemptHTTP2:   true,
	},
}

// FetchResult describes what a fetch produced, as opposed to what was asked
// for.
type FetchResult struct {
	Hash   string
	Dir    string // the cache directory the package now lives in
	Cached bool   // true if nothing was downloaded
}

// Fetch downloads the archive at url into the content-addressed cache.
//
// expectHash is the hash the caller already believes this content has (from
// an index entry or the lockfile), or "" when there is nothing to check
// against. A mismatch discards the download and fails hard — not a warning
// and not a prompt, because "the code you are about to compile is not the
// code that was reviewed" has no sensible continue-anyway path.
func Fetch(ctx context.Context, url, expectHash string) (FetchResult, error) {
	// A hash we already trust means the answer may already be on disk. This
	// is what lets a plain `hover hpm install` in a freshly cloned project
	// need no network at all, and what lets two projects share one copy of a
	// common dependency.
	if expectHash != "" {
		dir, err := CacheDir(expectHash)
		if err != nil {
			return FetchResult{}, err
		}
		if dirExists(dir) {
			return FetchResult{Hash: expectHash, Dir: dir, Cached: true}, nil
		}
	}

	format, err := FormatForURL(url)
	if err != nil {
		return FetchResult{}, err
	}
	if err := checkURLScheme(url); err != nil {
		return FetchResult{}, err
	}

	staging, err := newStagingDir()
	if err != nil {
		return FetchResult{}, err
	}
	defer os.RemoveAll(staging)

	body, err := httpGet(ctx, url)
	if err != nil {
		return FetchResult{}, err
	}
	defer body.Close()

	unpackDir := filepath.Join(staging, "unpack")
	if err := os.MkdirAll(unpackDir, 0755); err != nil {
		return FetchResult{}, err
	}

	root, err := ExtractArchive(io.LimitReader(body, maxDownloadBytes+1), format, unpackDir)
	if err != nil {
		return FetchResult{}, fmt.Errorf("%s: %w", url, err)
	}

	return publish(root, url, expectHash)
}

// publish hashes an extracted tree, verifies it against expectHash, and
// moves it into the cache under its hash.
//
// Shared by the HTTP and git transports so the verification and the atomic
// move exist in exactly one place — a second copy of "check the hash, then
// rename" is a second chance to get the order wrong.
func publish(root, source, expectHash string) (FetchResult, error) {
	hash, err := HashTree(root)
	if err != nil {
		return FetchResult{}, fmt.Errorf("could not hash %s: %w", source, err)
	}

	if expectHash != "" && hash != expectHash {
		return FetchResult{}, fmt.Errorf(
			"content of %s does not match the expected hash — refusing to install\n"+
				"  expected: %s\n"+
				"  actual:   %s\n"+
				"Something changed after this version was recorded: a moved tag, a re-uploaded\n"+
				"release asset, or tampering. Nothing was installed. If the change is legitimate,\n"+
				"`hover hpm update` will re-resolve and re-record it.",
			source, expectHash, hash)
	}

	dest, err := CacheDir(hash)
	if err != nil {
		return FetchResult{}, err
	}
	if dirExists(dest) {
		// Another project, or a concurrent run, already published exactly
		// this content. Identical hash means identical bytes.
		return FetchResult{Hash: hash, Dir: dest}, nil
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0755); err != nil {
		return FetchResult{}, err
	}
	if err := os.Rename(root, dest); err != nil {
		if dirExists(dest) {
			return FetchResult{Hash: hash, Dir: dest}, nil
		}
		return FetchResult{}, fmt.Errorf("could not publish %s into the cache: %w", source, err)
	}
	return FetchResult{Hash: hash, Dir: dest}, nil
}

// newStagingDir creates a scratch directory inside the cache root.
//
// Inside the cache root rather than the system temp dir so that publishing
// is a rename within one filesystem — atomic — instead of a cross-device
// copy that can be interrupted and leave a half-populated package looking
// fully installed.
func newStagingDir() (string, error) {
	root, err := CacheRoot()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(root, 0755); err != nil {
		return "", err
	}
	return os.MkdirTemp(root, ".staging-")
}

func httpGet(ctx context.Context, url string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("bad URL %q: %w", url, err)
	}
	req.Header.Set("User-Agent", "hover-hpm")
	req.Header.Set("Accept", "application/octet-stream, */*")

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("could not download %s: %w", url, err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("could not download %s: %s%s", url, resp.Status, statusHint(resp.StatusCode))
	}
	return resp.Body, nil
}

// statusHint turns the handful of HTTP statuses that mean something specific
// here into advice, instead of leaving the user with a bare number.
func statusHint(code int) string {
	switch code {
	case http.StatusNotFound:
		return " — the archive no longer exists at that URL (link rot, or a deleted release)"
	case http.StatusUnauthorized, http.StatusForbidden:
		return " — this looks like a private repository; use a `git = \"...\"` dependency, which can use your existing git credentials"
	case http.StatusTooManyRequests:
		return " — rate limited by the host; try again shortly"
	}
	return ""
}

// checkURLScheme requires https, with a documented escape for local testing.
//
// Plain http would let anyone on the path substitute the archive. The hash
// check would still catch that for an indexed package — but not for a first
// `hpm install <url>`, where there is no recorded hash yet, and that is
// exactly when the content is most trusted.
func checkURLScheme(url string) error {
	lower := strings.ToLower(url)
	switch {
	case strings.HasPrefix(lower, "https://"):
		return nil
	case strings.HasPrefix(lower, "http://"):
		if os.Getenv("HOVER_ALLOW_INSECURE_HTTP") == "1" {
			return nil
		}
		return fmt.Errorf("refusing to download over plain http: %s (set HOVER_ALLOW_INSECURE_HTTP=1 for a local test server)", url)
	default:
		return fmt.Errorf("unsupported URL scheme in %q — hpm fetches archives over https", url)
	}
}

// PackageNameFromURL derives a default package name from an archive URL, for
// `hover hpm install <url>` where no name was given.
//
//	https://github.com/u/hover-bjt/archive/refs/tags/v1.2.0.tar.gz -> hover-bjt
//	https://example.com/releases/my-models-0.3.1.tar.zst           -> my-models
func PackageNameFromURL(url string) string {
	s := url
	if i := strings.IndexAny(s, "?#"); i >= 0 {
		s = s[:i]
	}
	for _, f := range archiveFormats {
		if strings.HasSuffix(strings.ToLower(s), f.Suffix) {
			s = s[:len(s)-len(f.Suffix)]
			break
		}
	}

	segs := strings.Split(strings.Trim(s, "/"), "/")

	// GitHub and GitLab auto-generated archive URLs end in the REF, not the
	// project name — .../<repo>/archive/refs/tags/v1.2.0. Walking back past
	// the structural segments recovers the repository name, which is what a
	// person would call the package.
	name := ""
	for i := len(segs) - 1; i >= 0; i-- {
		seg := segs[i]
		switch seg {
		case "archive", "refs", "tags", "heads", "releases", "download", "-":
			continue
		}
		if i > 0 && isStructuralSegment(segs[i-1]) {
			continue // this segment is a ref like "v1.2.0"
		}
		name = seg
		break
	}
	if name == "" && len(segs) > 0 {
		name = segs[len(segs)-1]
	}

	name = trimVersionSuffix(name)
	return sanitizePackageName(name)
}

func isStructuralSegment(s string) bool {
	switch s {
	case "tags", "heads", "download", "refs":
		return true
	}
	return false
}

// trimVersionSuffix drops a trailing "-1.2.3" or "-v1.2.3" from a release
// asset filename, so my-models-0.3.1 becomes my-models.
func trimVersionSuffix(s string) string {
	i := strings.LastIndexByte(s, '-')
	if i <= 0 {
		return s
	}
	rest := strings.TrimPrefix(s[i+1:], "v")
	if rest == "" {
		return s
	}
	if _, err := ParseVersion(rest); err != nil {
		return s
	}
	return s[:i]
}

// sanitizePackageName keeps only characters a package name may contain, so a
// URL can never smuggle a path separator into a name that later becomes a
// directory or an index lookup.
func sanitizePackageName(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-' || c == '_' {
			b.WriteByte(c)
		}
	}
	return b.String()
}

// LooksLikeURL reports whether an install argument is a URL rather than an
// index package name. Package names are restricted to letters, digits, '-'
// and '_' precisely so this test can never be ambiguous.
func LooksLikeURL(s string) bool {
	return strings.Contains(s, "://")
}
