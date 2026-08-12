package ui

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

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
	kind  provider.Kind
	field provider.Field
	input textinput.Model
	state credState
	err   string
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
	// Prefer the secret among the required fields. Taking merely the first
	// would open an echoing form for a provider whose first field happens to
	// be an endpoint, and put the credential behind it.
	var field provider.Field
	for _, f := range k.Fields {
		if !f.Optional && f.Secret {
			field = f
			break
		}
	}
	if field.Key == "" {
		for _, f := range k.Fields {
			if !f.Optional {
				field = f
				break
			}
		}
	}
	in := textinput.New()
	in.Placeholder = field.Label
	in.Prompt = "> "
	in.CharLimit = 512
	if field.Secret {
		in.EchoMode = textinput.EchoPassword
		in.EchoCharacter = '•'
	}
	in.Focus()
	in.Width = min(60, max(20, m.w-24))

	m.cred = &credForm{kind: k, field: field, input: in}
	m.overlay = overlayCredential
}

// testCredential verifies the value against the provider before anything is
// written. A key that does not work must fail here rather than on the user's
// next prompt.
func testCredential(k provider.Kind, field provider.Field, value string) tea.Cmd {
	pc := config.ProviderConfig{Type: k.Type}
	applyField(&pc, field.Key, value)
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
	}
}

// saveCredential writes a verified credential. It prefers an environment
// variable reference over the literal: the config file is read by every tool
// the user points at their dotfiles, and a key that is not there cannot leak
// from there. The literal is used only when the variable is not actually set,
// since a reference to an unset variable is a provider that silently does not
// work.
func (m *App) saveCredential(k provider.Kind, field provider.Field, value string) {
	pc := m.cfg.Providers[k.Type]
	if pc.Type == "" {
		pc.Type = k.Type
	}
	if field.Secret {
		if env := envHolding(value); env != "" {
			pc.APIKey = ""
			pc.APIKeyEnv = env
		} else {
			pc.APIKey = value
			pc.APIKeyEnv = ""
		}
	} else {
		applyField(&pc, field.Key, value)
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
func (m *App) handleCredentialKey(msg tea.KeyMsg) (tea.Cmd, bool) {
	if m.cred == nil {
		return nil, false
	}
	switch msg.String() {
	case "esc":
		m.cred = nil
		m.overlay = overlayNone
		return nil, true
	case "enter":
		if m.cred.state == credTesting {
			return nil, true
		}
		value := strings.TrimSpace(m.cred.input.Value())
		if value == "" {
			m.cred.err = m.cred.field.Label + " cannot be empty"
			m.cred.state = credFailed
			return nil, true
		}
		m.cred.state = credTesting
		m.cred.err = ""
		return testCredential(m.cred.kind, m.cred.field, value), true
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
	k, field := m.cred.kind, m.cred.field
	value := strings.TrimSpace(m.cred.input.Value())
	m.saveCredential(k, field, value)
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
	if c.field.Help != "" {
		b.WriteString(styDim.Render(c.field.Help) + "\n")
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
