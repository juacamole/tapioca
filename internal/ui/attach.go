package ui

import (
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"tapioca/internal/provider"
	"tapioca/internal/textenc"
)

const (
	maxImageBytes    = 5 << 20
	maxInlineFile    = 50_000
	maxMentionFiles  = 300
	mentionScanDepth = 4
)

type attachment struct {
	label     string
	mediaType string
	data      []byte
}

var imageExts = map[string]string{
	".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg",
	".gif": "image/gif", ".webp": "image/webp",
}

// clipboardImage grabs an image from the system clipboard, if there is one.
func clipboardImage() (attachment, error) {
	if _, err := exec.LookPath("wl-paste"); err == nil {
		out, err := exec.Command("wl-paste", "--list-types").Output()
		if err == nil {
			for _, t := range strings.Fields(string(out)) {
				if strings.HasPrefix(t, "image/") {
					data, err := exec.Command("wl-paste", "--type", t).Output()
					if err == nil && len(data) > 0 {
						return attachment{label: "clipboard", mediaType: t, data: data}, nil
					}
				}
			}
		}
	}
	if _, err := exec.LookPath("xclip"); err == nil {
		out, _ := exec.Command("xclip", "-selection", "clipboard", "-t", "TARGETS", "-o").Output()
		for _, t := range strings.Fields(string(out)) {
			if strings.HasPrefix(t, "image/") {
				data, err := exec.Command("xclip", "-selection", "clipboard", "-t", t, "-o").Output()
				if err == nil && len(data) > 0 {
					return attachment{label: "clipboard", mediaType: t, data: data}, nil
				}
			}
		}
	}
	return attachment{}, errors.New("no image on the clipboard (needs wl-paste or xclip)")
}

// mentionToken returns the trailing @path token being typed, or "".
func mentionToken(input string) string {
	fields := strings.Fields(input)
	if len(fields) == 0 || !strings.HasSuffix(input, fields[len(fields)-1]) {
		return ""
	}
	last := fields[len(fields)-1]
	if strings.HasPrefix(last, "@") && len(last) >= 1 {
		return strings.TrimPrefix(last[1:], `"`)
	}
	return ""
}

// mentionInsert renders a completion as an @token, quoting spaced paths.
func mentionInsert(path string) string {
	if strings.ContainsAny(path, " \t") {
		return `@"` + path + `" `
	}
	return "@" + path + " "
}

// listFiles walks cwd shallowly for mention completion candidates.
func listFiles(cwd string) []string {
	var out []string
	skip := map[string]bool{".git": true, "node_modules": true, "target": true, "result": true, "vendor": true}
	var walk func(dir, rel string, depth int)
	walk = func(dir, rel string, depth int) {
		if depth > mentionScanDepth || len(out) >= maxMentionFiles {
			return
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		for _, e := range entries {
			if len(out) >= maxMentionFiles {
				return
			}
			name := e.Name()
			if strings.HasPrefix(name, ".") || skip[name] {
				continue
			}
			p := filepath.Join(rel, name)
			if e.IsDir() {
				walk(filepath.Join(dir, name), p, depth+1)
			} else {
				out = append(out, p)
			}
		}
	}
	walk(cwd, "", 0)
	sort.Strings(out)
	return out
}

// mentionMatches filters completion candidates by the typed prefix.
func (m *App) mentionMatches() []string {
	tok := mentionToken(m.ta.Value())
	if tok == "" && !strings.HasSuffix(m.ta.Value(), "@") {
		return nil
	}
	// MCP resources complete first: there are few of them next to a worktree,
	// and they would never be seen at the bottom of fifty files.
	out := m.mcpResourceMatches(tok)
	for _, f := range listFiles(m.cwd()) {
		if tok == "" || strings.Contains(strings.ToLower(f), strings.ToLower(tok)) {
			out = append(out, f)
		}
		if len(out) >= 50 {
			break
		}
	}
	return out
}

// maxMentionBytes bounds one inlined mention. @/dev/zero read forever on the
// update goroutine before this.
const maxMentionBytes = 4 << 20

var mentionRe = regexp.MustCompile(`@"([^"]+)"|@(\S+)`)

// expandMentions resolves @path tokens (quoted for spaced paths): text files
// are inlined below the prompt, image files become attachments.
func (m *App) expandMentions(text string, atts *[]attachment) string {
	var inlined []string
	for _, match := range mentionRe.FindAllStringSubmatch(text, -1) {
		rel := match[1]
		if rel == "" {
			rel = strings.TrimRight(match[2], ".,;:")
		}
		if text, ok := m.readMCPResource(rel); ok {
			inlined = append(inlined, text)
			continue
		}
		path := rel
		if !filepath.IsAbs(path) {
			path = filepath.Join(m.cwd(), rel)
		}
		// read_file gates secrets and out-of-tree paths; a mention inlines the
		// same bytes into the same outgoing prompt, so it gets the same gate.
		// Symlinks are resolved first, since the check is on the resolved path.
		if real, err := filepath.EvalSymlinks(path); err == nil {
			path = real
		}
		if m.mgr.Exec != nil && m.mgr.Exec.MentionBlocked(path) {
			inlined = append(inlined, fmt.Sprintf("[%s: outside the working directory or a secret — not attached]", rel))
			continue
		}
		// Regular, not merely "not a directory". A size cap bounds what a read
		// costs and says nothing about a read that never starts: a FIFO reports
		// a size of zero, walks past the cap below, and os.ReadFile on it blocks
		// until something writes — on the update goroutine, so the whole TUI
		// stops. tar stores FIFOs and extracts them without being asked to, and
		// a `notes.md` in the tree is offered by mention completion like any
		// other file.
		info, err := os.Stat(path)
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		if info.Size() > maxMentionBytes {
			inlined = append(inlined, fmt.Sprintf("[file: %s — %d bytes, too large to inline]", rel, info.Size()))
			continue
		}
		if mt, isImg := imageExts[strings.ToLower(filepath.Ext(path))]; isImg {
			if info.Size() <= maxImageBytes {
				if data, err := os.ReadFile(path); err == nil {
					*atts = append(*atts, attachment{label: rel, mediaType: mt, data: data})
				}
			}
			continue
		}
		if data, err := os.ReadFile(path); err == nil {
			content, isText := textenc.Decode(data)
			if !isText {
				inlined = append(inlined, fmt.Sprintf("[file: %s — binary, not inlined]", rel))
				continue
			}
			if len(content) > maxInlineFile {
				content = textenc.Cut(content, maxInlineFile) + "\n[truncated]"
			}
			inlined = append(inlined, fmt.Sprintf("[file: %s]\n```\n%s\n```", rel, content))
		}
	}
	if len(inlined) > 0 {
		text += "\n\n" + strings.Join(inlined, "\n\n")
	}
	return text
}

// buildUserMessage assembles the outgoing message from expanded text,
// attachments, and the raw typed input (kept for /edit).
func buildUserMessage(text string, atts []attachment, typed string) provider.Message {
	msg := provider.TextMessage("user", text)
	msg.Typed = typed
	for _, a := range atts {
		msg.Blocks = append(msg.Blocks, provider.Block{
			Type:      "image",
			MediaType: a.mediaType,
			Data:      base64.StdEncoding.EncodeToString(a.data),
		})
	}
	return msg
}
