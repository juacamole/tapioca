package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Listing used to load and parse every saved session — full message history —
// to build the picker, and `-c` goes through the same path, so starting the app
// got slower with every conversation you had ever had. The summaries are cached
// here instead, keyed by file size and modification time so a session edited or
// added behind our back is re-read and everything else is skipped.

// The search text is 97% of the cached data and only /resume's filter reads
// it, so it lives in its own file: `-c` should not pay to load a megabyte of
// conversation text it will never look at.
const (
	indexName  = "index.json"
	searchName = "search.json"
)

func searchPath() string { return filepath.Join(Dir(), searchName) }

func loadSearchText() map[string]string {
	out := map[string]string{}
	data, err := os.ReadFile(searchPath())
	if err != nil {
		return out
	}
	if json.Unmarshal(data, &out) != nil {
		return map[string]string{}
	}
	return out
}

func saveSearchText(m map[string]string) {
	data, err := json.Marshal(m)
	if err != nil {
		return
	}
	tmp := searchPath() + ".tmp"
	if os.WriteFile(tmp, data, 0o600) != nil {
		return
	}
	if os.Rename(tmp, searchPath()) != nil {
		os.Remove(tmp)
	}
}

// ListWithText is List plus the per-session text the picker filters on.
func ListWithText() ([]Meta, error) {
	metas, err := List()
	if err != nil {
		return nil, err
	}
	text := loadSearchText()
	for i := range metas {
		metas[i].Blob = text[metas[i].ID]
	}
	return metas, nil
}

// A session's most recent turns live in its journal, and appending to one
// leaves the snapshot untouched, so the journal's size and mtime are part of
// the key too — otherwise the picker would keep showing yesterday's summary.
type indexEntry struct {
	Meta  Meta  `json:"meta"`
	Size  int64 `json:"size"`
	Mod   int64 `json:"mod"` // unix nanoseconds
	JSize int64 `json:"jsize"`
	JMod  int64 `json:"jmod"`
}

type sessionIndex struct {
	Version int                   `json:"version"`
	Entries map[string]indexEntry `json:"entries"`
}

const indexVersion = 3

func indexPath() string { return filepath.Join(Dir(), indexName) }

func loadIndex() sessionIndex {
	idx := sessionIndex{Version: indexVersion, Entries: map[string]indexEntry{}}
	data, err := os.ReadFile(indexPath())
	if err != nil {
		return idx
	}
	var stored sessionIndex
	if json.Unmarshal(data, &stored) != nil || stored.Version != indexVersion || stored.Entries == nil {
		return idx // a format change invalidates the cache rather than breaking
	}
	return stored
}

// save writes the index; failure is not fatal, the next listing just re-reads.
func (idx sessionIndex) save() {
	data, err := json.Marshal(idx)
	if err != nil {
		return
	}
	tmp := indexPath() + ".tmp"
	if os.WriteFile(tmp, data, 0o600) != nil {
		return
	}
	if os.Rename(tmp, indexPath()) != nil {
		os.Remove(tmp)
	}
}

// journalStamp returns the journal's size and mtime, zero when there is none.
func journalStamp(id string) (int64, int64) {
	info, err := os.Stat(journalPath(id))
	if err != nil {
		return 0, 0
	}
	return info.Size(), info.ModTime().UnixNano()
}

// summarize builds the Meta for one session file.
func summarize(id string) (Meta, bool) {
	s, err := Load(id)
	if err != nil {
		return Meta{}, false
	}
	msgs := 0
	var blob strings.Builder
	for _, a := range s.Agents {
		msgs += len(a.Messages)
		if blob.Len() > searchBlobMax {
			continue
		}
		for _, m := range a.Messages {
			if blob.Len() > searchBlobMax {
				break
			}
			if !m.IsToolResult() {
				blob.WriteString(strings.ToLower(m.Text()))
				blob.WriteString("\n")
			}
		}
	}
	return Meta{
		ID: s.ID, Name: s.Name, Cwd: s.Cwd, UpdatedAt: s.UpdatedAt,
		Agents: len(s.Agents), Messages: msgs, Blob: blob.String(),
	}, true
}

// List returns stored sessions, newest first, parsing only the files whose
// summaries are missing or out of date.
func List() ([]Meta, error) {
	entries, err := os.ReadDir(Dir())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	idx := loadIndex()
	changed := false
	present := map[string]bool{}
	var text map[string]string // loaded only if something needs re-summarizing

	var metas []Meta
	var journals []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".json.log") {
			journals = append(journals, strings.TrimSuffix(e.Name(), ".json.log"))
			continue
		}
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") ||
			e.Name() == indexName || e.Name() == searchName {
			continue
		}
		id := strings.TrimSuffix(e.Name(), ".json")
		present[id] = true

		info, err := e.Info()
		if err != nil {
			continue
		}
		jsize, jmod := journalStamp(id)
		if cached, ok := idx.Entries[id]; ok &&
			cached.Size == info.Size() && cached.Mod == info.ModTime().UnixNano() &&
			cached.JSize == jsize && cached.JMod == jmod {
			metas = append(metas, cached.Meta)
			continue
		}
		meta, ok := summarize(id)
		if !ok {
			continue
		}
		if text == nil {
			text = loadSearchText()
		}
		text[id] = meta.Blob
		meta.Blob = "" // kept in the search file, not the index
		idx.Entries[id] = indexEntry{
			Meta: meta, Size: info.Size(), Mod: info.ModTime().UnixNano(),
			JSize: jsize, JMod: jmod,
		}
		changed = true
		metas = append(metas, meta)
	}

	// A journal without its snapshot is dead weight: nothing can load it, and
	// its session was deleted or renamed out from under it.
	for _, id := range journals {
		if !present[id] {
			os.Remove(journalPath(id))
		}
	}
	for id := range idx.Entries {
		if !present[id] {
			delete(idx.Entries, id)
			if text == nil {
				text = loadSearchText()
			}
			delete(text, id)
			changed = true
		}
	}
	if changed {
		idx.save()
		if text != nil {
			saveSearchText(text)
		}
	}
	sort.Slice(metas, func(i, j int) bool { return metas[i].UpdatedAt.After(metas[j].UpdatedAt) })
	return metas, nil
}
