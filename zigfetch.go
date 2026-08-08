package main

// Windows releases ship without a bundled Zig — hover resolves one at
// compile time instead: first a local ./toolchain/zig/zig.exe, then `zig`
// on PATH. If neither exists, it does NOT download anything inline (a
// surprise multi-hundred-MB download mid-compile, gated on a y/N prompt,
// is exactly the kind of thing that silently hangs in CI); it just points
// the user at `hover --setup`, which does the download explicitly. Linux
// never auto-downloads at all — it just tells the user to install Zig via
// their package manager (Linux's package-manager convention makes that the
// less surprising default there). See runSetup for the --setup command
// itself.
//
// zigVersion is stamped in at release-build time via:
//   go build -ldflags "-X main.zigVersion=<version>"
// (set from the Makefile's ZIG_VERSION). Left empty in ad-hoc `go build`
// runs, in which case auto-download is simply unavailable.

import (
	"archive/zip"
	"context"
	"encoding/json"
	"fmt"
	"hover/compiler/loader"
	"hover/hpm"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

var zigVersion string // stamped via -ldflags at release-build time

const zigIndexURL = "https://ziglang.org/download/index.json"

// zigDownloadKeys maps GOARCH to the key ziglang.org's index.json uses for
// a Windows host build. Keep in sync with the <id>_ZIGDL-equivalent triples
// in the Makefile's PLATFORMS matrix.
var zigDownloadKeys = map[string]string{
	"amd64": "x86_64-windows",
	"arm64": "aarch64-windows",
	"386":   "x86-windows",
}

// windowsToolchainDir returns {exeDir}/toolchain/zig, anchored to the
// executable's own directory (not cwd) — same reasoning as
// loader.ExeDir()'s other consumers.
func windowsToolchainDir() (string, error) {
	exeDir, err := loader.ExeDir()
	if err != nil {
		return "", fmt.Errorf("could not locate the hover executable's own directory: %w", err)
	}
	return filepath.Join(exeDir, "toolchain", "zig"), nil
}

// resolveWindowsZig finds a usable zig.exe: a local bundled copy, then
// PATH. It never downloads — see runSetup for that.
func resolveWindowsZig() (string, error) {
	toolchainDir, err := windowsToolchainDir()
	if err != nil {
		return "", fmt.Errorf("[Compile] %w", err)
	}
	bundled := filepath.Join(toolchainDir, "zig.exe")

	if _, err := os.Stat(bundled); err == nil {
		return bundled, nil
	}
	if p, err := exec.LookPath("zig"); err == nil {
		return p, nil
	}

	return "", fmt.Errorf("[Compile] Zig not found. Run `hover --setup` to download it, or install Zig manually and place it at %s (or put it on PATH)", bundled)
}

// setupZig ensures a usable Zig exists and returns its path. On Windows it
// will download one into ./toolchain/zig if none is reachable; on Linux it
// only reports, since installing via a package manager is the convention
// there.
func setupZig() (string, error) {
	if runtime.GOOS != "windows" {
		p, err := exec.LookPath("zig")
		if err != nil {
			return "", fmt.Errorf("[Setup] Zig not found on PATH. Install it via your package manager (e.g. `sudo pacman -S zig`, `sudo apt install zig`, `sudo dnf install zig`, `sudo pkg install zig`) and make sure it's on PATH")
		}
		fmt.Printf("[Setup] Zig found on PATH: %s\n", p)
		return p, nil
	}

	toolchainDir, err := windowsToolchainDir()
	if err != nil {
		return "", fmt.Errorf("[Setup] %w", err)
	}
	bundled := filepath.Join(toolchainDir, "zig.exe")

	if _, err := os.Stat(bundled); err == nil {
		fmt.Printf("[Setup] Zig already present: %s\n", bundled)
		return bundled, nil
	}
	if p, err := exec.LookPath("zig"); err == nil {
		fmt.Printf("[Setup] Zig found on PATH: %s (nothing to download)\n", p)
		return p, nil
	}

	key, ok := zigDownloadKeys[runtime.GOARCH]
	if !ok {
		return "", fmt.Errorf("[Setup] No automatic download is available for windows/%s. Install Zig manually and place it at %s (or put it on PATH)", runtime.GOARCH, bundled)
	}
	if zigVersion == "" {
		return "", fmt.Errorf("[Setup] This build has no pinned Zig version to download. Install Zig manually and place it at %s (or put it on PATH)", bundled)
	}

	fmt.Printf("[Setup] Downloading Zig %s for windows/%s to %s...\n", zigVersion, runtime.GOARCH, toolchainDir)
	if err := downloadZig(zigVersion, key, toolchainDir); err != nil {
		return "", fmt.Errorf("[Setup] Failed to download Zig: %w", err)
	}
	fmt.Printf("[Setup] Zig installed to %s\n", toolchainDir)
	return bundled, nil
}

// runSetup implements `hover --setup`: make sure a Zig is available, then
// build the C++ runtime from the shipped sources with that exact Zig (see
// runtimebuild.go for why it must be that one).
func runSetup() {
	zigPath, err := setupZig()
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}

	exeDir, err := loader.ExeDir()
	if err != nil {
		fmt.Printf("[Setup] Could not locate the hover executable's own directory: %v\n", err)
		os.Exit(1)
	}

	if err := buildRuntime(zigPath, filepath.Join(exeDir, "runtime")); err != nil {
		fmt.Printf("[Setup] %v\n", err)
		os.Exit(1)
	}

	// The standard library is downloaded, not bundled: releases ship no
	// stdlib/ directory, so this step is what makes `import <math>` work.
	// It runs last because it is the only part that needs the network, and
	// a failure here leaves a hover that still compiles anything importing
	// only relative files.
	if err := hpm.InstallStdlib(context.Background(), Version, os.Stdout); err != nil {
		fmt.Printf("[Setup] %v\n", err)
		os.Exit(1)
	}

	fmt.Println("[Setup] Done — hover is ready to use.")
}

