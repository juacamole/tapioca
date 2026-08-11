package session

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"hash/fnv"
	"os"
	"sync"
	"time"

	"tapioca/internal/provider"
	"tapioca/internal/stats"
)

// Saving used to rewrite the entire workspace — every message of every agent —
// after each completed turn and on every autosave tick. A session with a few
// large tool results is easily three quarters of a megabyte, so that was ~10ms
// of marshalling and disk per turn, and the idle tick rewrote bytes that had
// not changed at all.
//
// A turn almost always only *appends* messages, so appended messages and the
// small per-agent tail (queue, todos, stats) go to a JSONL journal beside the
// snapshot. The snapshot is rewritten only when history is rewritten
// (/compact, /regen, /edit), when settings change, or when the journal has
// grown large enough that replaying it stops being cheap.

// journalFloor is the size a journal must reach before it can trigger a
// rewrite: under it the snapshot is small enough that rewriting costs nothing
// worth avoiding.
const journalFloor = 64 << 10

func journalPath(id string) string { return pathFor(id) + ".log" }

// agentTail is the part of an agent that changes every turn without rewriting
// history.
type agentTail struct {
	Queue     []provider.Message `json:"queue,omitempty"`
	Todos     []TodoItem         `json:"todos,omitempty"`
	CtxTokens int                `json:"ctx,omitempty"`
	Stats     stats.Stats        `json:"stats"`
}

// journalRec is one incremental save: what each agent gained, in snapshot
// order, plus the fields that change without touching history.
type journalRec struct {
	UpdatedAt time.Time            `json:"updated_at"`
	Name      string               `json:"name,omitempty"`
	Active    int                  `json:"active"`
	Added     [][]provider.Message `json:"added"`
	Tails     []agentTail          `json:"tails"`
}

// agentMark is what the last write left on disk for one agent, enough to tell
// an append from a rewrite without re-reading the file.
type agentMark struct {
	cfg    uint64 // everything but history and tail: a settings change forces a snapshot
	tail   uint64
	shapes []uint64 // one per written message, so a rewritten one is caught
}

// savedState is the on-disk state this process last wrote for one session.
type savedState struct {
	snapSize, snapMod int64
	jrnSize, jrnMod   int64
	name, cwd         string
	created           time.Time
	active            int
	agents            []agentMark
}

var (
	savedMu  sync.Mutex
	savedAll = map[string]*savedState{}
)

func hashJSON(v any) uint64 {
	data, err := json.Marshal(v)
	if err != nil {
		return 0 // unhashable compares unequal, which forces a snapshot
	}
	h := fnv.New64a()
	h.Write(data)
	return h.Sum64()
}

// cfgHash covers every agent field except history and tail, by clearing those
// and hashing the rest: a field added to AgentState is then included by
// default rather than silently escaping the check.
func cfgHash(a AgentState) uint64 {
	a.Messages, a.Queue, a.Todos = nil, nil, nil
	a.Stats, a.CtxTokens = stats.Stats{}, 0
	return hashJSON(a)
}

// msgShape fingerprints a message without marshalling it. Comparing the whole
// prefix this way is what makes an append safe to assume: a message rewritten
// in place changes its timestamp or the size of something in it, and walking
// blocks costs nothing next to serialising megabytes of history.
func msgShape(m *provider.Message) uint64 {
	h := fnv.New64a()
	var buf [8]byte
	num := func(v int64) {
		binary.LittleEndian.PutUint64(buf[:], uint64(v))
		h.Write(buf[:])
	}
	h.Write([]byte(m.Role))
	h.Write([]byte(m.Model))
	num(m.Time.UnixNano())
	num(int64(len(m.Typed)))
	num(int64(len(m.Blocks)))
	for i := range m.Blocks {
		b := &m.Blocks[i]
		h.Write([]byte(b.Type))
		h.Write([]byte(b.ID))
		h.Write([]byte(b.Name))
		num(int64(len(b.Text)))
		num(int64(len(b.Content)))
		num(int64(len(b.Input)))
		num(int64(len(b.Data)))
		num(int64(len(b.Display)))
	}
	return h.Sum64()
}

func shapesOf(msgs []provider.Message) []uint64 {
	out := make([]uint64, len(msgs))
	for i := range msgs {
		out[i] = msgShape(&msgs[i])
	}
	return out
}

func samePrefix(shapes, prev []uint64) bool {
	if len(shapes) < len(prev) {
		return false
	}
	for i, h := range prev {
		if shapes[i] != h {
			return false
		}
	}
	return true
}

func tailOf(a AgentState) agentTail {
	return agentTail{Queue: a.Queue, Todos: a.Todos, CtxTokens: a.CtxTokens, Stats: a.Stats}
}

