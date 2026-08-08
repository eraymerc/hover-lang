package hpm

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Dependency is one entry under [dependencies].
//
// Three shapes, one type:
//
//	# indexed — resolved through an index, the normal case
//	[dependencies]
//	foo = "^1.2.0"
//
//	# unindexed — a direct archive URL
//	[dependencies.vendor-models]
//	url = "https://example.com/vendor-models-0.3.1.tar.zst"
//
//	# private — the opt-in git transport, for repositories needing auth
//	[dependencies.internal-models]
//	git = "git@github.internal:eng/models.git"
//	rev = "v0.3.0"
//
// The last two are escape hatches for private and vendor packages that will
// never appear in a public index. They are deliberately more verbose than
// the indexed form: they skip index review entirely, so they should look
// different at a glance in a code review.
type Dependency struct {
	// Name is the qualified name: "foo" for the official index,
	// "myindex:foo" for an added one. The index qualifier is part of the
	// identity because two indexes may legitimately publish the same name,
	// and they are not the same package.
	Name string

	Version string // version requirement; "" for a URL or git dependency
	URL     string // direct archive URL; "" otherwise
	Git     string // git remote, for the optional git transport; "" otherwise
	Rev     string // tag/branch/commit for a git dependency
}

// Indexed reports whether this dependency resolves through an index.
func (d Dependency) Indexed() bool { return d.URL == "" && d.Git == "" }

// Source describes where this dependency comes from, for messages.
func (d Dependency) Source() string {
	switch {
	case d.URL != "":
		return d.URL
	case d.Git != "":
		return d.Git
	}
	return "index " + d.IndexName()
}

// IndexName is the index this dependency's name is qualified to.
func (d Dependency) IndexName() string {
	if idx, _, ok := splitQualified(d.Name); ok {
		return idx
	}
	return OfficialIndex
}

// BareName is the package name without its index qualifier.
func (d Dependency) BareName() string {
	if _, bare, ok := splitQualified(d.Name); ok {
		return bare
	}
	return d.Name
}

// splitQualified splits "myindex:foo" into ("myindex", "foo", true), and
// reports false for an unqualified name.
func splitQualified(name string) (index, bare string, ok bool) {
	i := strings.IndexByte(name, ':')
	if i <= 0 || i == len(name)-1 {
		return "", name, false
	}
	return name[:i], name[i+1:], true
}

// IndexSource is one entry in the [[index]] array: an additional index the
// project trusts, beyond the official one.
type IndexSource struct {
	Name string
	URL  string
}

// Manifest is a parsed hover.toml.
type Manifest struct {
	Dir  string // project root (the directory containing the file)
	Path string

	Name    string
	Version string

	Deps    []Dependency
	Indexes []IndexSource

	doc *Document
}

// LoadManifest reads the hover.toml in dir.
func LoadManifest(dir string) (*Manifest, error) {
	path := filepath.Join(dir, ManifestName)
	src, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("could not read %s: %w", path, err)
	}
	doc, err := ParseTOML(string(src))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}

	m := &Manifest{Dir: dir, Path: path, doc: doc}

	if pkg := doc.Table("package"); pkg != nil {
		m.Name, _ = pkg.Get("name")
		m.Version, _ = pkg.Get("version")
	}

	// Simple form: one key per dependency under [dependencies].
	if deps := doc.Table("dependencies"); deps != nil {
		for _, kv := range deps.Keys {
			if err := ValidateRequirement(kv.Value); err != nil {
				return nil, fmt.Errorf("%s: dependency %q: %w", path, kv.Key, err)
			}
			m.Deps = append(m.Deps, Dependency{Name: kv.Key, Version: kv.Value})
		}
	}

	// Verbose form: one [dependencies.<name>] table per URL or git dependency.
	for _, t := range doc.SubTables("dependencies") {
		url, hasURL := t.Get("url")
		git, hasGit := t.Get("git")
		switch {
		case hasURL && hasGit:
			return nil, fmt.Errorf("%s: [dependencies.%s] sets both `url` and `git` — pick one",
				path, t.Name())
		case !hasURL && !hasGit:
			return nil, fmt.Errorf("%s: [dependencies.%s] has neither `url` nor `git` — an indexed dependency is written `%s = \"<version>\"` under [dependencies] instead",
				path, t.Name(), t.Name())
		}
		rev, _ := t.Get("rev")
		version, _ := t.Get("version")
		if hasURL {
			if _, err := FormatForURL(url); err != nil {
				return nil, fmt.Errorf("%s: [dependencies.%s]: %w", path, t.Name(), err)
			}
			if rev != "" {
				return nil, fmt.Errorf("%s: [dependencies.%s] sets `rev`, which only applies to a `git` dependency — an archive URL already names an exact release",
					path, t.Name())
			}
		}
		m.Deps = append(m.Deps, Dependency{Name: t.Name(), URL: url, Git: git, Rev: rev, Version: version})
	}

	for _, t := range doc.Tables("index") {
		name, _ := t.Get("name")
		url, _ := t.Get("url")
		if name == "" || url == "" {
			return nil, fmt.Errorf("%s: an [[index]] entry needs both `name` and `url`", path)
		}
		if name == OfficialIndex {
			return nil, fmt.Errorf("%s: %q is reserved for the index shipped with hover and cannot be redeclared — pick another name (packages from it will be written %s:package)",
				path, OfficialIndex, name)
		}
		m.Indexes = append(m.Indexes, IndexSource{Name: name, URL: url})
	}

	if err := m.validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return m, nil
}

