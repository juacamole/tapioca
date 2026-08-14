package provider

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Anthropic issues OAuth credentials through its own CLI: `ant auth login`
// opens a browser and stores a profile under ~/.config/anthropic. Tapioca
// implements no OAuth of its own — it asks the CLI for a token, which is the
// same thing the official SDKs do.
//
// The token is short-lived and is not refreshed once handed out, so it is
// fetched per request rather than held for the session. `print-credentials`
// refreshes on call, which is also why this shells out instead of reading the
// credentials file: the file has no refresh logic behind it.

// antBinary is the CLI's name. Held in a variable so tests can point at a stub
// rather than requiring the real CLI and a real login.
var antBinary = "ant"

// oauthFetchTimeout bounds the CLI call. It may perform a token refresh over
// the network, so this is not merely process-spawn time.
const oauthFetchTimeout = 20 * time.Second

// ErrNoAntCLI means the CLI is absent, which is a different problem from being
// logged out and has a different fix.
var ErrNoAntCLI = errors.New("the Anthropic CLI (ant) is not installed")

// oauthToken returns a bearer token from the active CLI profile.
//
// The flag matters: `ant auth print-credentials` with no flags prints the whole
// credentials JSON, and using that as a header yields an empty response rather
// than an error — a failure that looks like the model returning nothing.
func oauthToken(ctx context.Context) (string, error) {
	if _, err := exec.LookPath(antBinary); err != nil {
		return "", ErrNoAntCLI
	}
	ctx, cancel := context.WithTimeout(ctx, oauthFetchTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, antBinary, "auth", "print-credentials", "--access-token")
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			msg := strings.TrimSpace(string(ee.Stderr))
			if msg == "" {
				msg = "no active profile"
			}
			return "", fmt.Errorf("anthropic oauth: %s (run `ant auth login`)", msg)
		}
		return "", fmt.Errorf("anthropic oauth: %w", err)
	}
	tok := strings.TrimSpace(string(out))
	if tok == "" {
		return "", fmt.Errorf("anthropic oauth: the CLI returned no token (run `ant auth login`)")
	}
	// A JSON object here means the bare-token flag did not take effect, which
	// would otherwise be sent as a header and fail as an empty model response.
	if strings.HasPrefix(tok, "{") {
		return "", fmt.Errorf("anthropic oauth: the CLI returned JSON rather than a token — " +
			"its print-credentials flags have changed")
	}
	return tok, nil
}

// LoginCommand returns the command that performs a browser login, for a caller
// that can hand it the terminal. It is not run here: the CLI prints a URL and
// waits, and a caller that swallows its output leaves a user with no way to
// reach the browser flow on a machine where no browser can be opened.
func LoginCommand() *exec.Cmd {
	return exec.Command(antBinary, "auth", "login")
}

// oauthShadowed reports the environment variable that will be preferred over
// an OAuth profile. Credential resolution puts API keys ahead of profiles, so
// a stale exported key makes a successful login look like it did nothing.
func oauthShadowed() string {
	for _, name := range []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN"} {
		if os.Getenv(name) != "" {
			return name
		}
	}
	return ""
}

// installHint names a command that will actually work on this machine. A
// single hardcoded suggestion is wrong for most people: Homebrew is not
// present on a NixOS or plain Linux box, and telling someone to run a command
// they do not have is the same failure as telling them nothing.
//
// Nix users get the Go command rather than a nixpkgs one on purpose. The CLI
// is not in nixpkgs, and `nix-shell -p ant` installs Apache Ant — a Java build
// tool that shares the name and will fail at `ant auth login` in a way that
// looks like a Tapioca bug.
func installHint() string {
	if _, err := exec.LookPath("brew"); err == nil {
		return "brew install anthropics/tap/ant"
	}
	return "go install github.com/anthropics/anthropic-cli/cmd/ant@latest"
}

// wrongAnt reports whether the ant on PATH is Apache Ant rather than the
// Anthropic CLI. They share a name, Apache Ant is what package managers hand
// you when you ask for "ant", and its failures are unrecognisable as the wrong
// program: an unknown-argument complaint, or "build.xml does not exist". A
// user who has just been told to install the Anthropic CLI reads those as a
// bug in this app.
func wrongAnt() bool {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, _ := exec.CommandContext(ctx, antBinary, "-version").CombinedOutput()
	return strings.Contains(string(out), "Apache Ant")
}

// AnthropicOAuthAvailable reports whether a login could be attempted, and why
// not when it could not. The UI needs the reason: an absent CLI, the wrong
// program under the same name, and an absent login are three different
// problems with three different fixes, and reporting any of them as "auth
// failed" sends the user looking in the wrong place.
func AnthropicOAuthAvailable() (bool, string) {
	if _, err := exec.LookPath(antBinary); err != nil {
		return false, "the Anthropic CLI is not installed" + " — " + installHint()
	}
	if wrongAnt() {
		return false, "the `ant` on your PATH is Apache Ant, not the Anthropic CLI — " + installHint()
	}
	if _, err := oauthToken(context.Background()); err != nil {
		if errors.Is(err, ErrNoAntCLI) {
			return false, "the Anthropic CLI is not installed" + " — " + installHint()
		}
		return false, err.Error()
	}
	if env := oauthShadowed(); env != "" {
		return true, "logged in, but $" + env + " is set and takes precedence — unset it to use the login"
	}
	return true, "logged in"
}
