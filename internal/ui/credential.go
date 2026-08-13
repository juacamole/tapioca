package ui

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"

	"tapioca/internal/config"
	"tapioca/internal/provider"
)

// Entering a credential is the one place where a mistake is expensive and
// invisible: a wrong key is only discovered on the next prompt, by which point
// the screen has moved on. So the key is tested against the provider before it
// is written anywhere, and the result is reported in the provider's own words.
//
// The value is secret in the sense that matters here — it must not reach the
// transcript, a session file, a log, or a flash. It is held in a text input
// with echo off, written straight to the config, and dropped.

// credTestTimeout bounds the verification call. Long enough for a cold TLS
// handshake to a distant region, short enough that a wrong endpoint does not
// look like a hang.
const credTestTimeout = 15 * time.Second

// credState is where the entry flow has got to.
type credState int

const (
	credEntering credState = iota
	credTesting
	credFailed
)

type credForm struct {
	kind   provider.Kind
	fields []provider.Field  // in the order they are asked for
	values map[string]string // what has been entered so far
	at     int               // which field is being entered
	input  textinput.Model
	state  credState
	err    string
}

// field is the one currently being entered.
func (c *credForm) field() provider.Field {
	if c.at < len(c.fields) {
		return c.fields[c.at]
	}
	return provider.Field{}
}

// relevant reports whether a field applies given what has been entered. A
// header name is meaningless unless the auth style is header, and asking for
// it anyway trains people to press enter through questions that do not matter.
func (c *credForm) relevant(f provider.Field) bool {
	switch f.Key {
	case "auth_header":
		return c.values["auth_style"] == "header"
	case "auth_query":
		return c.values["auth_style"] == "query"
	case "api_key":
		return c.values["auth_style"] != "none"
	}
	return true
}

// advance moves to the next field that applies, and reports whether any
// remain.
func (c *credForm) advance() bool {
	for c.at++; c.at < len(c.fields); c.at++ {
		if c.relevant(c.fields[c.at]) {
			return true
		}
	}
	return false
}

// credTestedMsg carries the verification result back to the update loop. It
// deliberately does not carry the value: nothing downstream needs it, and a
// secret in a message is a secret in a crash dump.
type credTestedMsg struct {
	kind   string
	models int
	err    error
}

// openCredentialEntry starts the flow for a provider whose setup is a single
// secret. Providers needing several fields are #137.
func (m *App) openCredentialEntry(k provider.Kind) {
	m.cred = &credForm{kind: k, fields: k.Fields, values: map[string]string{}}
	// Start on the first field that applies. A provider whose first field is
	// conditional would otherwise open on a question about a choice not yet
	// made.
	if len(k.Fields) > 0 && !m.cred.relevant(k.Fields[0]) {
		m.cred.advance()
	}
	m.focusField()
	m.overlay = overlayCredential
}

// focusField prepares the input for the current field. Echo is decided per
// field rather than per form, so a secret is masked and an address is not.
func (m *App) focusField() {
	c := m.cred
	f := c.field()
	in := textinput.New()
	in.Placeholder = f.Label
	in.Prompt = "> "
	in.CharLimit = 512
	if f.Secret {
		in.EchoMode = textinput.EchoPassword
		in.EchoCharacter = '•'
	}
	if f.Default != "" {
		in.SetValue(f.Default)
	}
	in.Focus()
	in.SetWidth(min(60, max(20, m.w-24)))
	c.input = in
	c.state = credEntering
	c.err = ""
}

// validateField rejects a value the provider would reject later, at the point
// it was typed.
func validateField(f provider.Field, value string) error {
	if value == "" {
		return nil // optional, and already checked when required
	}
	if len(f.Choices) > 0 {
		for _, c := range f.Choices {
			if value == c {
				return nil
			}
		}
		return fmt.Errorf("%s must be one of %s", f.Label, strings.Join(f.Choices, ", "))
	}
	if f.Key == "base_url" {
		return provider.CheckBaseURL(value)
	}
	return nil
}

// testProvider builds the provider from everything entered and makes a real
// call. A configuration that does not work must fail here rather than on the
// user's next prompt, by which point the screen has moved on.
//
// It starts from the existing entry for the same reason saveCredential does:
// what gets tested has to be what gets saved. Some fields the form never asks
// about — an address pointing at a proxy, an api_version, extra headers — and
// dropping them here would report success about a provider that is not the one
// written to disk.
func testProvider(k provider.Kind, existing config.ProviderConfig, values map[string]string) tea.Cmd {
	pc := existing
	pc.Type = k.Type
	for key, v := range values {
		applyField(&pc, key, v)
	}
	return func() tea.Msg {
		p, err := provider.New(k.Type, pc)
		if err != nil {
			return credTestedMsg{kind: k.Type, err: err}
		}
		ctx, cancel := context.WithTimeout(context.Background(), credTestTimeout)
		defer cancel()
		models, err := p.ListModels(ctx)
		if err != nil {
			return credTestedMsg{kind: k.Type, err: err}
		}
		return credTestedMsg{kind: k.Type, models: len(models)}
	}
}

// applyField writes one catalog field into a provider config.
func applyField(pc *config.ProviderConfig, key, value string) {
	switch key {
	case "api_key":
		pc.APIKey = value
	case "base_url":
		pc.BaseURL = value
	case "region":
		pc.Region = value
	case "profile":
		pc.Profile = value
	case "project":
		pc.Project = value
	case "credentials_file":
		pc.CredentialsFile = value
	case "api_version":
		pc.APIVersion = value
	case "auth_style":
		pc.AuthStyle = value
	case "auth_header":
		pc.AuthHeader = value
	case "auth_query":
		pc.AuthQuery = value
	}
}

