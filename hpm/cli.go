package hpm

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
)

const Usage = `hpm — the Hover package manager

Usage: hover hpm <command> [arguments]
       hpm <command> [arguments]          (via the bundled symlink)

Commands:
  init [name]              Create a hover.toml in the current directory.
  install                  Restore everything from hover.toml + hover.lock.
  install <package>        Add a package from an index, then install it.
  install <archive-url>    Add an unindexed package by archive URL.
  update [package...]      Move to newer versions and rewrite the lockfile.
  remove <package>         Drop a dependency.
  list                     Show what this project depends on.
  verify                   Re-check every locked package against its hash.
  index add <url>          Trust an additional index.
  index list               Show configured indexes.
  index remove <name>      Stop using an index.
  clean                    Remove cached packages no longer referenced.

Options:
  --offline                Never touch the network; fail if something is missing.
  --locked                 Fail rather than change hover.lock (use this in CI).
  --name <name>            With "install <url>", the name to install it as.
  --git                    With "install <url>", use the git transport (private repos).
  --rev <ref>              With --git, the tag, branch or commit to check out.
  --manifest <path>        Use this hover.toml instead of searching upwards.
  -j <n>                   Maximum simultaneous downloads.

Packages are archives (.tar.gz or .tar.zst) verified against a content hash.
hover-lang.org hosts an index of pointers, never package bytes; see
docs/package-manager-design.md.`

// Run executes an hpm command line and returns a process exit code.
//
// Returns a code rather than calling os.Exit so that `hover hpm ...` and the
// `hpm` symlink share one implementation, and so tests can call it.
func Run(args []string) int {
	// Ctrl-C cancels in-flight downloads instead of leaving the process to
	// finish a fetch nobody is waiting for. Staging directories are removed
	// by deferred cleanup, so an interrupted install leaves no debris in the
	// cache.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	if len(args) == 0 {
		fmt.Println(Usage)
		return 0
	}

	cmd := args[0]
	rest := args[1:]

	switch cmd {
	case "-h", "--help", "help":
		fmt.Println(Usage)
		return 0
	}

	flags, positional, err := parseFlags(rest)
	if err != nil {
		return fail(err)
	}

	switch cmd {
	case "init":
		return cmdInit(positional)
	case "install", "i", "add":
		return cmdInstall(ctx, flags, positional)
	case "update", "upgrade":
		return cmdUpdate(ctx, flags, positional)
	case "remove", "rm", "uninstall":
		return cmdRemove(ctx, flags, positional)
	case "list", "ls":
		return cmdList(flags)
	case "verify":
		return cmdVerify(ctx, flags)
	case "index":
		return cmdIndex(ctx, flags, positional)
	case "clean":
		return cmdClean(flags)
	default:
		fmt.Fprintf(os.Stderr, "hpm: unknown command %q\n\n%s\n", cmd, Usage)
		return 1
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// FLAGS
// ─────────────────────────────────────────────────────────────────────────────

type cliFlags struct {
	Offline  bool
	Locked   bool
	Name     string
	Git      bool
	Rev      string
	Manifest string
	Jobs     int
}

// parseFlags separates flags from positional arguments, in any order. An
// unrecognized flag is an error rather than being silently ignored: a
// swallowed --locked in CI would mean the guarantee it was added for simply
// isn't there, with nothing to indicate that.
func parseFlags(args []string) (cliFlags, []string, error) {
	var f cliFlags
	var positional []string

	need := func(i int, name string) (string, int, error) {
		if i+1 >= len(args) {
			return "", i, fmt.Errorf("%s requires a value", name)
		}
		return args[i+1], i + 1, nil
	}

	for i := 0; i < len(args); i++ {
		arg := args[i]
		var err error
		switch {
		case arg == "--offline":
			f.Offline = true
		case arg == "--locked", arg == "--frozen":
			f.Locked = true
		case arg == "--git":
			f.Git = true
		case arg == "--name":
			f.Name, i, err = need(i, "--name")
		case strings.HasPrefix(arg, "--name="):
			f.Name = strings.TrimPrefix(arg, "--name=")
		case arg == "--rev":
			f.Rev, i, err = need(i, "--rev")
		case strings.HasPrefix(arg, "--rev="):
			f.Rev = strings.TrimPrefix(arg, "--rev=")
		case arg == "--manifest":
			f.Manifest, i, err = need(i, "--manifest")
		case strings.HasPrefix(arg, "--manifest="):
			f.Manifest = strings.TrimPrefix(arg, "--manifest=")
		case arg == "-j":
			var v string
			v, i, err = need(i, "-j")
			if err == nil {
				f.Jobs, err = atoiPositive(v)
			}
		case strings.HasPrefix(arg, "-j"):
			f.Jobs, err = atoiPositive(strings.TrimPrefix(arg, "-j"))
		case strings.HasPrefix(arg, "-"):
			err = fmt.Errorf("unknown option %q", arg)
		default:
			positional = append(positional, arg)
		}
		if err != nil {
			return f, nil, err
		}
	}
	return f, positional, nil
}

func atoiPositive(s string) (int, error) {
	n := 0
	if s == "" {
		return 0, fmt.Errorf("-j requires a number")
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, fmt.Errorf("-j expects a number, got %q", s)
		}
		n = n*10 + int(s[i]-'0')
	}
	if n == 0 {
		return 0, fmt.Errorf("-j must be at least 1")
	}
	return n, nil
}