type zigDownload struct {
	Tarball string `json:"tarball"`
}

// downloadZig fetches ziglang.org's download index, resolves the tarball
// URL for (version, key), and extracts it to destDir.
func downloadZig(version, key, destDir string) error {
	idx, err := fetchZigIndex()
	if err != nil {
		return err
	}

	versionKey := version
	if strings.Contains(version, "-dev.") || version == "master" {
		// Dev/master snapshots are only ever indexed under the literal key
		// "master" (never their own version string), and that key only
		// ever holds the CURRENT snapshot — an old pinned dev build's
		// download may simply no longer exist.
		masterEntry, ok := idx["master"]
		if !ok {
			return fmt.Errorf("zig download index has no master entry")
		}
		var actual string
		if raw, ok := masterEntry["version"]; ok {
			_ = json.Unmarshal(raw, &actual)
		}
		if actual != version {
			return fmt.Errorf("zig %s is no longer available (index master is now %s) — this hover build's pinned Zig version has gone stale", version, actual)
		}
		versionKey = "master"
	}

	entry, ok := idx[versionKey]
	if !ok {
		return fmt.Errorf("zig version %s not found in download index", version)
	}
	raw, ok := entry[key]
	if !ok {
		return fmt.Errorf("no %s build listed for zig %s", key, version)
	}
	var dl zigDownload
	if err := json.Unmarshal(raw, &dl); err != nil {
		return fmt.Errorf("malformed download entry for %s %s: %w", key, version, err)
	}
	if dl.Tarball == "" {
		return fmt.Errorf("no download URL listed for %s %s", key, version)
	}

	fmt.Printf("[Setup] Downloading %s...\n", dl.Tarball)
	archivePath, err := downloadToTemp(dl.Tarball)
	if err != nil {
		return err
	}
	defer os.Remove(archivePath)

	extractDir, err := os.MkdirTemp("", "zig-extract-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(extractDir)

	if err := unzip(archivePath, extractDir); err != nil {
		return err
	}

	entries, err := os.ReadDir(extractDir)
	if err != nil {
		return err
	}
	var extracted string
	for _, e := range entries {
		if e.IsDir() {
			extracted = filepath.Join(extractDir, e.Name())
			break
		}
	}
	if extracted == "" {
		return fmt.Errorf("unexpected archive layout: no top-level directory found")
	}

	if err := os.RemoveAll(destDir); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(destDir), 0o755); err != nil {
		return err
	}
	return os.Rename(extracted, destDir)
}

func fetchZigIndex() (map[string]map[string]json.RawMessage, error) {
	resp, err := http.Get(zigIndexURL)
	if err != nil {
		return nil, fmt.Errorf("fetching %s: %w", zigIndexURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetching %s: HTTP %d", zigIndexURL, resp.StatusCode)
	}
	var idx map[string]map[string]json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&idx); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", zigIndexURL, err)
	}
	return idx, nil
}

func downloadToTemp(url string) (string, error) {
	resp, err := http.Get(url)
	if err != nil {
		return "", fmt.Errorf("downloading %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("downloading %s: HTTP %d", url, resp.StatusCode)
	}

	f, err := os.CreateTemp("", "zig-*.zip")
	if err != nil {
		return "", err
	}
	defer f.Close()

	if _, err := io.Copy(f, resp.Body); err != nil {
		os.Remove(f.Name())
		return "", fmt.Errorf("saving download: %w", err)
	}
	return f.Name(), nil
}

func unzip(archivePath, destDir string) error {
	r, err := zip.OpenReader(archivePath)
	if err != nil {
		return err
	}
	defer r.Close()

	destPrefix := filepath.Clean(destDir) + string(os.PathSeparator)
	for _, f := range r.File {
		path := filepath.Join(destDir, f.Name)
		if !strings.HasPrefix(path, destPrefix) {
			return fmt.Errorf("illegal file path in archive: %s", f.Name)
		}
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(path, f.Mode()); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		if err := extractZipFile(f, path); err != nil {
			return err
		}
	}
	return nil
}

func extractZipFile(f *zip.File, destPath string) error {
	rc, err := f.Open()
	if err != nil {
		return err
	}
	defer rc.Close()

	out, err := os.OpenFile(destPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, f.Mode())
	if err != nil {
		return err
	}
	defer out.Close()

	_, err = io.Copy(out, rc)
	return err
}
