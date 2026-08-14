package ui

import (
	"encoding/json"
	"strings"
	"testing"

	"tapioca/internal/agent"
	"tapioca/internal/provider"
)

// wait defaults to blocking, so nothing about existing delegation changes.
func TestSpawnBlocksUnlessToldNotTo(t *testing.T) {
	for _, tc := range []struct {
		args      string
		wantAsync bool
	}{
		{`{"task":"t"}`, false},
		{`{"task":"t","wait":true}`, false},
		{`{"task":"t","wait":false}`, true},
	} {
		var in struct {
			Wait *bool `json:"wait"`
		}
		if err := json.Unmarshal([]byte(tc.args), &in); err != nil {
			t.Fatal(err)
		}
		got := in.Wait != nil && !*in.Wait
		if got != tc.wantAsync {
			t.Errorf("%s gave async=%v, want %v", tc.args, got, tc.wantAsync)
		}
	}
}

// The tool has to advertise it, or no model will use it.
func TestSpawnSchemaOffersWait(t *testing.T) {
	schema := string(agent.SpawnTool.InputSchema)
	if !strings.Contains(schema, `"wait"`) {
		t.Error("spawn_agent does not accept wait")
	}
	if !strings.Contains(schema, `"required":["task"]`) {
		t.Errorf("required fields changed: %s", schema)
	}
}

// A background answer reaches the parent as a pending note, which is the
// existing path for something a parent should know but did not ask for now.
func TestBackgroundAnswerReachesTheParent(t *testing.T) {
	m := dashApp(t, 100, 30)
	parent := m.mgr.ActiveAgent()
	child := m.mgr.Spawn(parent, "search")
	child.Messages = []provider.Message{
		provider.TextMessage("assistant", "found it in parser.go"),
	}

	req := &agent.SpawnReq{Async: true, ParentID: parent.ID, Reply: make(chan agent.SpawnResult, 1)}
	m.spawns[child.ID] = req
	m.finishSpawn(child)

	if len(parent.PendingNotes) != 1 {
		t.Fatalf("the parent has %d pending notes, want 1", len(parent.PendingNotes))
	}
	note := parent.PendingNotes[0]
	if !strings.Contains(note.Text(), "found it in parser.go") {
		t.Errorf("the answer did not reach the parent: %q", note.Text())
	}
	if !note.Hidden {
		t.Error("the note renders in the transcript; it is context, not a turn")
	}
}

// A failed background task must say so rather than deliver nothing, or the
// parent waits for an answer that is never coming.
func TestBackgroundFailureIsReported(t *testing.T) {
	m := dashApp(t, 100, 30)
	parent := m.mgr.ActiveAgent()
	child := m.mgr.Spawn(parent, "doomed")
	child.LastErr = "provider refused"

	req := &agent.SpawnReq{Async: true, ParentID: parent.ID, Reply: make(chan agent.SpawnResult, 1)}
	m.spawns[child.ID] = req
	m.finishSpawn(child)

	if len(parent.PendingNotes) != 1 {
		t.Fatalf("no note delivered for a failure")
	}
	if !strings.Contains(parent.PendingNotes[0].Text(), "provider refused") {
		t.Errorf("the failure was not passed on: %q", parent.PendingNotes[0].Text())
	}
}

// A parent that has gone away must not panic anything.
func TestBackgroundAnswerWithNoParentIsHarmless(t *testing.T) {
	m := dashApp(t, 100, 30)
	parent := m.mgr.ActiveAgent()
	child := m.mgr.Spawn(parent, "orphan")

	req := &agent.SpawnReq{Async: true, ParentID: 9999, Reply: make(chan agent.SpawnResult, 1)}
	m.spawns[child.ID] = req
	m.finishSpawn(child) // must not panic
}

// A blocking spawn is self-limiting — the parent cannot ask for another while
// it waits. A background one is not, so it needs a ceiling.
func TestBackgroundSpawnsAreCapped(t *testing.T) {
	m := dashApp(t, 100, 30)
	parent := m.mgr.ActiveAgent()

	for i := 0; i < maxBackgroundSpawns; i++ {
		child := m.mgr.Spawn(parent, "bg")
		m.spawns[child.ID] = &agent.SpawnReq{Async: true, ParentID: parent.ID}
	}
	if got := m.runningSpawns(); got != maxBackgroundSpawns {
		t.Fatalf("counted %d running, want %d", got, maxBackgroundSpawns)
	}

	req := &agent.SpawnReq{Task: "one too many", Async: true, ParentID: parent.ID, Reply: make(chan agent.SpawnResult, 1)}
	m.startSpawn(parent.ID, req)

	select {
	case res := <-req.Reply:
		if res.Err == nil {
			t.Fatal("the cap was not enforced")
		}
		if !strings.Contains(res.Err.Error(), "limit") {
			t.Errorf("the refusal does not explain itself: %v", res.Err)
		}
	default:
		t.Fatal("the over-cap spawn neither started nor was refused")
	}
}

// Blocking spawns do not count against the background cap: they cannot pile up.
func TestBlockingSpawnsDoNotCountTowardTheCap(t *testing.T) {
	m := dashApp(t, 100, 30)
	parent := m.mgr.ActiveAgent()
	for i := 0; i < 10; i++ {
		child := m.mgr.Spawn(parent, "sync")
		m.spawns[child.ID] = &agent.SpawnReq{Async: false, ParentID: parent.ID}
	}
	if got := m.runningSpawns(); got != 0 {
		t.Errorf("blocking spawns counted %d against the background cap", got)
	}
}