func (f cliFlags) options() Options {
	return Options{
		Offline:     f.Offline,
		Locked:      f.Locked,
		Concurrency: f.Jobs,
	}
}

// project locates and loads the manifest for the current command.
func (f cliFlags) project() (*Manifest, *Lockfile, error) {
	dir := ""
	if f.Manifest != "" {
		abs, err := filepath.Abs(f.Manifest)
		if err != nil {
			return nil, nil, err
		}
		if info, err := os.Stat(abs); err == nil && info.IsDir() {
			dir = abs
		} else {
			dir = filepath.Dir(abs)
		}
	} else {
		cwd, err := os.Getwd()
		if err != nil {
			return nil, nil, err
		}
		dir, err = FindProjectRoot(cwd)
		if err != nil {
			return nil, nil, err
		}
	}

	m, err := LoadManifest(dir)
	if err != nil {
		return nil, nil, err
	}
	lock, _, err := LoadLockfile(dir)
	if err != nil {
		return nil, nil, err
	}
	return m, lock, nil
}

func fail(err error) int {
	fmt.Fprintf(os.Stderr, "hpm: %v\n", err)
	return 1
}

// ─────────────────────────────────────────────────────────────────────────────
// COMMANDS
// ─────────────────────────────────────────────────────────────────────────────

func cmdInit(args []string) int {
	cwd, err := os.Getwd()
	if err != nil {
		return fail(err)
	}
	name := ""
	if len(args) > 0 {
		name = args[0]
	}
	m, err := InitManifest(cwd, name)
	if err != nil {
		return fail(err)
	}
	fmt.Printf("Created %s\n", m.Path)
	return 0
}

// cmdInstall covers all three forms, distinguished by the argument:
//
//	install            restore from the lockfile
//	install foo        add an indexed package
//	install <url>      add an unindexed package
//
// One verb, matching `npm install` / `cargo fetch` / `go mod download`. It
// takes no filename: the manifest is found by walking up from cwd, so it
// works from anywhere inside a project.
func cmdInstall(ctx context.Context, f cliFlags, args []string) int {
	m, lock, err := f.project()
	if err != nil {
		return fail(err)
	}

	// The manifest is restored if resolution fails. Adding a dependency and
	// installing it is one action from the user's point of view, so a failed
	// install must not leave hover.toml naming something that could never be
	// installed — the next plain `hpm install`, in CI or by a colleague,
	// would then fail for a reason nobody chose.
	var restore []byte
	if len(args) > 0 {
		if f.Locked {
			return fail(fmt.Errorf("--locked was given, but adding a dependency changes %s", ManifestName))
		}
		if restore, err = os.ReadFile(m.Path); err != nil {
			return fail(err)
		}
		for _, arg := range args {
			if err := addDependency(m, f, arg); err != nil {
				return fail(err)
			}
		}
		if err := m.Save(); err != nil {
			return fail(err)
		}
		// Re-read, so resolution runs against exactly the bytes on disk
		// rather than an in-memory model that could differ from them.
		m, lock, err = f.project()
		if err != nil {
			os.WriteFile(m.Path, restore, 0644)
			return fail(err)
		}
	}

	code := resolveAndLock(ctx, m, lock, f.options())
	if code != 0 && restore != nil {
		if werr := writeFileAtomic(m.Path, restore); werr == nil {
			fmt.Fprintf(os.Stderr, "hpm: %s was left unchanged.\n", ManifestName)
		}
	}
	return code
}

