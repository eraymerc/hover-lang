package hpm

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// ─────────────────────────────────────────────────────────────────────────────
// THE OPTIONAL GIT TRANSPORT
//
// Git is NOT how packages are normally fetched — see fetch.go. It exists
// here for exactly one case the archive transport cannot serve: a private
// repository, where the value is git's credential handling (helpers, SSH
// agents, corporate auth), not git itself.
//
// It is therefore opt-in, per dependency, and spelled differently in the
// manifest so it is visible at a glance:
//
//	[dependencies.internal-models]
//	git = "git@github.internal:eng/models.git"
//	rev = "v0.3.0"
//
// A user with no git installed can still install everything else. That is
// the whole point of keeping this off the default path: the failure is
// scoped to the dependencies that asked for it, with a message saying so,
// rather than making `hpm` useless on a machine that has only the hover
// binary.
// ─────────────────────────────────────────────────────────────────────────────

// requireGit checks git is available, with an error that says what to do
// rather than "exec: git: executable file not found in $PATH".
func requireGit(dep string) error {
	if _, err := exec.LookPath("git"); err != nil {
		return fmt.Errorf("dependency %q uses the git transport, but git was not found on PATH.\n"+
			"Install it (e.g. `sudo pacman -S git`, `sudo apt install git`), or point the dependency at an\n"+
			"archive URL instead — every other dependency installs without git.", dep)
	}
	return nil
}

// runGit executes git in dir and returns its stdout, trimmed. Stderr is
// folded into the error, because git's own diagnostics ("Repository not
// found", "could not read Username") are the ones a user needs to see.
func runGit(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	// Never prompt. A package manager that blocks on a hidden credential
	// prompt hangs CI with no output — the same failure mode the design
	// rules out for interactive install prompts. A configured helper still
	// works; only interactive fallback is disabled.
	cmd.Env = append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0",
		"GCM_INTERACTIVE=never",
	)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), msg)
	}
	return strings.TrimSpace(string(out)), nil
}

// FetchGit clones url at ref into the content-addressed cache.
//
// Returns the same FetchResult as the HTTP path, and goes through the same
// publish() — so a git-sourced package is hashed, verified and cached
// identically. The transport differs; the trust model does not.
func FetchGit(ctx context.Context, name, url, ref, expectHash string) (FetchResult, error) {
	if expectHash != "" {
		dir, err := CacheDir(expectHash)
		if err != nil {
			return FetchResult{}, err
		}
		if dirExists(dir) {
			return FetchResult{Hash: expectHash, Dir: dir, Cached: true}, nil
		}
	}
	if err := requireGit(name); err != nil {
		return FetchResult{}, err
	}

	staging, err := newStagingDir()
	if err != nil {
		return FetchResult{}, err
	}
	defer os.RemoveAll(staging)

	work := filepath.Join(staging, "pkg")
	if err := gitFetchRef(ctx, url, ref, work); err != nil {
		return FetchResult{}, fmt.Errorf("could not fetch %s%s: %w", url, refSuffix(ref), err)
	}

	// The .git directory is dropped before hashing: its contents differ
	// between fetch strategies for identical source, so keeping it would
	// make the hash depend on HOW a package was downloaded rather than on
	// what it contains — and the same package fetched over git and over
	// https must hash the same.
	if err := os.RemoveAll(filepath.Join(work, ".git")); err != nil {
		return FetchResult{}, err
	}

	return publish(work, url, expectHash)
}

// gitFetchRef checks out a single ref (tag, branch or commit sha) of url
// into dir, without history.
//
// init + fetch rather than `clone --branch`, because a raw commit sha is not
// a valid --branch argument and pinning to a commit is the point: a tag can
// be moved, a commit cannot.
func gitFetchRef(ctx context.Context, url, ref, dir string) error {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	if _, err := runGit(ctx, dir, "init", "--quiet"); err != nil {
		return err
	}
	if _, err := runGit(ctx, dir, "remote", "add", "origin", url); err != nil {
		return err
	}

	if ref == "" {
		if _, err := runGit(ctx, dir, "fetch", "--depth", "1", "--quiet", "origin", "HEAD"); err != nil {
			return err
		}
	} else if _, err := runGit(ctx, dir, "fetch", "--depth", "1", "--quiet", "origin", ref); err != nil {
		// A server with uploadpack.allowReachableSHA1InWant disabled refuses
		// a bare sha. Fall back to fetching everything and resolving locally
		// — slower, but the alternative is telling the user their perfectly
		// valid commit pin is unsupported.
		if _, err2 := runGit(ctx, dir, "fetch", "--quiet", "--tags", "origin"); err2 != nil {
			return err
		}
	}

	target := "FETCH_HEAD"
	if ref != "" && looksLikeSHA(ref) {
		target = ref
	}
	_, err := runGit(ctx, dir, "checkout", "--quiet", "--detach", target)
	return err
}

func refSuffix(ref string) string {
	if ref == "" {
		return ""
	}
	return " at " + ref
}

// looksLikeSHA reports whether ref is a full or abbreviated commit hash
// rather than a tag or branch name.
func looksLikeSHA(ref string) bool {
	if len(ref) < 7 || len(ref) > 40 {
		return false
	}
	for i := 0; i < len(ref); i++ {
		c := ref[i]
		if !(c >= '0' && c <= '9') && !(c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
