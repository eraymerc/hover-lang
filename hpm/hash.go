package hpm

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// HashTree computes the content hash of a package directory: a single
// sha256 over every file's path and bytes, in sorted order.
//
// Properties this has to have, and why:
//
//   - Deterministic across machines. Directory iteration order is not, so
//     paths are sorted, and separators are normalised to '/' so a hash
//     computed on Windows matches one computed on Linux.
//   - Sensitive to structure, not just content. The path and its length go
//     into the digest, so moving a file between directories changes the
//     hash — otherwise two different trees could collide by having the same
//     bytes arranged differently.
//   - Sensitive to the executable bit, which is the one file mode that can
//     change behaviour.
//
// Excluded: .git (a clone's object database differs between fetch methods
// for identical content, so including it would make the hash unreproducible)
// and hover.lock (a package's own lockfile is about its development, not its
// content as a dependency).
func HashTree(root string) (string, error) {
	type entry struct {
		rel  string
		path string
		exec bool
	}
	var entries []entry

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		rel = filepath.ToSlash(rel)
		if rel == "." {
			return nil
		}
		if d.IsDir() {
			if isExcludedDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if isExcludedFile(rel) {
			return nil
		}
		// Symlinks are not followed and not hashed: a package that needs one
		// is doing something a source-only package should not, and following
		// them would let a link out of the tree change the hash of files
		// that are not part of the package.
		if d.Type()&fs.ModeSymlink != 0 {
			return nil
		}
		info, infoErr := d.Info()
		if infoErr != nil {
			return infoErr
		}
		entries = append(entries, entry{rel: rel, path: path, exec: info.Mode()&0111 != 0})
		return nil
	})
	if err != nil {
		return "", err
	}

	sort.Slice(entries, func(i, j int) bool { return entries[i].rel < entries[j].rel })

	h := sha256.New()
	fmt.Fprintf(h, "hover-tree-v1\n%d\n", len(entries))
	for _, e := range entries {
		mode := "0644"
		if e.exec {
			mode = "0755"
		}
		f, err := os.Open(e.path)
		if err != nil {
			return "", err
		}
		info, err := f.Stat()
		if err != nil {
			f.Close()
			return "", err
		}
		// Length-prefixed, so no combination of file names and contents can
		// be rearranged into the same byte stream.
		fmt.Fprintf(h, "%d %s %s %d\n", len(e.rel), e.rel, mode, info.Size())
		if _, err := io.Copy(h, f); err != nil {
			f.Close()
			return "", err
		}
		f.Close()
		h.Write([]byte("\n"))
	}

	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}

func isExcludedDir(name string) bool {
	return name == ".git"
}

func isExcludedFile(rel string) bool {
	return strings.EqualFold(filepath.Base(rel), LockName)
}