// addDependency records one new dependency in the manifest, choosing the
// form from the argument's shape.
func addDependency(m *Manifest, f cliFlags, arg string) error {
	if !LooksLikeURL(arg) {
		// An index package, optionally with a version: `foo` or `foo@^1.2`.
		name, version := arg, "*"
		if i := strings.LastIndexByte(arg, '@'); i > 0 {
			name, version = arg[:i], arg[i+1:]
		}
		if _, bare, _ := splitQualified(name); true {
			if err := validPackageName(bare); err != nil {
				return err
			}
		}
		if err := ValidateRequirement(version); err != nil {
			return err
		}
		if idx, _, qualified := splitQualified(name); qualified && !m.hasIndex(idx) {
			return fmt.Errorf("index %q is not configured — add it with `hover hpm index add <url> --name %s`", idx, idx)
		}
		m.AddIndexedDep(name, version)
		return nil
	}

	name := f.Name
	if name == "" {
		name = PackageNameFromURL(arg)
	}
	if err := validPackageName(name); err != nil {
		return fmt.Errorf("could not derive a package name from %s (%w) — pass --name", arg, err)
	}

	if f.Git {
		m.AddGitDep(name, arg, f.Rev)
		return nil
	}
	if f.Rev != "" {
		return fmt.Errorf("--rev only applies with --git; an archive URL already names one exact release")
	}
	if _, err := FormatForURL(arg); err != nil {
		return fmt.Errorf("%w\nIf this is a git repository rather than an archive, use --git (it needs git installed, and is meant for private repos)", err)
	}
	m.AddURLDep(name, arg)
	return nil
}

func cmdUpdate(ctx context.Context, f cliFlags, args []string) int {
	m, lock, err := f.project()
	if err != nil {
		return fail(err)
	}
	if f.Locked {
		return fail(fmt.Errorf("--locked and `update` contradict each other: update exists to change %s", LockName))
	}
	for _, name := range args {
		if _, ok := m.Dep(name); !ok {
			return fail(fmt.Errorf("%q is not a dependency of this project", name))
		}
	}
	opts := f.options()
	opts.Update = true
	opts.UpdateOnly = args
	return resolveAndLock(ctx, m, lock, opts)
}

func cmdRemove(ctx context.Context, f cliFlags, args []string) int {
	if len(args) == 0 {
		return fail(fmt.Errorf("remove needs a package name"))
	}
	m, lock, err := f.project()
	if err != nil {
		return fail(err)
	}
	for _, name := range args {
		if !m.RemoveDep(name) {
			return fail(fmt.Errorf("%q is not a dependency of this project", name))
		}
		lock.Remove(name)
		fmt.Printf("Removed %s\n", name)
	}
	if err := m.Save(); err != nil {
		return fail(err)
	}

	// Re-resolve rather than only deleting the entry: dropping a dependency
	// can also drop everything it alone pulled in, and a lockfile still
	// listing those would keep them pinned forever.
	m, lock, err = f.project()
	if err != nil {
		return fail(err)
	}
	return resolveAndLock(ctx, m, lock, f.options())
}

