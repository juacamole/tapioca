package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"tapioca/internal/config"
)

// maxResourceBytes caps what one resource may add to the conversation, and the
// cap was checked on one of the two branches. An item with no text and a blob
// wrote "[<uri>: binary content omitted]" and then `continue`d straight past
// the check, so a response made of nothing but tiny blob entries put every
// server-chosen URI into the prompt with no bound at all.
func TestABlobOnlyResourceRespectsTheSameBudgetAsText(t *testing.T) {
	const entries = 40_000
	uri := "db://" + strings.Repeat("u", 60)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req rpcMsg
		if json.NewDecoder(r.Body).Decode(&req) != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if req.ID == nil {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		var result string
		switch req.Method {
		case "initialize":
			result = fmt.Sprintf(`{"protocolVersion":%q,"capabilities":{"tools":{},"resources":{}}}`, protocolVersion)
		case "tools/list":
			result = `{"tools":[]}`
		case "resources/list":
			result = `{"resources":[{"uri":"db://big","name":"big"}]}`
		case "resources/read":
			var b strings.Builder
			b.WriteString(`{"contents":[`)
			for i := 0; i < entries; i++ {
				if i > 0 {
					b.WriteByte(',')
				}
				fmt.Fprintf(&b, `{"uri":%q,"blob":"AA"}`, uri)
			}
			b.WriteString(`]}`)
			result = b.String()
		default:
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"error":{"code":-32601,"message":"no"}}`, req.ID)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":%s}`, req.ID, result)
	}))
	defer srv.Close()

	c, err := Start(context.Background(), config.MCPServerConfig{Name: "big", URL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	text, err := c.ReadResource(context.Background(), "db://big")
	if err != nil {
		t.Fatal(err)
	}
	// The control: the server really did send far more than the budget, so a
	// result inside it is the cap working and not the server being modest.
	if entries*len(uri) <= maxResourceBytes {
		t.Skip("the stub response is smaller than the budget; the test proves nothing")
	}
	if len(text) > maxResourceBytes+64 {
		t.Errorf("a blob-only resource added %d bytes to the prompt, budget is %d",
			len(text), maxResourceBytes)
	}
}

// Bounding the placeholder must not stop it being produced: a resource with a
// text part and a blob part still reports both.
func TestABlobIsStillAnnouncedAlongsideText(t *testing.T) {
	c, _ := startContent(t, `{"resources":{}}`)
	text, err := c.ReadResource(context.Background(), "db://users")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "alice") || !strings.Contains(text, "binary content omitted") {
		t.Fatalf("got %q", text)
	}
}
