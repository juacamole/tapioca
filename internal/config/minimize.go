package config

import (
	"strings"
)

// A saved config restated every field of every struct, so a file that recorded
// one changed setting ran to eighty lines of empty strings, zeros and provider
// blocks for backends nobody had configured. That buries the lines that carry
// information and invites people to fill in fields meant to stay unset.
//
// Only what differs from the defaults is written. Load starts from Default()
// and decodes the file over it, so an omitted key comes back as exactly the
// value that was omitted — the file shrinks without changing what it means.
//
// The rule cannot be "drop empty values": autosave, auto_compact,
// model_catalog and sandbox_network all default to true, so dropping `false`
// would silently turn them back on the next time the app started.

// valueTables hold user data rather than settings with defaults — provider
// entries, MCP and LSP servers, cost overrides, colours. Their fields mean
// "unset" when empty, and there is no non-empty default to flip, so empties
// are dropped there instead. Provider entries in particular must survive:
// dropping one because it matches the built-in default would delete it, since
// Load only restores the defaults when the file names no providers at all.
var valueTables = map[string]bool{
	"providers": true,
	"mcp":       true,
	"lsp":       true,
	"costs":     true,
	"colors":    true,
	"hooks":     true,
}

// minimize removes from full every line that only restates what defaults
// already says, then drops any table left with nothing in it. Both inputs come
// from the same encoder, so identical values are identical text.
//
// Annotated lines stay whatever their value. Comments are attached to the key
// above or beside them, so removing the key would throw the note away — and a
// line someone wrote a note about is the last line to delete for being
// unremarkable.
func minimize(full, defaults string, comments commentMap) string {
	def := indexValues(defaults)

	type line struct {
		text  string
		table string // "" for the top level
		kept  bool
	}
	var lines []line
	table := ""
	for _, raw := range strings.Split(full, "\n") {
		t := strings.TrimSpace(raw)
		switch {
		case strings.HasPrefix(t, "[") && strings.HasSuffix(t, "]"):
			table = tableName(t)
			lines = append(lines, line{text: raw, table: table})
			continue
		case t == "" || strings.HasPrefix(t, "#"):
			lines = append(lines, line{text: raw, table: table, kept: true})
			continue
		}
		key, value, ok := splitKeyValue(t)
		if !ok {
			lines = append(lines, line{text: raw, table: table, kept: true})
			continue
		}
		if !annotated(comments, table, key) && drop(table, key, value, def) {
			continue
		}
		lines = append(lines, line{text: raw, table: table, kept: true})
	}

	// A table whose keys all went is noise on its own — [keys] and
	// [permissions] with nothing under them are the common case.
	used := map[string]bool{}
	// A table header carrying its own comment stays even when empty, for the
	// same reason an annotated key does.
	for k, c := range comments {
		name, isTable := strings.CutSuffix(k, "\x00")
		if !isTable || name == "" || (len(c.before) == 0 && c.trailing == "") {
			continue
		}
		for _, anc := range ancestors(name) {
			used[anc] = true
		}
	}
	for _, l := range lines {
		if l.kept && l.table != "" && strings.TrimSpace(l.text) != "" &&
			!strings.HasPrefix(strings.TrimSpace(l.text), "#") {
			// Mark this table and every table above it: a parent stays when a
			// child has content, or [providers] would vanish from over
			// [providers.ollama].
			for _, anc := range ancestors(l.table) {
				used[anc] = true
			}
		}
	}

	var out []string
	for _, l := range lines {
		if l.table != "" && !used[l.table] {
			continue
		}
		out = append(out, l.text)
	}
	return collapseBlankRuns(out)
}

// annotated reports whether a key carries a comment of its own.
func annotated(comments commentMap, table, key string) bool {
	c, ok := comments[table+"\x00"+key]
	return ok && (len(c.before) > 0 || c.trailing != "")
}

// drop reports whether a key can be left out.
func drop(table, key, value string, def map[string]string) bool {
	root := table
	if i := strings.IndexByte(root, '.'); i >= 0 {
		root = root[:i]
	}
	if valueTables[root] {
		return isEmptyValue(value)
	}
	d, ok := def[table+"\x00"+key]
	return ok && d == value
}

// isEmptyValue reports whether a value carries nothing. Booleans are never
// treated as empty: false is a real answer wherever it appears.
func isEmptyValue(v string) bool {
	switch strings.TrimSpace(v) {
	case `""`, "''", "0", "0.0", "[]", "{}":
		return true
	}
	return false
}

// indexValues maps "table\x00key" to the value text as the encoder wrote it.
func indexValues(text string) map[string]string {
	out := map[string]string{}
	table := ""
	for _, raw := range strings.Split(text, "\n") {
		t := strings.TrimSpace(raw)
		if strings.HasPrefix(t, "[") && strings.HasSuffix(t, "]") {
			table = tableName(t)
			continue
		}
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		if key, value, ok := splitKeyValue(t); ok {
			out[table+"\x00"+key] = value
		}
	}
	return out
}

// tableName strips the brackets from a table header, including the double
// brackets of an array of tables.
func tableName(t string) string {
	t = strings.TrimSuffix(strings.TrimPrefix(t, "[["), "]]")
	return strings.TrimSuffix(strings.TrimPrefix(t, "["), "]")
}

// ancestors lists a table path and every parent of it.
func ancestors(table string) []string {
	var out []string
	parts := strings.Split(table, ".")
	for i := range parts {
		out = append(out, strings.Join(parts[:i+1], "."))
	}
	return out
}

// splitKeyValue splits "key = value" without being fooled by an = inside the
// value.
func splitKeyValue(t string) (key, value string, ok bool) {
	i := strings.IndexByte(t, '=')
	if i <= 0 {
		return "", "", false
	}
	key = strings.TrimSpace(t[:i])
	if key == "" || strings.ContainsAny(key, " \t") && !strings.HasPrefix(key, `"`) {
		return "", "", false
	}
	return key, strings.TrimSpace(t[i+1:]), true
}

// collapseBlankRuns keeps the output from filling with the gaps left by
// removed lines.
func collapseBlankRuns(lines []string) string {
	var out []string
	blank := 0
	for _, l := range lines {
		if strings.TrimSpace(l) == "" {
			blank++
			if blank > 1 {
				continue
			}
		} else {
			blank = 0
		}
		out = append(out, l)
	}
	for len(out) > 0 && strings.TrimSpace(out[len(out)-1]) == "" {
		out = out[:len(out)-1]
	}
	return strings.Join(out, "\n") + "\n"
}
