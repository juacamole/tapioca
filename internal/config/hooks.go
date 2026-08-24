package config

import (
	"fmt"
	"maps"
	"net/netip"
	"net/url"
	"os"
	"os/user"
	"path/filepath"
	"slices"
	"strings"
	"sync"
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
		return nil, fmt.Errorf("ignoring %d hook(s): %s is inside a working tree opened in this session, "+
			"where a repository could have committed it — %s",
			len(c.Hooks), c.Path(), c.remedy())
	}
	return c.Hooks, nil
}

// remedy is what the user can do about a refusal. Naming DefaultPath() is the
// answer when the config was aimed at from somewhere else — a committed
// config.toml, a --settings inside the checkout. It is not the answer when the
// file already is the default one, which happens for real: XDG_CONFIG_HOME
// pointed at a versioned dotfiles directory, worked on from inside that
// directory. Telling that user to move the file to the path it is already at
// is a message that reads as a bug, and a policy nobody can act on gets turned
// off rather than followed.
func (c *Config) remedy() string {
	if resolveReal(c.Path()) == resolveReal(DefaultPath()) {
		return "the configuration directory is itself inside this tree — " +
			"keep it outside the directories you work in, or unset XDG_CONFIG_HOME/TAPIOCA_CONFIG_DIR here"
	}
	return "move it to " + DefaultPath()
}

// insideTree reports whether the file this config was loaded from lives in the
// tree being worked on, and so was possibly written by whoever produced that
// tree.
func (c *Config) insideTree(cwd string) bool { return InsideTree(c.Path(), cwd) }

// tainted remembers every file this process has already judged as inside a
// working tree, because that judgement is not about where the user is standing.
//
// "Could this file have been written by the tree that shipped it" is permanent;
// "is it under the directory being worked on right now" is not, and only the
// second question was being asked. /cd changes the working directory and
// applyReload then re-reads the same file from disk with nothing withheld, so
// walking out of the tree handed every withdrawn key back: [[hooks]], [[lsp]]
// command, editor, bash_allow, permissions.allow and permission_mode.
//
// It takes no code execution to reach. A repository's README says to run
// `tapioca --settings ./tapioca.toml`; the launch warning fires and everything
// that executes is withdrawn, exactly as designed. Then the user gives up on
// that checkout and types /cd ~/work/real-project — which is what the refusal
// message all but tells them to do — and the repository's language server
// starts, its editor is one keystroke away, and the session is in bypass.
//
// The cost is a user whose config lives in a versioned dotfiles directory they
// also work in: opening that directory once withdraws those keys for the rest
// of the session, and a restart is what brings them back. The default location
// is exempt above, so this is only the case where the file is somewhere else,
// which is the case the whole rule is about. A withdrawal that a walk across
// the filesystem undoes is not a withdrawal.
var (
	taintedMu    sync.Mutex
	taintedFiles = map[string]bool{}
)

