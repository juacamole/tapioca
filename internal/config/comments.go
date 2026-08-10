package config

import (
	"strings"
)

// The app writes this file on every toggle — a permission-mode change, a model
// switch, a panel move. Re-encoding the struct is safe but drops every comment,
// so the documented starter config lost its explanations on first use, along
// with anything the user had written. Values are still re-encoded (editing TOML
// structurally in place is a good way to corrupt it); the comments are carried
// across and re-attached to the keys they belonged to.

type keyComments struct {
	before   []string // whole-line comments immediately above the key
	trailing string   // "# ..." after the value on the same line
}

// commentMap indexes comments by "table\x00key", plus "" for the file header.
type commentMap map[string]keyComments

const (
	headerKey = "\x00header"
	tailKey   = "\x00tail"
)

// splitComment finds a trailing comment, ignoring '#' inside quotes — colors
// like "#A78BFA" are values, not comments.
func splitComment(line string) (code, comment string) {
	inSingle, inDouble := false, false
	for i := 0; i < len(line); i++ {
		switch c := line[i]; c {
		case '\\':
			if inDouble {
				i++
			}
		case '\'':
			if !inDouble {
				inSingle = !inSingle
			}
		case '"':
			if !inSingle {
				inDouble = !inDouble
			}
		case '#':
			if !inSingle && !inDouble {
				return strings.TrimRight(line[:i], " \t"), line[i:]
			}
		}
	}
	return line, ""
}

// keyOf returns the key a line assigns to, or "".
func keyOf(code string) string {
	trimmed := strings.TrimSpace(code)
	if trimmed == "" || strings.HasPrefix(trimmed, "[") {
		return ""
	}
	name, _, ok := strings.Cut(trimmed, "=")
	if !ok {
		return ""
	}
	return strings.Trim(strings.TrimSpace(name), `"'`)
}

// tableOf returns the table a header line opens, or "" if it is not a header.
func tableOf(code string) (string, bool) {
	trimmed := strings.TrimSpace(code)
	if !strings.HasPrefix(trimmed, "[") || !strings.HasSuffix(trimmed, "]") {
		return "", false
	}
	return strings.Trim(trimmed, "[]"), true
}

// harvestComments records the comments attached to each key of a TOML file.
func harvestComments(text string) commentMap {
	out := commentMap{}
	table := ""
	var pending []string
	var header []string
	seenKey := false

	for _, line := range strings.Split(text, "\n") {
		code, comment := splitComment(line)
		if strings.TrimSpace(code) == "" {
			switch {
			case comment == "":
				// A blank line inside a block is part of it: whole commented-out
				// example sections are separated by them, and dropping the gap
				// dropped the sections.
				if len(pending) > 0 {
					pending = append(pending, "")
				}
			case !seenKey && table == "":
				header = append(header, comment)
			default:
				pending = append(pending, comment)
			}
			continue
		}
		if t, ok := tableOf(code); ok {
			if len(pending) > 0 {
				out[t+"\x00"] = keyComments{before: trimTrailingBlanks(pending)}
			}
			table, pending = t, nil
			continue
		}
		if key := keyOf(code); key != "" {
			seenKey = true
			out[table+"\x00"+key] = keyComments{before: trimTrailingBlanks(pending), trailing: comment}
			pending = nil
		}
	}
	if len(header) > 0 {
		out[headerKey] = keyComments{before: header}
	}
	// Comments with nothing after them still belong to the file.
	if len(pending) > 0 {
		out[tailKey] = keyComments{before: trimTrailingBlanks(pending)}
	}
	return out
}

func trimTrailingBlanks(lines []string) []string {
	for len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// applyComments re-attaches harvested comments to freshly encoded TOML.
func applyComments(encoded string, cm commentMap) string {
	if len(cm) == 0 {
		return encoded
	}
	var out []string
	if h, ok := cm[headerKey]; ok {
		out = append(out, h.before...)
		out = append(out, "")
	}
	table := ""
	for _, line := range strings.Split(encoded, "\n") {
		if t, ok := tableOf(line); ok {
			table = t
			if c, ok := cm[t+"\x00"]; ok {
				out = append(out, c.before...)
			}
			out = append(out, line)
			continue
		}
		key := keyOf(line)
		if key == "" {
			out = append(out, line)
			continue
		}
		c, ok := cm[table+"\x00"+key]
		if !ok {
			out = append(out, line)
			continue
		}
		out = append(out, c.before...)
		if c.trailing != "" {
			line += "  " + c.trailing
		}
		out = append(out, line)
	}
	if c, ok := cm[tailKey]; ok {
		out = append(out, c.before...)
	}
	return strings.Join(out, "\n")
}
