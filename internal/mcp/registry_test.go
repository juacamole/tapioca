package mcp

import "testing"

// A server logged in to mid-session is reconnected while its failed entry is
// still in the registry. Added rather than replaced, it would offer every tool
// twice under one name and go on displaying the failure the login just fixed.
func TestReplaceSupersedesAFailedConnection(t *testing.T) {
	reg := NewRegistry()
	reg.SetError("remote", "authorization required")
	first := &Client{Name: "remote", closed: make(chan struct{})}
	reg.Add(first)

	reg.Replace(&Client{Name: "remote", closed: make(chan struct{})})
	if n := len(reg.Clients()); n != 1 {
		t.Fatalf("%d clients under one name after a reconnection, want 1", n)
	}
	if errs := reg.Errors(); len(errs) != 0 {
		t.Errorf("the fixed failure is still on screen: %v", errs)
	}
	if first.Alive() {
		t.Error("the superseded connection was left open")
	}

	// Another server's entry is not disturbed by one being replaced.
	other := &Client{Name: "docs", closed: make(chan struct{})}
	reg.Add(other)
	reg.Replace(&Client{Name: "remote", closed: make(chan struct{})})
	if n := len(reg.Clients()); n != 2 {
		t.Fatalf("%d clients, want the replaced one and docs", n)
	}
	if !other.Alive() {
		t.Error("replacing one server closed another")
	}
}
