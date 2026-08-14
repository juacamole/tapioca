// Package version holds the release number, in one place.
//
// It used to be a const in package main plus two hand-copied literals in the
// ACP and MCP handshakes, which is three chances to forget one at release time
// and tell a peer the wrong thing.
package version

// Version is the release. flake.nix carries it a second time, because Nix
// cannot read a Go constant; the release workflow checks the two agree.
const Version = "1.1.0"
