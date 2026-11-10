package main

// The C++ runtime is shipped as SOURCE and compiled on the user's machine
// by the user's own Zig, rather than shipped as a prebuilt static library.
//
// The point is ABI safety. sim.cpp is always compiled by whatever Zig the
// user has; if the runtime archive it links against came from a different
// toolchain, you get the failure documented in .gitignore — a
// g++/libstdc++ archive against a zig/libc++ sim.o yields undefined
// std::__cxx11 symbols. Building both with the same Zig makes that class
// of mismatch structurally impossible, and as a bonus removes any need to
// know the user's target triple: a native build is just the Zig default.
//
// This runs from `hover --setup` only (see runSetup). Compiling without a
// built runtime is a hard error pointing at --setup, rather than a
// surprise minute-long stall in the middle of someone's first compile.

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
)

// runtimeLibName is the built archive's filename. One name on every
// platform: it is an internal artifact, produced and consumed only by
// hover itself, and passed to the linker by full path.
const runtimeLibName = "libhover_runtime.a"

// zigStampName records which Zig built the archive, so a later compile can
// refuse to link against an archive built by a different toolchain instead
// of failing later with unresolved C++ symbols.
const zigStampName = ".zig-version"

// cxxStd is the C++ standard for everything hover compiles: the runtime
// archive, every sim.cpp, and the link-cache warm-up. One constant because
// all three must agree — the warm-up only pays off if its flags match a real
// compile, and the runtime and sim.cpp share headers. The Makefile's
// check-runtime target mirrors it as CXX_STD.
const cxxStd = "-std=c++20"

// cxxOpt is the optimisation and floating-point contract for the same three
// compiles, for the same reason. -O3 itself keeps IEEE semantics (no
// reassociation, unlike -ffast-math), but three things around it do not hold
// by default and are pinned here:
//
//	-ffp-contract=off   clang fuses a*b + c into one FMA where the CPU has one.
//	                    The build is native (no -target), so the last bits of
//	                    a result depended on the machine — and in a switching
//	                    circuit a last-bit difference can move an edge by a
//	                    step. Off, that source of machine dependence is gone.
//	                    Not every source: the build still targets the host
//	                    CPU, so Eigen may vectorise (and order its sums)
//	                    differently with and without AVX, and exp/log/pow can
//	                    differ in the last bit between C libraries (and the
//	                    wasm playground's). Bit-identical results across
//	                    machines would also need a fixed -mcpu and libm.
//	-fwrapv             signed integer overflow (a Hover `int` counter) wraps,
//	                    instead of being undefined behaviour -O3 may exploit.
//	-fno-strict-aliasing importc/FFI code may view memory through another type.
//
// The runtime's stamp records these, so a runtime built with other flags is
// caught by checkRuntimeLib and rebuilt with `hover --setup`. The Makefile's
// check-runtime target and web/build.mjs mirror them.
var cxxOpt = []string{"-O3", "-ffp-contract=off", "-fwrapv", "-fno-strict-aliasing"}

// runtimeStamp is what the runtime archive is stamped with: the Zig version
// and the compile flags it was built with, one per line.
func runtimeStamp(zigVersion string) string {
	return zigVersion + "\n" + cxxStd + " " + strings.Join(cxxOpt, " ") + "\n"
}

// runtimeBuildDir is where compiled objects and the final archive live,
// alongside the shipped sources.
func runtimeBuildDir(runtimeDir string) string { return filepath.Join(runtimeDir, "build") }

func runtimeLibPath(runtimeDir string) string {
	return filepath.Join(runtimeBuildDir(runtimeDir), runtimeLibName)
}

func zigStampPath(runtimeDir string) string {
	return filepath.Join(runtimeBuildDir(runtimeDir), zigStampName)
}

