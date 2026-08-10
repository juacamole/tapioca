package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// A dev server, a watch build, a long test run: things you start and then keep
// working alongside. The foreground path cannot express that — it blocks the
// turn until the command exits — so these run detached and their output is
// collected for the agent to poll.

const (
	maxJobOutput = 200_000 // per job; the oldest output is dropped first
	maxJobs      = 20
)

type job struct {
	id      string
	command string
	started time.Time

	cancel context.CancelFunc

	mu       sync.Mutex
	output   []byte
	read     int // how much the agent has already been shown
	dropped  int // bytes discarded to stay under the cap
	done     bool
	exitErr  error
	finished time.Time
}

// Write collects output as the process produces it, keeping the newest.
func (j *job) Write(p []byte) (int, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.output = append(j.output, p...)
	if over := len(j.output) - maxJobOutput; over > 0 {
		j.output = j.output[over:]
		j.dropped += over
		if j.read > over {
			j.read -= over
		} else {
			j.read = 0
		}
	}
	return len(p), nil
}

// drain returns output not yet shown, and the job's current state.
func (j *job) drain() (text string, done bool, err error, dropped int) {
	j.mu.Lock()
	defer j.mu.Unlock()
	text = string(j.output[j.read:])
	j.read = len(j.output)
	dropped, j.dropped = j.dropped, 0
	return text, j.done, j.exitErr, dropped
}

func (j *job) finish(err error) {
	j.mu.Lock()
	j.done, j.exitErr, j.finished = true, err, time.Now()
	j.mu.Unlock()
}

func (j *job) isDone() bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.done
}

// startBackground launches a command and returns its job id immediately.
func (e *Executor) startBackground(command string) (string, error) {
	e.mu.Lock()
	if e.jobs == nil {
		e.jobs = map[string]*job{}
	}
	// Finished jobs are kept so their output can still be collected; only
	// prune when there are too many, oldest finished first.
	if len(e.jobs) >= maxJobs {
		e.pruneJobsLocked()
	}
	if len(e.jobs) >= maxJobs {
		e.mu.Unlock()
		return "", fmt.Errorf("too many background jobs (%d); collect or kill some first", maxJobs)
	}
	e.jobSeq++
	id := fmt.Sprintf("job-%d", e.jobSeq)
	e.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	cmd, err := e.bashCommand(ctx, command)
	if err != nil {
		cancel()
		return "", err
	}
	j := &job{id: id, command: command, started: time.Now(), cancel: cancel}
	cmd.Stdout = j
	cmd.Stderr = j
	if err := cmd.Start(); err != nil {
		cancel()
		return "", err
	}

	e.mu.Lock()
	e.jobs[id] = j
	e.mu.Unlock()

	go func() {
		err := cmd.Wait()
		if err != nil && strings.Contains(err.Error(), exec.ErrWaitDelay.Error()) {
			err = nil // a lingering child held the pipe; the command itself was fine
		}
		j.finish(err)
		cancel()
	}()
	return id, nil
}

// pruneJobsLocked drops the oldest finished jobs. The caller holds e.mu.
func (e *Executor) pruneJobsLocked() {
	var oldest *job
	for _, j := range e.jobs {
		if !j.isDone() {
			continue
		}
		if oldest == nil || j.finished.Before(oldest.finished) {
			oldest = j
		}
	}
	if oldest != nil {
		delete(e.jobs, oldest.id)
	}
}

func (e *Executor) job(id string) *job {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.jobs[id]
}

// jobOutput returns whatever a background job has produced since last asked.
func (e *Executor) jobOutput(raw json.RawMessage) (string, bool, error) {
	var a struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(raw, &a)
	if strings.TrimSpace(a.ID) == "" {
		return e.jobList(), false, nil
	}
	j := e.job(a.ID)
	if j == nil {
		return "no such job " + a.ID + "\n" + e.jobList(), true, nil
	}
	text, done, exitErr, dropped := j.drain()

	var b strings.Builder
	status := "running"
	if done {
		status = "finished"
		if exitErr != nil {
			status = "failed: " + exitErr.Error()
		}
	}
	fmt.Fprintf(&b, "%s (%s) — %s\n", j.id, j.command, status)
	if dropped > 0 {
		fmt.Fprintf(&b, "[%d earlier bytes dropped]\n", dropped)
	}
	if strings.TrimSpace(text) == "" {
		b.WriteString("(no new output)")
	} else {
		b.WriteString(text)
	}
	return capOutput(b.String()), false, nil
}

func (e *Executor) jobList() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.jobs) == 0 {
		return "no background jobs"
	}
	var lines []string
	for _, j := range e.jobs {
		state := "running"
		if j.isDone() {
			state = "finished"
		}
		lines = append(lines, fmt.Sprintf("%s  %s  %s", j.id, state, truncateLabel(j.command)))
	}
	return "background jobs:\n" + strings.Join(lines, "\n")
}

// killJob stops a background job and reports what it had produced.
func (e *Executor) killJob(raw json.RawMessage) (string, bool, error) {
	var a struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(raw, &a) != nil || strings.TrimSpace(a.ID) == "" {
		return "invalid arguments: need {\"id\": \"job-1\"}", true, nil
	}
	j := e.job(a.ID)
	if j == nil {
		return "no such job " + a.ID, true, nil
	}
	j.cancel()
	// Give the process a moment to die so the reply reflects reality.
	for i := 0; i < 20 && !j.isDone(); i++ {
		time.Sleep(50 * time.Millisecond)
	}
	e.mu.Lock()
	delete(e.jobs, a.ID)
	e.mu.Unlock()
	return "killed " + a.ID, false, nil
}

// StopJobs kills every background job; the app calls this on the way out so
// nothing is left running after it exits.
func (e *Executor) StopJobs() {
	e.mu.Lock()
	jobs := make([]*job, 0, len(e.jobs))
	for _, j := range e.jobs {
		jobs = append(jobs, j)
	}
	e.jobs = map[string]*job{}
	e.mu.Unlock()
	for _, j := range jobs {
		j.cancel()
	}
}
