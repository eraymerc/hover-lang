package elaborator

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

func ParseEngineering(s string) float64 {
	u := map[string]float64{"p": 1e-12, "n": 1e-9, "u": 1e-6, "m": 1e-3, "k": 1e3, "Meg": 1e6, "G": 1e9}
	for k, v := range u {
		if strings.HasSuffix(s, k) {
			val, _ := strconv.ParseFloat(strings.TrimSuffix(s, k), 64)
			return val * v
		}
	}
	val, _ := strconv.ParseFloat(s, 64)
	return val
}

func isBranch(t string) bool {
	t = strings.ToLower(t)
	return t == "l" || t == "inductor" || t == "voltage_source" || t == "v" || t == "vcvs" || t == "ccvs" || t == "e" || t == "h"
}

func copyMapSF(m map[string]float64) map[string]float64 {
	c := make(map[string]float64, len(m))
	for k, v := range m {
		c[k] = v
	}
	return c
}

func copyMapSS(m map[string]string) map[string]string {
	c := make(map[string]string, len(m))
	for k, v := range m {
		c[k] = v
	}
	return c
}

// splitQualifiedName splits a possibly-aliased name like "Motor.PMSM" into
// ("Motor", "PMSM", true). For an unqualified name like "PMSM", it returns
// ("", "PMSM", false).
//
// Only the FIRST dot matters — "Motor.PMSM" splits into "Motor" and "PMSM",
// not further. This matches how module/function names are written: aliases
// are always exactly one segment, since imports are non-transitive (you
// can't write "Motor.SubAlias.Thing").
func splitQualifiedName(name string) (alias string, bare string, isQualified bool) {
	idx := strings.IndexByte(name, '.')
	if idx == -1 {
		return "", name, false
	}
	return name[:idx], name[idx+1:], true
}

// resolveImportPathFor resolves an import path the same way the loader
// package does — relative to the directory of the file that wrote the
// import statement. Kept as a small local copy rather than importing the
// loader package here, to avoid the elaborator depending on file-system
// layout concerns beyond this one calculation; the loader remains the
// single source of truth for the actual file discovery and cycle checks.
func resolveImportPathFor(currentFilePath, importPath string, isSystem bool) string {
	if isSystem {
		if root, err := stdlibRoot(); err == nil {
			return filepath.Clean(filepath.Join(root, filepath.FromSlash(strings.TrimLeft(importPath, "/"))))
		}
	}
	return filepath.Clean(filepath.Join(filepath.Dir(currentFilePath), importPath))
}

func stdlibRoot() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if real, e := filepath.EvalSymlinks(exe); e == nil {
		exe = real
	}
	return filepath.Join(filepath.Dir(exe), "standard_library"), nil
}