// saveCredential writes a verified credential. It prefers an environment
// variable reference over the literal: the config file is read by every tool
// the user points at their dotfiles, and a key that is not there cannot leak
// from there. The literal is used only when the variable is not actually set,
// since a reference to an unset variable is a provider that silently does not
// work.
func (m *App) saveCredential(k provider.Kind, values map[string]string) {
	pc := m.cfg.Providers[k.Type]
	if pc.Type == "" {
		pc.Type = k.Type
	}
	for _, f := range k.Fields {
		value, entered := values[f.Key]
		if !entered {
			continue
		}
		if f.Secret {
			if env := envHolding(value); env != "" {
				pc.APIKey = ""
				pc.APIKeyEnv = env
			} else {
				pc.APIKey = value
				pc.APIKeyEnv = ""
			}
			continue
		}
		applyField(&pc, f.Key, value)
	}
	if m.cfg.Providers == nil {
		m.cfg.Providers = map[string]config.ProviderConfig{}
	}
	m.cfg.Providers[k.Type] = pc
	m.saveCfg()
}

// envHolding returns the name of an environment variable already holding this
// exact value, so a user who has exported their key gets a config that
// references it rather than a second copy on disk.
func envHolding(value string) string {
	if value == "" {
		return ""
	}
	for _, kv := range os.Environ() {
		name, v, ok := strings.Cut(kv, "=")
		if ok && v == value && looksLikeKeyVar(name) {
			return name
		}
	}
	return ""
}

func looksLikeKeyVar(name string) bool {
	up := strings.ToUpper(name)
	return strings.Contains(up, "API_KEY") || strings.Contains(up, "TOKEN")
}

// handleCredentialKey drives the entry overlay.
func (m *App) handleCredentialKey(msg tea.KeyPressMsg) (tea.Cmd, bool) {
	if m.cred == nil {
		return nil, false
	}
	switch msg.String() {
	case "esc":
		m.cred = nil
		m.overlay = overlayNone
		return nil, true
	case "enter":
		c := m.cred
		if c.state == credTesting {
			return nil, true
		}
		f := c.field()
		value := strings.TrimSpace(c.input.Value())
		if value == "" && !f.Optional {
			c.err = f.Label + " cannot be empty"
			c.state = credFailed
			return nil, true
		}
		// Each value is checked as it is entered, so a bad address is reported
		// on the address rather than as a connection failure three fields
		// later, when it is no longer obvious which answer was wrong.
		if err := validateField(f, value); err != nil {
			c.err = err.Error()
			c.state = credFailed
			return nil, true
		}
		c.values[f.Key] = value

		if c.advance() {
			m.focusField()
			return nil, true
		}
		// Every field answered: now the whole thing is tried for real.
		c.state = credTesting
		c.err = ""
		return testProvider(c.kind, m.cfg.Providers[c.kind.Type], c.values), true
	}
	if m.cred.state == credTesting {
		return nil, true
	}
	var cmd tea.Cmd
	m.cred.input, cmd = m.cred.input.Update(msg)
	return cmd, true
}

// handleCredentialTested closes the flow on success, or reports the provider's
// own error and leaves the value in place to be corrected.
func (m *App) handleCredentialTested(msg credTestedMsg) tea.Cmd {
	if m.cred == nil || m.cred.kind.Type != msg.kind {
		return nil
	}
	if msg.err != nil {
		m.cred.state = credFailed
		m.cred.err = sanitizeLabel(msg.err.Error())
		return nil
	}
	k := m.cred.kind
	m.saveCredential(k, m.cred.values)
	// Provider instances are cached by name, and the one in hand was built
	// before this credential existed. Without dropping it the screen reports a
	// connection while the next prompt still goes out with the old key — or
	// none.
	m.mgr.ReloadProviders()
	m.cred = nil
	m.overlay = overlayNone
	m.setFlash(fmt.Sprintf("%s connected"+gl.sep+"%d models", k.Label, msg.models), false)
	return m.flashCmd()
}

// viewCredential renders the entry overlay.
func (m *App) viewCredential() string {
	if m.cred == nil {
		return ""
	}
	c := m.cred
	var b strings.Builder
	b.WriteString(styAccent.Render("connect "+c.kind.Label) + "\n\n")
	// What this connects to, said rather than inferred. A key from an
	// Anthropic account is billed per token, which is not what someone
	// carrying a Pro or Max plan expects to be signing up for.
	if c.kind.Desc != "" {
		b.WriteString(styDim.Render(c.kind.Desc) + "\n")
	}
	f := c.field()
	if f.Help != "" {
		b.WriteString(styDim.Render(f.Help) + "\n")
	}
	if len(c.fields) > 1 {
		b.WriteString(styDim.Render(fmt.Sprintf("%s (%d of %d)", f.Label, c.at+1, len(c.fields))) + "\n")
	}
	if len(f.Choices) > 0 {
		b.WriteString(styDim.Render("one of: "+strings.Join(f.Choices, ", ")) + "\n")
	}
	if c.kind.URL != "" {
		b.WriteString(styDim.Render(c.kind.URL) + "\n")
	}
	b.WriteString("\n" + c.input.View() + "\n\n")

	switch c.state {
	case credTesting:
		b.WriteString(styDim.Render("checking…"))
	case credFailed:
		b.WriteString(styErr.Render(gl.toolErr + " " + c.err))
	default:
		b.WriteString(styDim.Render("enter checks and saves" + gl.sep + "esc cancels"))
	}

	w := min(m.w-8, 72)
	return borderStyle(true).Width(w).Padding(0, 1).Render(b.String())
}
