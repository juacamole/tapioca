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

// AnthropicOAuthAvailable reports whether a login could be attempted, and why
// not when it could not. The UI needs the reason: an absent CLI and an absent
// login have different fixes, and reporting either as "auth failed" sends the
// user looking in the wrong place.
func AnthropicOAuthAvailable() (bool, string) {
	if _, err := exec.LookPath(antBinary); err != nil {
		return false, "install the Anthropic CLI: brew install anthropics/tap/ant"
	}
	if _, err := oauthToken(context.Background()); err != nil {
		if errors.Is(err, ErrNoAntCLI) {
			return false, "install the Anthropic CLI: brew install anthropics/tap/ant"
		}
		return false, err.Error()
	}
	if env := oauthShadowed(); env != "" {
		return true, "logged in, but $" + env + " is set and takes precedence — unset it to use the login"
	}
	return true, "logged in"
}