// zigVersionOf reports the version string of the Zig at zigPath.
func zigVersionOf(zigPath string) (string, error) {
	out, err := exec.Command(zigPath, "version").Output()
	if err != nil {
		return "", fmt.Errorf("running `%s version`: %w", zigPath, err)
	}
	return strings.TrimSpace(string(out)), nil
}

// runtimeSources lists the runtime's .cpp files. Discovered by walking
// rather than hardcoded, so adding a solver needs no edit here — build/ is
// our own output and Eigen is header-only, so both are skipped.
func runtimeSources(runtimeDir string) ([]string, error) {
	var srcs []string
	err := filepath.WalkDir(runtimeDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == "build" || d.Name() == "Eigen" {
				return filepath.SkipDir
			}
			return nil
		}
		if filepath.Ext(path) == ".cpp" {
			srcs = append(srcs, path)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(srcs) // deterministic archive member order
	if len(srcs) == 0 {
		return nil, fmt.Errorf("no .cpp sources found under %s — is this a complete hover release?", runtimeDir)
	}
	return srcs, nil
}

// buildRuntime compiles every runtime source natively and archives the
// results into runtimeLibPath(runtimeDir).
func buildRuntime(zigPath, runtimeDir string) error {
	srcs, err := runtimeSources(runtimeDir)
	if err != nil {
		return err
	}

	buildDir := runtimeBuildDir(runtimeDir)
	objDir := filepath.Join(buildDir, "obj")
	if err := os.RemoveAll(objDir); err != nil {
		return fmt.Errorf("clearing %s: %w", objDir, err)
	}
	if err := os.MkdirAll(objDir, 0o755); err != nil {
		return fmt.Errorf("creating %s (is the hover install directory writable?): %w", objDir, err)
	}

	// Object names are flattened, so two sources may not share a basename.
	// They don't today, and a collision is caught here rather than silently
	// dropping a translation unit from the archive.
	objs := make([]string, len(srcs))
	seen := make(map[string]string, len(srcs))
	for i, src := range srcs {
		base := strings.TrimSuffix(filepath.Base(src), ".cpp") + ".o"
		if prev, dup := seen[base]; dup {
			return fmt.Errorf("runtime sources %s and %s share a basename", prev, src)
		}
		seen[base] = src
		objs[i] = filepath.Join(objDir, base)
	}

	fmt.Printf("[Setup] Compiling %d runtime sources with %s...\n", len(srcs), zigPath)

	// Compile in parallel: this is ~1 CPU-minute of Eigen-heavy template
	// instantiation, and doing it serially is the difference between a
	// setup step that feels instant and one that feels broken.
	workers := runtime.NumCPU()
	if workers > len(srcs) {
		workers = len(srcs)
	}

	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		firstErr error
		next     int
	)
	claim := func() int {
		mu.Lock()
		defer mu.Unlock()
		if next >= len(srcs) || firstErr != nil {
			return -1
		}
		i := next
		next++
		return i
	}
	fail := func(err error) {
		mu.Lock()
		defer mu.Unlock()
		if firstErr == nil {
			firstErr = err
		}
	}

	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				i := claim()
				if i < 0 {
					return
				}
				// No -target: a native build is the Zig default, and it
				// detects the host libc properly instead of guessing a
				// triple.
				args := append([]string{"c++", cxxStd}, cxxOpt...)
				args = append(args, "-w",
					"-I"+runtimeDir,
					"-I"+filepath.Join(runtimeDir, "Eigen"),
					"-c", srcs[i], "-o", objs[i],
				)
				out, err := exec.Command(zigPath, args...).CombinedOutput()
				if err != nil {
					fail(fmt.Errorf("compiling %s: %w\n%s", srcs[i], err, out))
				}
			}
		}()
	}
	wg.Wait()
	if firstErr != nil {
		return firstErr
	}

	libPath := runtimeLibPath(runtimeDir)
	_ = os.Remove(libPath) // `ar rcs` updates in place; start clean
	arArgs := append([]string{"ar", "rcs", libPath}, objs...)
	if out, err := exec.Command(zigPath, arArgs...).CombinedOutput(); err != nil {
		return fmt.Errorf("archiving %s: %w\n%s", libPath, err, out)
	}

	version, err := zigVersionOf(zigPath)
	if err != nil {
		return err
	}
	if err := os.WriteFile(zigStampPath(runtimeDir), []byte(runtimeStamp(version)), 0o644); err != nil {
		return fmt.Errorf("writing zig version stamp: %w", err)
	}

	// The objects are only inputs to the archive; keeping ~100MB of them
	// around in a shipped install buys nothing, since --setup always
	// rebuilds from scratch anyway.
	_ = os.RemoveAll(objDir)

	fmt.Printf("[Setup] Runtime built: %s (zig %s)\n", libPath, version)

	warmLinkCache(zigPath)
	return nil
}

