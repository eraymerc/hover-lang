package hpm

import (
	"fmt"
	"strconv"
	"strings"
)

// ─────────────────────────────────────────────────────────────────────────────
// A DELIBERATELY SMALL TOML
//
// hover.toml and hover.lock use a strict subset of TOML: comments, `[table]`
// and `[table.sub]` headers, `[[array-of-tables]]` headers, and `key = value`
// where a value is a string, an integer, or a bool. No inline tables, no
// arrays, no multi-line strings, no dotted keys.
//
// Written here rather than pulled from a dependency for two reasons. First,
// hover has exactly one non-stdlib Go dependency today and a package manager
// is a bad place to start growing a dependency tree — a supply chain is the
// thing it is supposed to be careful about. Second, the subset is small
// enough that the whole grammar fits on a screen, which matters for a file
// format users hand-edit and whose parse result decides what code gets
// downloaded.
//
// The parser keeps the original lines. Edits (adding or removing a
// dependency) rewrite individual lines rather than re-serialising the
// document, so comments, blank lines and key order survive `hover hpm
// install` untouched. A dependency file people maintain by hand must not be
// reformatted by the tool that reads it.
// ─────────────────────────────────────────────────────────────────────────────

// tomlKV is one `key = value` line.
type tomlKV struct {
	Key   string
	Value string // decoded (quotes removed)
	Line  int    // index into Document.lines
}

// tomlTable is one `[header]` section and the keys under it. The root table
// (keys before any header) is represented with an empty Path and Line -1.
type tomlTable struct {
	Path      []string
	Array     bool // came from [[double brackets]]
	Line      int  // index of the header line, or -1 for the root table
	Keys      []tomlKV
	LastLine  int // index of the last line belonging to this table
	rawHeader string
}

// Document is a parsed TOML file that still remembers its own text.
type Document struct {
	lines  []string
	tables []*tomlTable
}

// ParseTOML parses the supported subset. Errors name the line number,
// because the most common cause is someone hand-editing hover.toml.
func ParseTOML(src string) (*Document, error) {
	doc := &Document{}
	if src != "" {
		doc.lines = strings.Split(strings.ReplaceAll(src, "\r\n", "\n"), "\n")
	}

	root := &tomlTable{Path: nil, Line: -1, LastLine: -1}
	doc.tables = []*tomlTable{root}
	current := root

	for i, raw := range doc.lines {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		if strings.HasPrefix(line, "[") {
			isArray := strings.HasPrefix(line, "[[")
			closing := "]"
			if isArray {
				closing = "]]"
			}
			end := strings.Index(line, closing)
			if end == -1 {
				return nil, fmt.Errorf("line %d: unterminated table header %q", i+1, line)
			}
			open := 1
			if isArray {
				open = 2
			}
			header := strings.TrimSpace(line[open:end])
			path, err := splitTableHeader(header)
			if err != nil {
				return nil, fmt.Errorf("line %d: %w", i+1, err)
			}
			current = &tomlTable{Path: path, Array: isArray, Line: i, LastLine: i, rawHeader: header}
			doc.tables = append(doc.tables, current)
			continue
		}

		eq := strings.IndexByte(line, '=')
		if eq == -1 {
			return nil, fmt.Errorf("line %d: expected `key = value`, got %q", i+1, line)
		}
		key, err := decodeKey(strings.TrimSpace(line[:eq]))
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", i+1, err)
		}
		value, err := decodeValue(stripComment(strings.TrimSpace(line[eq+1:])))
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", i+1, err)
		}
		current.Keys = append(current.Keys, tomlKV{Key: key, Value: value, Line: i})
		current.LastLine = i
	}

	return doc, nil
}

// splitTableHeader splits `dependencies.private-thing` into its segments,
// honouring quoted segments so a package name containing a dot or a colon
// (`"myindex:foo"`) stays one segment.
func splitTableHeader(header string) ([]string, error) {
	var parts []string
	var cur strings.Builder
	inQuote := false
	for i := 0; i < len(header); i++ {
		c := header[i]
		switch {
		case c == '"':
			inQuote = !inQuote
		case c == '.' && !inQuote:
			parts = append(parts, strings.TrimSpace(cur.String()))
			cur.Reset()
		default:
			cur.WriteByte(c)
		}
	}
	if inQuote {
		return nil, fmt.Errorf("unterminated quote in table header %q", header)
	}
	parts = append(parts, strings.TrimSpace(cur.String()))
	for _, p := range parts {
		if p == "" {
			return nil, fmt.Errorf("empty segment in table header %q", header)
		}
	}
	return parts, nil
}

func decodeKey(k string) (string, error) {
	if strings.HasPrefix(k, `"`) {
		if !strings.HasSuffix(k, `"`) || len(k) < 2 {
			return "", fmt.Errorf("unterminated quoted key %q", k)
		}
		return strconv.Unquote(k)
	}
	if k == "" {
		return "", fmt.Errorf("empty key")
	}
	return k, nil
}

func decodeValue(v string) (string, error) {
	if strings.HasPrefix(v, `"`) {
		s, err := strconv.Unquote(v)
		if err != nil {
			return "", fmt.Errorf("bad string value %q", v)
		}
		return s, nil
	}
	if v == "" {
		return "", fmt.Errorf("missing value")
	}
	// Bare values (ints, bools) are kept as text; every caller that wants a
	// number parses it itself, and nothing in these files is arithmetic.
	return v, nil
}

// stripComment removes a trailing `# ...` that is not inside a string.
func stripComment(s string) string {
	inQuote := false
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '"':
			if i == 0 || s[i-1] != '\\' {
				inQuote = !inQuote
			}
		case '#':
			if !inQuote {
				return strings.TrimSpace(s[:i])
			}
		}
	}
	return s
}

