// Package stats accumulates per-agent usage data for the dashboards.
// All mutation happens on the UI goroutine, so no locking is needed.
package stats

import "time"

const keepLast = 100

// RequestStat records one completed model request.
type RequestStat struct {
	Time     time.Time     `json:"time"`
	Provider string        `json:"provider,omitempty"`
	Model    string        `json:"model"`
	In       int           `json:"in"`
	Out      int           `json:"out"`
	CacheR   int           `json:"cache_r,omitempty"`
	CacheW   int           `json:"cache_w,omitempty"`
	Dur      time.Duration `json:"dur"`
}

// ToolCallStat records one tool invocation.
type ToolCallStat struct {
	Time    time.Time     `json:"time"`
	Name    string        `json:"name"`
	Args    string        `json:"args"`
	Result  string        `json:"result"`
	IsError bool          `json:"is_error"`
	Dur     time.Duration `json:"dur"`
	Running bool          `json:"running"`
}

// Stats aggregates everything the dashboards show for one agent.
type Stats struct {
	InputTokens      int            `json:"input_tokens"`
	OutputTokens     int            `json:"output_tokens"`
	CacheReadTokens  int            `json:"cache_read_tokens,omitempty"`
	CacheWriteTokens int            `json:"cache_write_tokens,omitempty"`
	Turns            int            `json:"turns"`
	Requests         []RequestStat  `json:"requests"`
	ToolCalls        []ToolCallStat `json:"tool_calls"`
}

// AddRequest records a completed request and updates totals.
func (s *Stats) AddRequest(providerName, model string, in, out, cacheR, cacheW int, dur time.Duration) {
	s.InputTokens += in
	s.OutputTokens += out
	s.CacheReadTokens += cacheR
	s.CacheWriteTokens += cacheW
	s.Requests = append(s.Requests, RequestStat{
		Time: time.Now(), Provider: providerName, Model: model, In: in, Out: out,
		CacheR: cacheR, CacheW: cacheW, Dur: dur,
	})
	if len(s.Requests) > keepLast {
		s.Requests = s.Requests[len(s.Requests)-keepLast:]
	}
}

// StartToolCall appends a running tool call and returns its index.
func (s *Stats) StartToolCall(name, args string) int {
	s.ToolCalls = append(s.ToolCalls, ToolCallStat{Time: time.Now(), Name: name, Args: args, Running: true})
	if len(s.ToolCalls) > keepLast {
		s.ToolCalls = s.ToolCalls[len(s.ToolCalls)-keepLast:]
	}
	return len(s.ToolCalls) - 1
}

// FinishToolCall marks the most recent running call with the given name done.
func (s *Stats) FinishToolCall(name, result string, isError bool, dur time.Duration) {
	for i := len(s.ToolCalls) - 1; i >= 0; i-- {
		tc := &s.ToolCalls[i]
		if tc.Running && tc.Name == name {
			tc.Running = false
			tc.Result = result
			tc.IsError = isError
			tc.Dur = dur
			return
		}
	}
}

// LastRequest returns the most recent request stat, or nil.
func (s *Stats) LastRequest() *RequestStat {
	if len(s.Requests) == 0 {
		return nil
	}
	return &s.Requests[len(s.Requests)-1]
}
