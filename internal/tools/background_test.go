package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func bg(t *testing.T, e *Executor, command string) string {
	t.Helper()
	raw, _ := json.Marshal(map[string]any{"command": command, "background": true})
	out, isErr, err := e.Call(context.Background(), "bash", raw, allow)
	if err != nil || isErr {
		t.Fatalf("starting background job: %q %v", out, err)
	}
	id := ""
	for _, f := range strings.Fields(out) {
		if strings.HasPrefix(f, "job-") {
			id = f
		}
	}
	if id == "" {
		t.Fatalf("no job id in %q", out)
	}
	return id
}

func poll(t *testing.T, e *Executor, id string) string {
	t.Helper()
	raw, _ := json.Marshal(map[string]any{"id": id})
	out, _, err := e.Call(context.Background(), "bash_output", raw, allow)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// The point of the feature: start something slow, keep working, collect its
// output as it appears rather than blocking the turn until it exits.
func TestBackgroundJobStreamsOutputAndFinishes(t *testing.T) {
	e := NewExecutor(t.TempDir(), ModeBypass)
	defer e.StopJobs()

	start := time.Now()
	id := bg(t, e, "echo first; sleep 1; echo second")
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Errorf("starting blocked for %v; it should return at once", elapsed)
	}

	// Output arrives incrementally, and each poll only shows what is new.
	// Poll until the job reports finishing; seeing its output does not mean
	// the process has been reaped yet.
	var seenFirst, seenSecond, finished bool
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) && !finished {
		out := poll(t, e, id)
		if strings.Contains(out, "first") {
			seenFirst = true
		}
		if strings.Contains(out, "second") {
			seenSecond = true
		}
		if strings.Contains(out, "finished") {
			finished = true
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if !seenFirst || !seenSecond {
		t.Fatalf("missed output (first=%v second=%v)", seenFirst, seenSecond)
	}
	if !finished {
		t.Fatal("job never reported finishing")
	}
	// Everything has been collected, so there is nothing new left.
	if out := poll(t, e, id); !strings.Contains(out, "no new output") {
		t.Errorf("output repeated on a second poll: %q", out)
	}
}

func TestBackgroundJobCanBeKilled(t *testing.T) {
	e := NewExecutor(t.TempDir(), ModeBypass)
	defer e.StopJobs()
	id := bg(t, e, "sleep 60")

	raw, _ := json.Marshal(map[string]any{"id": id})
	out, isErr, err := e.Call(context.Background(), "bash_kill", raw, allow)
	if err != nil || isErr || !strings.Contains(out, "killed") {
		t.Fatalf("kill failed: %q %v", out, err)
	}
	if out := poll(t, e, id); !strings.Contains(out, "no such job") {
		t.Errorf("killed job still listed: %q", out)
	}
}

func TestFailedBackgroundJobReportsIt(t *testing.T) {
	e := NewExecutor(t.TempDir(), ModeBypass)
	defer e.StopJobs()
	id := bg(t, e, "echo oops >&2; exit 3")

	deadline := time.Now().Add(10 * time.Second)
	var out string
	for time.Now().Before(deadline) {
		out = poll(t, e, id)
		if strings.Contains(out, "failed") {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !strings.Contains(out, "failed") {
		t.Fatalf("a non-zero exit was not reported: %q", out)
	}
	if !strings.Contains(out, "oops") {
		t.Errorf("stderr was not collected: %q", out)
	}
}

func TestListingJobsWithoutAnID(t *testing.T) {
	e := NewExecutor(t.TempDir(), ModeBypass)
	defer e.StopJobs()

	out, _, _ := e.Call(context.Background(), "bash_output", json.RawMessage(`{}`), allow)
	if !strings.Contains(out, "no background jobs") {
		t.Errorf("expected an empty listing, got %q", out)
	}
	id := bg(t, e, "sleep 30")
	out, _, _ = e.Call(context.Background(), "bash_output", json.RawMessage(`{}`), allow)
	if !strings.Contains(out, id) || !strings.Contains(out, "sleep 30") {
		t.Errorf("job missing from the listing: %q", out)
	}
}

// Polling must not ask permission again: starting the command was the
// decision, and a prompt per poll would make the feature unusable.
func TestPollingDoesNotPrompt(t *testing.T) {
	e := NewExecutor(t.TempDir(), ModeManual)
	defer e.StopJobs()

	var log []string
	raw, _ := json.Marshal(map[string]any{"command": "echo hi", "background": true})
	out, _, err := e.Call(context.Background(), "bash", raw, asks(Decision{Allow: true}, &log))
	if err != nil {
		t.Fatal(err)
	}
	if len(log) != 1 {
		t.Fatalf("starting should prompt exactly once, got %v", log)
	}
	id := strings.Fields(out)[1]

	log = nil
	e.Call(context.Background(), "bash_output", json.RawMessage(`{"id":"`+id+`"}`), asks(Decision{}, &log))
	e.Call(context.Background(), "bash_kill", json.RawMessage(`{"id":"`+id+`"}`), asks(Decision{}, &log))
	if len(log) != 0 {
		t.Errorf("polling or killing prompted: %v", log)
	}
}

func TestStopJobsKillsEverything(t *testing.T) {
	e := NewExecutor(t.TempDir(), ModeBypass)
	id := bg(t, e, "sleep 60")
	j := e.job(id)
	e.StopJobs()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && !j.isDone() {
		time.Sleep(50 * time.Millisecond)
	}
	if !j.isDone() {
		t.Error("a job survived StopJobs")
	}
	if out := poll(t, e, id); !strings.Contains(out, "no such job") {
		t.Errorf("job still registered: %q", out)
	}
}

// Output is capped so a chatty process cannot grow without bound.
func TestJobOutputIsCapped(t *testing.T) {
	j := &job{}
	big := strings.Repeat("x", maxJobOutput+5000)
	j.Write([]byte(big))
	text, _, _, dropped := j.drain()
	if len(text) > maxJobOutput {
		t.Errorf("kept %d bytes, cap is %d", len(text), maxJobOutput)
	}
	if dropped == 0 {
		t.Error("dropped bytes were not reported")
	}
}
