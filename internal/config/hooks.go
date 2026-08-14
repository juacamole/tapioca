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
	root := workRoot(cwd)
	if under(resolveReal(c.Path()), root) {
		return nil, fmt.Errorf("ignoring %d hook(s): %s is inside the working tree, "+
			"where a repository could have committed it — move them to %s",
			len(c.Hooks), c.Path(), DefaultPath())
	}
	return c.Hooks, nil
}

// workRoot is the repository enclosing cwd, or cwd when there is none. The
// walk matters: a clone with a config.toml at its root is equally the
// repository's when the user is working three directories down.
func workRoot(cwd string) string {
	abs, err := filepath.Abs(cwd)
	if err != nil {
		abs = cwd
	}
	abs = resolveReal(abs)
	for dir := abs; ; {
		for _, marker := range []string{".git", ".hg", ".jj"} {
			if _, err := os.Stat(filepath.Join(dir, marker)); err == nil {
				return dir
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return abs
		}
		dir = parent
	}
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
