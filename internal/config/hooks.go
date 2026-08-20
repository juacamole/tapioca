package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Everything else in this file is a preference; a hook is a command that runs
// with the user's own privileges. So it answers to the rule the rest of the
// project already follows about where configuration may come from: a
// repository supplies prompt text (AGENTS.md, .tapioca/commands) and nothing
// that executes — MCP servers, language servers, bash_allow and [permissions]
// all come from the user's own config file, which is never discovered inside
// the working tree.
//
// A repository can still reach that file by choosing where it is looked up: an
// .envrc that points XDG_CONFIG_HOME into the checkout, or a committed
// config.toml that --settings is aimed at. Both end with a file that the clone
// wrote, so hooks are honoured only when the config lives outside the tree
// being worked on. The refusal is reported rather than silent, because a
// policy that quietly stopped applying is worse than one that never existed.

// TrustedHooks returns the hooks that may run for work in cwd, and an error
// naming why when they may not.
func (c *Config) TrustedHooks(cwd string) ([]HookConfig, error) {
	if len(c.Hooks) == 0 {
		return nil, nil
	}
	if c.insideTree(cwd) {
		return nil, fmt.Errorf("ignoring %d hook(s): %s is inside the working tree, "+
			"where a repository could have committed it — move them to %s",
			len(c.Hooks), c.Path(), DefaultPath())
	}
	return c.Hooks, nil
}

// insideTree reports whether the file this config was loaded from lives in the
// tree being worked on, and so was possibly written by whoever produced that
// tree.
func (c *Config) insideTree(cwd string) bool {
	path := resolveReal(c.Path())
	// The config at the home-directory location is the user's own by
	// construction: an extracted archive cannot write there, because getting a
	// file read as the config is exactly what redirecting XDG_CONFIG_HOME or
	// aiming --settings elsewhere achieves. Without this, working in a
	// directory that merely contains the config — a home directory is the
	// ordinary case — read as a repository having supplied it, and answered by
	// telling the user to move the file to where it already was.
	if home, err := os.UserHomeDir(); err == nil {
		if path == resolveReal(filepath.Join(home, ".config", "tapioca", "config.toml")) {
			return false
		}
	}
	for _, root := range treeRoots(cwd) {
		if under(path, root) {
			return true
		}
	}
	return false
}

// RestrictIfInsideTree drops everything in an in-tree config that decides what
// executes, or what executes without asking, and returns one note per key
// dropped. Nothing happens for a config outside the tree, which is the normal
// case.
//
// A hook was never the only key in that file with the power the paragraph at
// the top of this file describes. `command` under [[mcp]], [[lsp]] and
// [[agents.external]] is a program started at launch — a committed config.toml
// with `[[lsp]] command = "sh"` ran before the first keystroke, while its
// [[hooks]] were being refused two lines away. bash_allow and permissions.allow
// hand out standing approvals, and permission_mode = "bypass" removes the gate
// altogether. Gating one of the seven was not a policy.
//
// Ask and deny rules are kept. A repository can only narrow with those, and a
// narrowing it chose is not a narrowing anyone has to be protected from.
func (c *Config) RestrictIfInsideTree(cwd string) []string {
	if !c.insideTree(cwd) {
		return nil
	}
	orig := *c
	c.unrestrict = func(save *Config) {
		save.MCP, save.LSP, save.Agents = orig.MCP, orig.LSP, orig.Agents
		save.BashAllow, save.Permissions = orig.BashAllow, orig.Permissions
		save.PermissionMode = orig.PermissionMode
	}
	var notes []string
	drop := func(cond bool, what string, clear func()) {
		if !cond {
			return
		}
		clear()
		notes = append(notes, fmt.Sprintf("ignoring %s: %s is inside the working tree, "+
			"where a repository could have committed it — move it to %s",
			what, c.Path(), DefaultPath()))
	}
	drop(len(c.MCP) > 0, "mcp server(s)", func() { c.MCP = nil })
	drop(len(c.LSP) > 0, "language server(s)", func() { c.LSP = nil })
	drop(len(c.Agents.External) > 0, "external agent(s)", func() { c.Agents.External = nil })
	drop(len(c.BashAllow) > 0, "bash_allow", func() { c.BashAllow = nil })
	drop(len(c.Permissions.Allow) > 0, "permissions.allow", func() { c.Permissions.Allow = nil })
	drop(c.PermissionMode == "auto" || c.PermissionMode == "bypass",
		"permission_mode = "+c.PermissionMode, func() { c.PermissionMode = "manual" })
	return notes
}

// treeRoots is the working tree cwd belongs to, as far as it can be known.
//
// A version-controlled checkout answers exactly, and that is the common case.
// It is not the only one: a tarball, a zip, a vendored directory or a clone
// with .git removed has no VCS marker, and the walk then returned cwd itself —
// so a config one directory up counted as outside the tree and its hooks ran.
// Working from a subdirectory was the whole exploit.
//
// Build files are the boundary for those, since an archive of a real project
// carries one. A directory with no marker of any kind falls back to cwd, which
// is the residual gap: a bare pile of files, with a config in a parent, worked
// on from a subdirectory. Walking further would have to stop somewhere
// arbitrary, and stopping at the filesystem root would refuse a system-wide
// config — a rule that cries wolf gets turned off.
func treeRoots(cwd string) []string {
	abs, err := filepath.Abs(cwd)
	if err != nil {
		abs = cwd
	}
	abs = resolveReal(abs)

	markers := []string{
		".git", ".hg", ".jj", // a checkout
		"go.mod", "package.json", "Cargo.toml", // an archive of one
		"pyproject.toml", "pom.xml", "build.gradle", "Gemfile", "composer.json",
	}
	// Every ancestor that carries a marker counts, not the nearest one. The
	// archive chooses where its markers are, so stopping at the first put the
	// boundary where the archive wanted it: a go.mod dropped in the
	// subdirectory being worked in makes that subdirectory the tree, and a
	// config committed one level above it is then "outside".
	var roots []string
	for dir := abs; ; {
		for _, marker := range markers {
			if _, err := os.Stat(filepath.Join(dir, marker)); err == nil {
				roots = append(roots, dir)
				break
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	if len(roots) == 0 {
		// Nothing says where the tree ends, so the directory being worked in is
		// treated as all of it: an archive that ships no marker at all must not
		// thereby escape the check.
		return []string{abs}
	}
	return roots
}

// under reports whether a resolved path is dir or inside it.
func under(path, dir string) bool {
	return path == dir || strings.HasPrefix(path, dir+string(filepath.Separator))
}

// resolveReal follows symlinks, so a link inside the checkout cannot make a
// file it controls look like it lives elsewhere. A path that does not resolve
// is cleaned lexically, which is the conservative answer here: it stays where
// it was written rather than escaping the comparison.
func resolveReal(p string) string {
	if real, err := filepath.EvalSymlinks(p); err == nil {
		return real
	}
	return filepath.Clean(p)
}