// validate catches the manifest errors that would otherwise turn into
// confusing behaviour much later — a dependency naming an index that was
// never declared being the main one, since it would simply fail to resolve
// with no hint about the missing [[index]] block.
func (m *Manifest) validate() error {
	seen := map[string]bool{}
	for _, d := range m.Deps {
		if seen[d.Name] {
			return fmt.Errorf("dependency %q is declared twice", d.Name)
		}
		seen[d.Name] = true

		if err := validPackageName(d.BareName()); err != nil {
			return err
		}
		if idx, _, qualified := splitQualified(d.Name); qualified && d.Indexed() {
			if !m.hasIndex(idx) {
				return fmt.Errorf("dependency %q refers to index %q, which is not declared — add:\n\n[[index]]\nname = %q\nurl = \"...\"",
					d.Name, idx, idx)
			}
		}
	}

	names := map[string]bool{}
	for _, ix := range m.Indexes {
		if names[ix.Name] {
			return fmt.Errorf("index %q is declared twice", ix.Name)
		}
		names[ix.Name] = true
	}
	return nil
}

func (m *Manifest) hasIndex(name string) bool {
	if name == OfficialIndex {
		return true
	}
	for _, ix := range m.Indexes {
		if ix.Name == name {
			return true
		}
	}
	return false
}

// Dep returns the dependency with the given qualified name.
func (m *Manifest) Dep(name string) (Dependency, bool) {
	for _, d := range m.Deps {
		if d.Name == name {
			return d, true
		}
	}
	return Dependency{}, false
}

// IndexURL returns the clone URL for a named index.
func (m *Manifest) IndexURL(name string) (string, bool) {
	if name == OfficialIndex {
		return officialIndexURL(), true
	}
	for _, ix := range m.Indexes {
		if ix.Name == name {
			return ix.URL, true
		}
	}
	return "", false
}

// AllIndexes lists every index this project resolves against, official first.
func (m *Manifest) AllIndexes() []IndexSource {
	out := []IndexSource{{Name: OfficialIndex, URL: officialIndexURL()}}
	out = append(out, m.Indexes...)
	return out
}

// ─────────────────────────────────────────────────────────────────────────────
// MUTATION
// ─────────────────────────────────────────────────────────────────────────────

// AddIndexedDep records `name = version` under [dependencies], replacing any
// existing entry for that name.
func (m *Manifest) AddIndexedDep(name, version string) {
	m.doc.RemoveTable("dependencies", name) // in case it was previously a URL dep
	m.doc.Set([]string{"dependencies"}, name, version)
}

// AddURLDep records a [dependencies.<name>] block for a direct archive URL.
func (m *Manifest) AddURLDep(name, url string) {
	m.doc.Remove([]string{"dependencies"}, name) // in case it was previously indexed
	m.doc.RemoveTable("dependencies", name)
	m.doc.Set([]string{"dependencies", name}, "url", url)
}

// AddGitDep records a [dependencies.<name>] block using the optional git
// transport.
func (m *Manifest) AddGitDep(name, git, rev string) {
	m.doc.Remove([]string{"dependencies"}, name)
	m.doc.RemoveTable("dependencies", name)
	m.doc.Set([]string{"dependencies", name}, "git", git)
	if rev != "" {
		m.doc.Set([]string{"dependencies", name}, "rev", rev)
	}
}

// RemoveDep drops a dependency in whichever form it was written.
func (m *Manifest) RemoveDep(name string) bool {
	removed := m.doc.Remove([]string{"dependencies"}, name)
	if m.doc.RemoveTable("dependencies", name) {
		removed = true
	}
	return removed
}

// AddIndex appends an [[index]] block.
func (m *Manifest) AddIndex(name, url string) {
	lines := m.doc.String()
	if lines != "" && !strings.HasSuffix(lines, "\n") {
		lines += "\n"
	}
	lines += fmt.Sprintf("\n[[index]]\nname = %q\nurl = %q\n", name, url)
	if doc, err := ParseTOML(lines); err == nil {
		m.doc = doc
	}
}

// RemoveIndex drops the [[index]] block with the given name, along with the
// dependencies qualified to it — leaving those behind would produce a
// manifest that no longer validates, so the removal has to be complete or
// not happen at all.
func (m *Manifest) RemoveIndex(name string) (removedDeps []string, ok bool) {
	found := false
	for _, t := range m.doc.Tables("index") {
		if n, _ := t.Get("name"); n == name {
			found = true
			for i := t.LastLine; i >= t.Line; i-- {
				m.doc.deleteLine(i)
			}
			m.doc.reparse()
			break
		}
	}
	if !found {
		return nil, false
	}
	for _, d := range m.Deps {
		if idx, _, q := splitQualified(d.Name); q && idx == name {
			m.RemoveDep(d.Name)
			removedDeps = append(removedDeps, d.Name)
		}
	}
	sort.Strings(removedDeps)
	return removedDeps, true
}

// Save writes the manifest back, preserving everything the edits did not
// touch. Written through a temp file and renamed, so an interrupted write
// cannot leave a project with a half-written dependency list.
func (m *Manifest) Save() error {
	return writeFileAtomic(m.Path, []byte(ensureTrailingNewline(m.doc.String())))
}

func ensureTrailingNewline(s string) string {
	if s == "" || strings.HasSuffix(s, "\n") {
		return s
	}
	return s + "\n"
}

func writeFileAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// InitManifest creates a starter hover.toml in dir.
func InitManifest(dir, name string) (*Manifest, error) {
	path := filepath.Join(dir, ManifestName)
	if fileExists(path) {
		return nil, fmt.Errorf("%s already exists", path)
	}
	if name == "" {
		name = filepath.Base(dir)
	}
	content := fmt.Sprintf(`# Hover project manifest. Declarative only — hover never executes this file.
# See docs/package-manager-design.md.

[package]
name = %q
version = "0.1.0"

[dependencies]
`, name)
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		return nil, err
	}
	return LoadManifest(dir)
}
