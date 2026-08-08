package agent

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"tapioca/internal/config"
	"tapioca/internal/session"
)

func TestWriteTodosReplacesAndNormalizes(t *testing.T) {
	a := &Agent{}
	out, isErr := a.writeTodos(json.RawMessage(`{"todos":[
		{"content":"read the parser","status":"done"},
		{"content":"fix the bug","status":"doing"},
		{"content":"  ","status":"pending"},
		{"content":"add a test","status":"nonsense"}]}`))
	if isErr {
		t.Fatalf("writeTodos failed: %s", out)
	}
	if len(a.Todos) != 3 {
		t.Fatalf("blank item kept: %+v", a.Todos)
	}
	if a.Todos[0].Status != TodoDone || a.Todos[1].Status != TodoDoing || a.Todos[2].Status != TodoPending {
		t.Fatalf("statuses not normalized: %+v", a.Todos)
	}
	if done, total := a.TodoProgress(); done != 1 || total != 3 {
		t.Fatalf("progress = %d/%d, want 1/3", done, total)
	}
	if !strings.Contains(out, "[x] read the parser") || !strings.Contains(out, "(1/3 done)") {
		t.Fatalf("result does not echo the list back:\n%s", out)
	}

	// A second call replaces rather than appends.
	if _, isErr := a.writeTodos(json.RawMessage(`{"todos":[{"content":"only this","status":"pending"}]}`)); isErr {
		t.Fatal("second write failed")
	}
	if len(a.Todos) != 1 || a.Todos[0].Content != "only this" {
		t.Fatalf("list was not replaced: %+v", a.Todos)
	}
}

func TestTodosSurviveSessionRoundTrip(t *testing.T) {
	mgr := NewManager(&config.Config{}, nil, nil)
	mgr.Agents = []*Agent{{
		Name:  "agent-1",
		Todos: []TodoItem{{Content: "step one", Status: TodoDone}, {Content: "step two", Status: TodoDoing}},
	}}
	s := mgr.ToSession("id", "name", time.Now())

	data, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	var restored session.Session
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatal(err)
	}
	mgr.LoadSession(&restored)
	got := mgr.Agents[0].Todos
	if len(got) != 2 || got[0].Content != "step one" || got[1].Status != TodoDoing {
		t.Fatalf("todos lost across save/load: %+v", got)
	}
}

func TestWriteTodosRejectsGarbage(t *testing.T) {
	a := &Agent{Todos: []TodoItem{{Content: "keep me", Status: TodoDoing}}}
	if _, isErr := a.writeTodos(json.RawMessage(`{"todos":"not a list"}`)); !isErr {
		t.Fatal("malformed input accepted")
	}
	if len(a.Todos) != 1 {
		t.Fatalf("malformed input clobbered the list: %+v", a.Todos)
	}
	if out, isErr := a.writeTodos(json.RawMessage(`{"todos":[]}`)); isErr || !strings.Contains(out, "cleared") {
		t.Fatalf("empty list should clear: out=%q isErr=%v", out, isErr)
	}
}