// fresh reports whether the files still look like what this process wrote. An
// edit behind our back means the marks describe something that is no longer
// there, and appending to it would produce a session that never existed.
func (st *savedState) fresh(id string) bool {
	info, err := os.Stat(pathFor(id))
	if err != nil || info.Size() != st.snapSize || info.ModTime().UnixNano() != st.snapMod {
		return false
	}
	info, err = os.Stat(journalPath(id))
	if os.IsNotExist(err) {
		return st.jrnSize == 0
	}
	return err == nil && info.Size() == st.jrnSize && info.ModTime().UnixNano() == st.jrnMod
}

// delta describes s against what was last written. ok is false when the
// difference cannot be expressed as an append and the snapshot has to be
// rewritten; a nil rec with ok means nothing changed and there is nothing to
// write at all.
func (st *savedState) delta(s *Session) (rec *journalRec, marks []agentMark, ok bool) {
	if s.Name != st.name || s.Cwd != st.cwd || !s.CreatedAt.Equal(st.created) ||
		len(s.Agents) != len(st.agents) {
		return nil, nil, false
	}
	rec = &journalRec{Name: s.Name, Active: s.Active, Added: make([][]provider.Message, len(s.Agents)),
		Tails: make([]agentTail, len(s.Agents))}
	marks = make([]agentMark, len(s.Agents))
	changed := s.Active != st.active

	for i, a := range s.Agents {
		old := st.agents[i]
		cfg := cfgHash(a)
		shapes := shapesOf(a.Messages)
		if cfg == 0 || old.cfg == 0 || cfg != old.cfg || !samePrefix(shapes, old.shapes) {
			return nil, nil, false
		}
		tail := tailOf(a)
		mark := agentMark{cfg: cfg, tail: hashJSON(tail), shapes: shapes}
		if mark.tail == 0 {
			return nil, nil, false
		}
		if len(shapes) > len(old.shapes) {
			rec.Added[i] = a.Messages[len(old.shapes):]
			changed = true
		}
		rec.Tails[i] = tail
		if mark.tail != old.tail {
			changed = true
		}
		marks[i] = mark
	}
	if !changed {
		return nil, nil, true
	}
	return rec, marks, true
}

// appendRecord adds one line to the journal. It reports false when the write
// would make the journal big enough that a fresh snapshot is the better deal,
// leaving the caller to write one.
func (st *savedState) appendRecord(id string, rec *journalRec, marks []agentMark) (bool, error) {
	rec.UpdatedAt = time.Now()
	line, err := json.Marshal(rec)
	if err != nil {
		return false, nil // fall back to a snapshot rather than fail the save
	}
	line = append(line, '\n')

	limit := int64(journalFloor)
	if half := st.snapSize / 2; half > limit {
		limit = half
	}
	if st.jrnSize+int64(len(line)) > limit {
		return false, nil
	}

	f, err := os.OpenFile(journalPath(id), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return false, nil
	}
	if _, err := f.Write(line); err != nil {
		f.Close()
		// A short write leaves a half line behind; replay stops there, so the
		// snapshot that follows is what makes the session whole again.
		return false, nil
	}
	if err := f.Close(); err != nil {
		return false, nil
	}
	info, err := os.Stat(journalPath(id))
	if err != nil {
		return false, nil
	}
	st.jrnSize, st.jrnMod = info.Size(), info.ModTime().UnixNano()
	st.active, st.agents = rec.Active, marks
	return true, nil
}

// markSnapshot records the state a full write just left on disk.
func markSnapshot(s *Session, info os.FileInfo) {
	st := &savedState{
		snapSize: info.Size(), snapMod: info.ModTime().UnixNano(),
		name: s.Name, cwd: s.Cwd, created: s.CreatedAt, active: s.Active,
		agents: make([]agentMark, len(s.Agents)),
	}
	for i, a := range s.Agents {
		st.agents[i] = agentMark{cfg: cfgHash(a), tail: hashJSON(tailOf(a)), shapes: shapesOf(a.Messages)}
	}
	savedMu.Lock()
	savedAll[s.ID] = st
	savedMu.Unlock()
}

// applyJournal replays the journal onto a freshly loaded snapshot. Anything
// unreadable ends the replay instead of failing the load: losing the tail of a
// session beats not opening it.
func applyJournal(s *Session, id string) {
	f, err := os.Open(journalPath(id))
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), 16<<20)
	for sc.Scan() {
		var rec journalRec
		if json.Unmarshal(sc.Bytes(), &rec) != nil {
			return
		}
		if len(rec.Added) != len(s.Agents) || len(rec.Tails) != len(s.Agents) {
			return
		}
		for i := range s.Agents {
			a := &s.Agents[i]
			a.Messages = append(a.Messages, rec.Added[i]...)
			t := rec.Tails[i]
			a.Queue, a.Todos, a.CtxTokens, a.Stats = t.Queue, t.Todos, t.CtxTokens, t.Stats
		}
		s.Active, s.UpdatedAt = rec.Active, rec.UpdatedAt
		if rec.Name != "" {
			s.Name = rec.Name
		}
	}
}