// warmLinkCache performs one throwaway link so Zig builds and caches its
// libc++/compiler-rt now. Compiling with -c never triggers that, so
// without this the user's *first* real compile spends the time instead —
// and prints a few hundred warnings from inside libc++, which reads as
// "my code is broken" at exactly the worst moment. Best-effort: a failure
// here costs only the warm cache, so it is not worth failing setup over.
func warmLinkCache(zigPath string) {
	dir, err := os.MkdirTemp("", "hover-warm-*")
	if err != nil {
		return
	}
	defer os.RemoveAll(dir)

	src := filepath.Join(dir, "warm.cpp")
	if err := os.WriteFile(src, []byte("#include <string>\nint main(){return (int)std::string(\"\").size();}\n"), 0o644); err != nil {
		return
	}

	fmt.Println("[Setup] Warming Zig's C++ link cache...")
	// Same flags as a real compile, so the cached artifacts actually hit.
	args := append([]string{"c++", cxxStd}, cxxOpt...)
	args = append(args, "-w", src, "-o", filepath.Join(dir, "warm"))
	cmd := exec.Command(zigPath, args...)
	_ = cmd.Run()
}

// checkRuntimeLib returns the runtime archive path, or an error explaining
// which --setup-shaped problem needs fixing. Verifying the stamp on every
// compile costs one `zig version` spawn and catches a toolchain swap
// before it turns into unresolved C++ symbols at link time.
func checkRuntimeLib(zigPath, runtimeDir string) (string, error) {
	libPath := runtimeLibPath(runtimeDir)
	if _, err := os.Stat(libPath); err != nil {
		return "", fmt.Errorf("[Compile] Runtime library not built yet. Run `hover --setup` first")
	}

	stamp, err := os.ReadFile(zigStampPath(runtimeDir))
	if err != nil {
		return "", fmt.Errorf("[Compile] Runtime library has no Zig version stamp — rebuild it with `hover --setup`")
	}
	lines := strings.SplitN(strings.TrimSpace(string(stamp)), "\n", 2)
	built := strings.TrimSpace(lines[0])

	current, err := zigVersionOf(zigPath)
	if err != nil {
		return "", fmt.Errorf("[Compile] %w", err)
	}
	if built != current {
		return "", fmt.Errorf("[Compile] Runtime library was built with Zig %s but you now have Zig %s. "+
			"Linking across toolchain versions produces undefined C++ symbols — run `hover --setup` to rebuild", built, current)
	}
	// The second line is the compile flags (runtimeStamp). A runtime built
	// with other floating-point or overflow rules than sim.cpp gets would mix
	// two arithmetics in one simulation; a stamp without the line predates it.
	if want := strings.TrimSpace(strings.SplitN(runtimeStamp(current), "\n", 2)[1]); len(lines) < 2 || strings.TrimSpace(lines[1]) != want {
		return "", fmt.Errorf("[Compile] Runtime library was built with different compiler flags than this hover uses (%s) — run `hover --setup` to rebuild", want)
	}
	return libPath, nil
}
