package provider

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"

	"tapioca/internal/config"
)

// A provider that never stops sending deltas — broken, or an endpoint someone
// else controls — grew the heap without bound: the scanner's cap bounds one
// line, not the response assembled from them, and the turn has no deadline.
func TestEndlessStreamIsCutOff(t *testing.T) {
	stop := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		chunk := strings.Repeat("A", 32<<10)
		fl, _ := w.(http.Flusher)
		for {
			select {
			case <-stop:
				return
			default:
			}
			_, err := fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":%q}}]}\n\n", chunk)
			if err != nil {
				return
			}
			if fl != nil {
				fl.Flush()
			}
		}
	}))
	defer srv.Close()
	defer close(stop)

	p := NewOpenAI("test", config.ProviderConfig{Type: "openai", BaseURL: srv.URL})
	out := make(chan Event, 1024)
	go func() {
		for range out {
		}
	}()

	var m0, m1 runtime.MemStats
	runtime.ReadMemStats(&m0)
	done := make(chan error, 1)
	go func() {
		_, err := p.Stream(context.Background(), Request{Model: "m", Messages: []Message{
			TextMessage("user", "hi"),
		}}, out)
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("an endless stream ended without an error")
		}
		if !strings.Contains(err.Error(), "exceeded") {
			t.Fatalf("unexpected error: %v", err)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("the stream was never cut off")
	}
	runtime.ReadMemStats(&m1)
	if grew := int64(m1.HeapInuse) - int64(m0.HeapInuse); grew > 512<<20 {
		t.Fatalf("heap grew %d MB before the cut-off", grew>>20)
	}
}