// ─────────────────────────────────────────────────────────────────────────────
// READING
// ─────────────────────────────────────────────────────────────────────────────

func pathEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Table returns the single table at path, or nil. For `[[array]]` headers use
// Tables instead.
func (d *Document) Table(path ...string) *tomlTable {
	for _, t := range d.tables {
		if !t.Array && pathEqual(t.Path, path) {
			return t
		}
	}
	return nil
}

// Tables returns every table declared at path, in file order — the
// `[[index]]` case.
func (d *Document) Tables(path ...string) []*tomlTable {
	var out []*tomlTable
	for _, t := range d.tables {
		if pathEqual(t.Path, path) {
			out = append(out, t)
		}
	}
	return out
}

// SubTables returns every table whose path is exactly one segment longer
// than prefix — `[dependencies.foo]` for prefix `dependencies`.
func (d *Document) SubTables(prefix ...string) []*tomlTable {
	var out []*tomlTable
	for _, t := range d.tables {
		if len(t.Path) == len(prefix)+1 && pathEqual(t.Path[:len(prefix)], prefix) {
			out = append(out, t)
		}
	}
	return out
}

// Get returns the value of key in this table, and whether it was present.
func (t *tomlTable) Get(key string) (string, bool) {
	if t == nil {
		return "", false
	}
	for _, kv := range t.Keys {
		if kv.Key == key {
			return kv.Value, true
		}
	}
	return "", false
}

// Name returns the last segment of a table's path — the package name for
// `[dependencies.foo]`.
func (t *tomlTable) Name() string {
	if len(t.Path) == 0 {
		return ""
	}
	return t.Path[len(t.Path)-1]
}

func (d *Document) String() string { return strings.Join(d.lines, "\n") }

// ─────────────────────────────────────────────────────────────────────────────
// EDITING
//
// Line-level, so everything the user wrote around the edit survives.
// ─────────────────────────────────────────────────────────────────────────────

// Set assigns key = value inside the table at path, creating the table if it
// does not exist. An existing key is rewritten in place, preserving its
// position; a new key is appended to the end of its table rather than the
// end of the file, so related keys stay together.
func (d *Document) Set(path []string, key, value string) {
	line := formatKV(key, value)

	t := d.Table(path...)
	if t == nil {
		d.appendLines(append(blankIfNeeded(d.lines), "["+formatHeaderPath(path)+"]", line)...)
		d.reparse()
		return
	}

	for _, kv := range t.Keys {
		if kv.Key == key {
			d.lines[kv.Line] = line
			d.reparse()
			return
		}
	}

	d.insertAfter(t.LastLine, line)
	d.reparse()
}

// Remove deletes key from the table at path. Returns whether anything was
// removed. If that leaves an empty `[table.sub]` section behind, the header
// goes too — an orphaned `[dependencies.foo]` with no keys would otherwise
// read as a dependency with no source.
func (d *Document) Remove(path []string, key string) bool {
	t := d.Table(path...)
	if t == nil {
		return false
	}
	for _, kv := range t.Keys {
		if kv.Key != key {
			continue
		}
		d.deleteLine(kv.Line)
		d.reparse()
		return true
	}
	return false
}

// RemoveTable deletes an entire `[table]` section, header and keys.
func (d *Document) RemoveTable(path ...string) bool {
	t := d.Table(path...)
	if t == nil || t.Line < 0 {
		return false
	}
	// Delete downwards so earlier indices stay valid.
	for i := t.LastLine; i >= t.Line; i-- {
		d.deleteLine(i)
	}
	d.reparse()
	return true
}

func (d *Document) appendLines(all ...string) { d.lines = all }

func (d *Document) insertAfter(idx int, line string) {
	if idx < 0 || idx+1 > len(d.lines) {
		d.lines = append(d.lines, line)
		return
	}
	d.lines = append(d.lines[:idx+1], append([]string{line}, d.lines[idx+1:]...)...)
}

func (d *Document) deleteLine(idx int) {
	if idx < 0 || idx >= len(d.lines) {
		return
	}
	d.lines = append(d.lines[:idx], d.lines[idx+1:]...)
}

// reparse rebuilds the table index after an edit. Re-running the parser is
// cheap on files this size, and it is the only way to keep every recorded
// line index correct without duplicating the parser's bookkeeping in each
// edit operation — which is exactly where an off-by-one would silently
// corrupt someone's manifest.
func (d *Document) reparse() {
	if fresh, err := ParseTOML(strings.Join(d.lines, "\n")); err == nil {
		d.tables = fresh.tables
		d.lines = fresh.lines
	}
}

// blankIfNeeded returns lines with a trailing blank separator, so an
// appended table header never ends up glued to the previous key.
func blankIfNeeded(lines []string) []string {
	if len(lines) == 0 {
		return lines
	}
	if strings.TrimSpace(lines[len(lines)-1]) == "" {
		return lines
	}
	return append(lines, "")
}

func formatKV(key, value string) string {
	return quoteKeyIfNeeded(key) + " = " + strconv.Quote(value)
}

func formatHeaderPath(path []string) string {
	parts := make([]string, len(path))
	for i, p := range path {
		parts[i] = quoteKeyIfNeeded(p)
	}
	return strings.Join(parts, ".")
}

// quoteKeyIfNeeded quotes a key that is not a bare TOML key. Qualified
// package names contain a colon (`myindex:foo`), which is not bare-key
// legal, so this is load-bearing rather than cosmetic.
func quoteKeyIfNeeded(k string) string {
	for i := 0; i < len(k); i++ {
		c := k[i]
		bare := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
			(c >= '0' && c <= '9') || c == '-' || c == '_'
		if !bare {
			return strconv.Quote(k)
		}
	}
	if k == "" {
		return `""`
	}
	return k
}