func cmdList(f cliFlags) int {
	m, lock, err := f.project()
	if err != nil {
		return fail(err)
	}
	if len(m.Deps) == 0 {
		fmt.Printf("%s declares no dependencies.\n", m.Path)
		return 0
	}

	deps := append([]Dependency(nil), m.Deps...)
	sort.Slice(deps, func(i, j int) bool { return deps[i].Name < deps[j].Name })

	fmt.Printf("Dependencies of %s:\n", m.Path)
	for _, d := range deps {
		locked, isLocked := lock.Get(d.Name)
		status := "not installed"
		if isLocked {
			dir, _ := CacheDir(locked.Hash)
			status = locked.Version
			if status == "" {
				status = "pinned"
			}
			if !dirExists(dir) {
				status += " (not in cache — run `hover hpm install`)"
			}
		}
		fmt.Printf("  %-24s %-14s %s\n", d.Name, requirementOrDash(d), status)
	}

	// Transitive packages are in the lockfile but not the manifest; showing
	// them separately keeps "what I asked for" distinct from "what that
	// pulled in", which is the distinction people actually want when a name
	// they never typed shows up in a build.
	var indirect []LockedPackage
	for _, p := range lock.Packages {
		if _, direct := m.Dep(p.Name); !direct {
			indirect = append(indirect, p)
		}
	}
	if len(indirect) > 0 {
		sort.Slice(indirect, func(i, j int) bool { return indirect[i].Name < indirect[j].Name })
		fmt.Println("\nPulled in by those:")
		for _, p := range indirect {
			fmt.Printf("  %-24s %s\n", p.Name, p.Version)
		}
	}
	return 0
}

func requirementOrDash(d Dependency) string {
	switch {
	case d.Git != "":
		return "git"
	case d.URL != "":
		return "url"
	case d.Version == "":
		return "*"
	}
	return d.Version
}

func cmdIndex(ctx context.Context, f cliFlags, args []string) int {
	if len(args) == 0 {
		return fail(fmt.Errorf("index needs a subcommand: add, list or remove"))
	}
	m, _, err := f.project()
	if err != nil {
		return fail(err)
	}

	switch args[0] {
	case "list", "ls":
		for _, ix := range m.AllIndexes() {
			handle, err := OpenIndex(ix.Name, ix.URL)
			state := "never synced"
			if err == nil && handle.Synced() {
				state = "synced"
				if age, ok := handle.Age(); ok {
					state = fmt.Sprintf("synced %s ago", roundDuration(age))
				}
			}
			fmt.Printf("  %-16s %-60s %s\n", ix.Name, ix.URL, state)
		}
		return 0

	case "add":
		if len(args) < 2 {
			return fail(fmt.Errorf("index add needs a URL"))
		}
		url := args[1]
		name := f.Name
		if name == "" {
			name = PackageNameFromURL(url)
		}
		if err := validIndexName(name); err != nil {
			return fail(fmt.Errorf("%w — pass --name", err))
		}
		if _, exists := m.IndexURL(name); exists {
			return fail(fmt.Errorf("an index named %q is already configured", name))
		}
		if _, err := FormatForURL(url); err != nil {
			return fail(err)
		}

		// The one prompt in hpm. Adding an index is a real, consequential
		// trust decision — you are agreeing to accept code this index points
		// at. Nothing else prompts, because a prompt whose answer is always
		// yes trains people to hit enter unread, spending the attention that
		// should be reserved for exactly this.
		if !confirmIndexAdd(name, url) {
			fmt.Println("Cancelled. Nothing was changed.")
			return 1
		}

		m.AddIndex(name, url)
		if err := m.Save(); err != nil {
			return fail(err)
		}
		fmt.Printf("Added index %q to %s\n", name, m.Path)

		handle, err := OpenIndex(name, url)
		if err != nil {
			return fail(err)
		}
		if err := handle.Sync(ctx); err != nil {
			return fail(err)
		}
		names, _ := handle.List()
		fmt.Printf("Synced %q — %d package(s). Install from it with `hover hpm install %s:<package>`.\n",
			name, len(names), name)
		return 0

	case "remove", "rm":
		if len(args) < 2 {
			return fail(fmt.Errorf("index remove needs a name"))
		}
		name := args[1]
		if name == OfficialIndex {
			return fail(fmt.Errorf("the %q index cannot be removed", OfficialIndex))
		}
		dropped, ok := m.RemoveIndex(name)
		if !ok {
			return fail(fmt.Errorf("no index named %q is configured", name))
		}
		if err := m.Save(); err != nil {
			return fail(err)
		}
		fmt.Printf("Removed index %q\n", name)
		for _, d := range dropped {
			fmt.Printf("  also removed dependency %s (it came from that index)\n", d)
		}
		if dir, err := IndexDir(name); err == nil {
			os.RemoveAll(dir)
		}
		return 0

	default:
		return fail(fmt.Errorf("unknown index subcommand %q — expected add, list or remove", args[0]))
	}
}