// InsideTree is insideTree for a file that is not the config: --mcp-config
// names [[mcp]] servers, which are programs started at launch, and it was the
// one way of pointing at such a file that nothing checked. --settings aimed at
// a committed config.toml is refused; the same file handed to --mcp-config was
// honoured in full, which made the refusal a matter of which flag was typed.
func InsideTree(file, cwd string) bool {
	// A relative path is relative to the directory being worked in, which is
	// the same directory the tree is measured from. resolveReal would make it
	// absolute against the process's own, and the two being the same in every
	// call today is an invariant nothing states and nothing enforces.
	if file != "" && !filepath.IsAbs(file) {
		if abs, err := filepath.Abs(cwd); err == nil {
			file = filepath.Join(abs, file)
		}
	}
	path := resolveReal(file)
	// The config at the home-directory location is the user's own by
	// construction: an extracted archive cannot write there, because getting a
	// file read as the config is exactly what redirecting XDG_CONFIG_HOME or
	// aiming --settings elsewhere achieves. Without this, working in a
	// directory that merely contains the config — a home directory is the
	// ordinary case — read as a repository having supplied it, and answered by
	// telling the user to move the file to where it already was.
	if home := usersHome(); home != "" {
		if path == resolveReal(filepath.Join(home, ".config", "tapioca", "config.toml")) {
			return false
		}
	}
	taintedMu.Lock()
	defer taintedMu.Unlock()
	if taintedFiles[path] {
		return true
	}
	for _, root := range treeRoots(cwd) {
		if under(path, root) {
			taintedFiles[path] = true
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
// `editor` is the same key wearing different clothes: its value is split into
// an argv and executed the moment the user opens the external editor, so
// `editor = "sh ./pwn.sh"` in a committed config was arbitrary execution one
// keystroke away — and no prompt stands between a keystroke and its editor.
//
// A provider's base_url decides where the credential goes: with the API key
// coming from the environment, `[providers.anthropic] base_url` in a committed
// config sends that key, and every prompt, to a host the repository named.
// Only an address on this machine survives, since pointing at a local model
// server is the one reason a repository has to name one at all.
//
// Ask and deny rules are kept. A repository can only narrow with those, and a
// narrowing it chose is not a narrowing anyone has to be protected from.
func (c *Config) RestrictIfInsideTree(cwd string) []string {
	if !c.insideTree(cwd) {
		return nil
	}
	orig := *c
	// Filled in below, and read only when a Save happens: which providers had a
	// base_url taken off them, so putting one back does not overwrite one the
	// user has since chosen in the app.
	restoreBase := map[string]string{}
	restoreCreds := map[string]string{}
	c.unrestrict = func(save *Config) {
		save.MCP, save.LSP, save.Agents = orig.MCP, orig.LSP, orig.Agents
		save.BashAllow, save.Permissions = orig.BashAllow, orig.Permissions
		save.PermissionMode = orig.PermissionMode
		if save.Editor == "" {
			save.Editor = orig.Editor
		}
		if len(restoreBase)+len(restoreCreds) > 0 && save.Providers != nil {
			next := make(map[string]ProviderConfig, len(save.Providers))
			for k, v := range save.Providers {
				next[k] = v
			}
			for name, base := range restoreBase {
				if p, ok := next[name]; ok && p.BaseURL == "" {
					p.BaseURL = base
					next[name] = p
				}
			}
			for name, file := range restoreCreds {
				if p, ok := next[name]; ok && p.CredentialsFile == "" {
					p.CredentialsFile = file
					next[name] = p
				}
			}
			save.Providers = next
		}
	}
	var notes []string
	drop := func(cond bool, what string, clear func()) {
		if !cond {
			return
		}
		clear()
		notes = append(notes, fmt.Sprintf("ignoring %s: %s is inside a working tree opened in this session, "+
			"where a repository could have committed it — %s",
			what, c.Path(), c.remedy()))
	}
	drop(len(c.MCP) > 0, "mcp server(s)", func() { c.MCP = nil })
	drop(len(c.LSP) > 0, "language server(s)", func() { c.LSP = nil })
	drop(len(c.Agents.External) > 0, "external agent(s)", func() { c.Agents.External = nil })
	drop(len(c.BashAllow) > 0, "bash_allow", func() { c.BashAllow = nil })
	drop(len(c.Permissions.Allow) > 0, "permissions.allow", func() { c.Permissions.Allow = nil })
	drop(c.PermissionMode == "auto" || c.PermissionMode == "bypass",
		"permission_mode = "+c.PermissionMode, func() { c.PermissionMode = "manual" })
	drop(strings.TrimSpace(c.Editor) != "", "editor = "+c.Editor, func() { c.Editor = "" })
	// A fresh map, never an assignment into the existing one: the config is
	// copied by value in places that then hold the same map, and a restriction
	// that reached back into a caller's copy would be a different bug from the
	// one being fixed here.
	editProvider := func(name string, fn func(*ProviderConfig)) {
		next := make(map[string]ProviderConfig, len(c.Providers))
		for k, v := range c.Providers {
			next[k] = v
		}
		cleaned := next[name]
		fn(&cleaned)
		next[name] = cleaned
		c.Providers = next
	}
	for _, name := range slices.Sorted(maps.Keys(c.Providers)) {
		p := c.Providers[name]
		drop(offMachine(p.BaseURL), fmt.Sprintf("providers.%s.base_url = %s", name, p.BaseURL), func() {
			editProvider(name, func(cleaned *ProviderConfig) {
				restoreBase[name] = cleaned.BaseURL
				cleaned.BaseURL = ""
			})
		})
		// credentials_file is base_url one indirection further out. The vertex
		// service-account grant reads token_uri out of the JSON that file names
		// and POSTs to it, so a repository that commits both a config and a
		// key file chooses the address the exchange is made against — the
		// address base_url is refused for, reached by naming a file instead.
		// The token exchange runs on its own the first time a model answers,
		// and title_model picks the provider without the user choosing it.
		drop(strings.TrimSpace(p.CredentialsFile) != "",
			fmt.Sprintf("providers.%s.credentials_file = %s", name, p.CredentialsFile), func() {
				editProvider(name, func(cleaned *ProviderConfig) {
					restoreCreds[name] = cleaned.CredentialsFile
					cleaned.CredentialsFile = ""
				})
			})
	}
	return notes
}

// accountHome is where the account database says this user lives. It is a
// variable so a test can say what the machine cannot be asked to pretend.
var accountHome = func() string {
	u, err := user.Current()
	if err != nil {
		return ""
	}
	return u.HomeDir
}

// usersHome returns the home directory only when it is the user's own, and ""
// when that cannot be said.
//
// os.UserHomeDir is $HOME, and $HOME is an environment variable — the same kind
// of thing this file already treats as attacker-influenced when it is
// XDG_CONFIG_HOME. A tree that ships an .envrc exporting HOME=$PWD and commits
// .config/tapioca/config.toml lands exactly on the exemption above, and every
// key RestrictIfInsideTree exists to refuse was honoured again: hooks ran at
// launch. Defending one of the two variables was not defending either.
//
// The account database is the second opinion, since nothing in a checkout
// writes it. When there is no second opinion to be had — a build whose
// os/user answers from $HOME as well, or a uid with no passwd entry, which is
// ordinary inside a container — the exemption stands rather than telling every
// such user that their own config is untrusted.
func usersHome() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	if acct := accountHome(); acct != "" && resolveReal(acct) != resolveReal(home) {
		return ""
	}
	return home
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
		// An .envrc is a marker for the same reason the rest are — it says
		// "this directory is a project" — and it is the one the residual gap
		// below was reachable through on purpose. An archive that ships
		//
		//	proj/.envrc                   export XDG_CONFIG_HOME=$PWD/cfg
		//	proj/cfg/tapioca/config.toml
		//	proj/src/go.mod
		//
		// carries no marker in proj and one in src, so working in src made proj
		// "outside the tree" and the shipped config the user's own: hooks,
		// [[mcp]], [[lsp]] and permission_mode = "bypass", all honoured.
		// direnv reads an .envrc from any ancestor, which is what makes the
		// layout work at all — so the file the attack needs is exactly the file
		// that now marks the directory.
		".envrc",
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
//
// It is made absolute first. EvalSymlinks leaves a relative path relative, and
// the tree roots it is compared against are absolute, so a relative path was
// never under any of them: `tapioca --settings ./config.toml`, run in the
// checkout that committed that file, was read as a config living outside the
// tree and every key in it honoured.
func resolveReal(p string) string {
	if abs, err := filepath.Abs(p); err == nil {
		p = abs
	}
	if real, err := filepath.EvalSymlinks(p); err == nil {
		return real
	}
	return filepath.Clean(p)
}

// IsLoopbackHost reports whether a host names this machine. It has to be an
// address literal or the name reserved for one; see provider.CheckBaseURL,
// which is the other caller. One implementation, because two would drift and
// the answer decides whether a credential leaves the machine.
func IsLoopbackHost(host string) bool {
	// RFC 6761 reserves localhost for the loopback interface, in both the bare
	// and the fully qualified spelling.
	if strings.EqualFold(strings.TrimSuffix(host, "."), "localhost") {
		return true
	}
	ip, err := netip.ParseAddr(strings.Trim(host, "[]"))
	return err == nil && ip.IsLoopback()
}

// offMachine reports whether a base_url sends requests somewhere other than
// this machine. An address that cannot be parsed counts as off-machine: the
// question here is whether to trust a value a repository wrote, and "it did not
// parse" is not a reason to trust it.
func offMachine(raw string) bool {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return false // nothing configured; the built-in default applies
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return true
	}
	return !IsLoopbackHost(u.Hostname())
}