func cmdClean(f cliFlags) int {
	m, lock, err := f.project()
	if err != nil {
		return fail(err)
	}
	_ = m

	root, err := CacheRoot()
	if err != nil {
		return fail(err)
	}
	keep := map[string]bool{}
	for _, p := range lock.Packages {
		if dir, err := CacheDir(p.Hash); err == nil {
			keep[filepath.Base(dir)] = true
		}
	}

	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Println("Nothing cached.")
			return 0
		}
		return fail(err)
	}

	removed := 0
	for _, e := range entries {
		if !e.IsDir() || keep[e.Name()] {
			continue
		}
		// Only this project's lockfile is consulted, so anything another
		// project still uses would be removed too — which is safe precisely
		// because the cache is content-addressed: the next install refetches
		// it under the same hash and nothing is lost but bandwidth.
		if err := os.RemoveAll(filepath.Join(root, e.Name())); err == nil {
			removed++
		}
	}
	fmt.Printf("Removed %d cached package(s).\n", removed)
	return 0
}

// ─────────────────────────────────────────────────────────────────────────────
// SHARED
// ─────────────────────────────────────────────────────────────────────────────

// resolveAndLock is the body of install, update and remove: resolve, report,
// write the lockfile.
func resolveAndLock(ctx context.Context, m *Manifest, lock *Lockfile, opts Options) int {
	if len(m.Deps) == 0 && len(lock.Packages) == 0 {
		fmt.Printf("%s declares no dependencies — nothing to install.\n", m.Path)
		return 0
	}

	r := NewResolver(m, lock, opts)
	packages, err := r.Resolve(ctx)
	if err != nil {
		return fail(err)
	}

	next := &Lockfile{Path: lock.Path, Packages: packages}
	if opts.Locked && !sameLock(lock, next) {
		return fail(fmt.Errorf("--locked was given but resolution changed %s", LockName))
	}
	if err := next.Save(); err != nil {
		return fail(err)
	}

	fmt.Printf("%d package(s) ready.\n", len(packages))
	return 0
}

// sameLock compares two lockfiles by content, for --locked.
func sameLock(a, b *Lockfile) bool {
	if len(a.Packages) != len(b.Packages) {
		return false
	}
	index := map[string]LockedPackage{}
	for _, p := range a.Packages {
		index[p.Name] = p
	}
	for _, p := range b.Packages {
		prev, ok := index[p.Name]
		if !ok || prev != p {
			return false
		}
	}
	return true
}

// confirmIndexAdd asks before trusting a new index.
//
// With no terminal attached the answer is no, and the command fails. That is
// the safe direction: a script that adds an index unattended should say so
// explicitly by writing the [[index]] block into hover.toml, where it is
// visible in a diff — which is the entire reason index configuration lives
// in the manifest rather than in hidden global state.
func confirmIndexAdd(name, url string) bool {
	fmt.Printf("Add index %q?\n\n  %s\n\n", name, url)
	fmt.Println("Packages from this index will be installed as " + name + ":<package> and can never")
	fmt.Println("shadow an official package name. You are trusting whoever maintains it to point")
	fmt.Println("only at code you would accept.")
	fmt.Print("\nContinue? [y/N] ")

	if !stdinIsTerminal() {
		fmt.Println("\nNot a terminal — declining. Add the [[index]] block to hover.toml instead.")
		return false
	}
	var answer string
	fmt.Scanln(&answer)
	answer = strings.ToLower(strings.TrimSpace(answer))
	return answer == "y" || answer == "yes"
}

func stdinIsTerminal() bool {
	info, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

func roundDuration(d interface{ Hours() float64 }) string {
	h := d.Hours()
	switch {
	case h < 1:
		return "less than an hour"
	case h < 48:
		return fmt.Sprintf("%.0f hours", h)
	default:
		return fmt.Sprintf("%.0f days", h/24)
	}
}
