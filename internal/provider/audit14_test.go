package provider

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"tapioca/internal/config"
)

// crossDomain rewrites a loopback URL to name the host by its other name.
// "localhost" and "127.0.0.1" resolve to the same interface and are different
// domains as far as net/http's redirect rules are concerned, which is what lets
// a test show a genuine cross-domain hop without needing a second address.
func crossDomain(t *testing.T, raw string) string {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	u.Host = "localhost:" + u.Port()
	return u.String()
}

// unhardenedFollows is the control every case below needs: with net/http's
// default policy — which is what this client had — the header really does
// arrive at the other domain. If it does not happen here, this machine cannot
// show the difference and the case that follows proves nothing.
func unhardenedFollows(t *testing.T, endpoint, header, key string) {
	t.Helper()
	req, err := http.NewRequest("GET", endpoint+"/v1/models", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set(header, key)
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		t.Skipf("control: the default client did not follow the redirect here (%v)", err)
	}
	resp.Body.Close()
}

// The model server is one of the two untrusted inputs: a local llama-server, a
// gateway a colleague told the user to point at, an endpoint that was fine
// yesterday. It chooses every byte of its reply, and a reply is allowed to be a
// redirect.
//
// tools/web.go answered this question already, for the same reason and in the
// opposite direction: "The host of a fetch is approved by the user, but
// redirects are not". The provider client — which carries the provider API key
// and the whole conversation — had no CheckRedirect at all, so net/http's
// default applied: follow ten hops to anywhere, and strip only Authorization,
// Cookie, Cookie2 and Www-Authenticate on the way.
//
// That default is what made the gap easy to miss. Every credential this
// codebase puts somewhere other than Authorization travelled intact:
//
//	x-api-key             the Anthropic key            anthropic.go
//	api-key               the Azure key                openai.go
//	<auth_header>         a custom gateway's key       auth.go
//	X-Amz-Security-Token  the STS session token        awssig.go
//
// CheckBaseURL refuses a plaintext remote base URL and isLoopback was written
// carefully so that refusal could not be tricked — but CheckBaseURL only ever
// sees the configured string, and the hop that got around it is the one nobody
// configured.
func TestARedirectDoesNotCarryTheCredentialToAnotherDomain(t *testing.T) {
	const key = "sk-provider-key-do-not-leak"

	var reached bool
	var gotHeaders http.Header
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached, gotHeaders = true, r.Header.Clone()
		w.Write([]byte(`{"data":[]}`))
	}))
	defer collector.Close()
	elsewhere := crossDomain(t, collector.URL)

	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere+r.URL.Path, http.StatusFound)
	}))
	defer endpoint.Close()

	unhardenedFollows(t, endpoint.URL, "X-Api-Key", key)
	if !reached || !strings.Contains(gotHeaders.Get("X-Api-Key"), key) {
		t.Skip("control: the default client did not carry the header across the hop here, so this test shows nothing")
	}
	reached, gotHeaders = false, nil

	p, err := NewCustom("gw", config.ProviderConfig{
		Type: "custom", BaseURL: endpoint.URL, APIKey: key,
		AuthStyle: AuthHeader, AuthHeader: "X-Api-Key",
	})
	if err != nil {
		t.Fatal(err)
	}
	// ListModels rather than a stream: it is the shortest path that carries the
	// credential, and the model picker calls it before a model has even been
	// chosen.
	if _, err := p.ListModels(context.Background()); err == nil {
		t.Error("a redirect off the configured origin was followed without complaint")
	}
	if reached {
		t.Fatalf("a 302 from the model endpoint handed the provider key to another domain: %q",
			gotHeaders.Get("X-Api-Key"))
	}
}

// The body is the other half of what a redirect carries, and for a turn that is
// the system prompt plus every message in the conversation. net/http replays it
// on a 307/308 because the request was built from a *strings.Reader and so has
// a GetBody, and it strips nothing from a body at any time.
func TestARedirectDoesNotReplayTheConversationToAnotherDomain(t *testing.T) {
	const secret = "the-user-private-conversation-text"

	var gotBody string
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer collector.Close()
	elsewhere := crossDomain(t, collector.URL)

	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	defer endpoint.Close()

	// Control: the same 307, replayed by a client with net/http's own policy.
	resp, err := (&http.Client{}).Post(endpoint.URL+"/v1/chat/completions",
		"application/json", strings.NewReader(`{"probe":"`+secret+`"}`))
	if err != nil {
		t.Skipf("control: the default client did not replay the 307 here (%v)", err)
	}
	resp.Body.Close()
	if !strings.Contains(gotBody, secret) {
		t.Skip("control: the default client did not replay the body across the hop here, so this test shows nothing")
	}
	gotBody = ""

	p, err := NewCustom("gw", config.ProviderConfig{
		Type: "custom", BaseURL: endpoint.URL, APIKey: "k", AuthStyle: AuthBearer,
	})
	if err != nil {
		t.Fatal(err)
	}
	req := Request{
		Model:    "m",
		System:   "you are a helpful assistant",
		Messages: []Message{TextMessage("user", secret)},
	}
	out := make(chan Event, 1024)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range out {
		}
	}()
	_, streamErr := p.Stream(context.Background(), req, out)
	<-done
	if streamErr == nil {
		t.Error("a 307 off the configured origin was followed without complaint")
	}
	if strings.Contains(gotBody, secret) {
		t.Fatal("a 307 from the model endpoint replayed the conversation to another domain")
	}
}

// The other half, with equal weight: an endpoint that redirects within its own
// origin is an ordinary thing — a base URL without the trailing slash, a
// version path that moved — and it has to keep working, credential and all.
func TestASameOriginRedirectIsStillFollowed(t *testing.T) {
	const key = "sk-provider-key"

	var served bool
	var gotKey string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/moved") {
			http.Redirect(w, r, "/moved"+r.URL.Path, http.StatusFound)
			return
		}
		served, gotKey = true, r.Header.Get("X-Api-Key")
		w.Write([]byte(`{"data":[{"id":"m1"}]}`))
	}))
	defer srv.Close()

	p, err := NewCustom("gw", config.ProviderConfig{
		Type: "custom", BaseURL: srv.URL, APIKey: key,
		AuthStyle: AuthHeader, AuthHeader: "X-Api-Key",
	})
	if err != nil {
		t.Fatal(err)
	}
	models, err := p.ListModels(context.Background())
	if err != nil {
		t.Fatalf("a redirect within the configured origin was refused: %v", err)
	}
	if !served || gotKey != key {
		t.Fatalf("same-origin redirect: served=%v key=%q", served, gotKey)
	}
	if len(models) != 1 || models[0] != "m1" {
		t.Fatalf("models = %q", models)
	}
}

// RetryDelay is exported and is called before its caller has checked anything
// about the attempt number. A shift by a negative amount is a runtime panic,
// not an error, so the function has to be total over every int a caller can
// reach — see TestAFailingFallbackDoesNotBringTheProcessDown in the agent
// package for the one that did.
func TestRetryDelayIsTotal(t *testing.T) {
	for _, attempt := range []int{-1 << 31, -2, -1, 0, 1, 2, 5, 62, 63, 64, 1 << 30} {
		d, ok := RetryDelay(attempt, nil)
		if !ok {
			t.Errorf("attempt %d: not ok", attempt)
		}
		if d <= 0 || d > retryMaxDelay*2 {
			t.Errorf("attempt %d: delay %v is outside the range the loop can wait", attempt, d)
		}
	}
}

// maxResponseBytes bounds what a stream may accumulate, and the block/tool-call
// count bounds how many pieces it may arrive in. Between them sat the id: it is
// retained for the life of the stream exactly like the name and the arguments,
// and it was the one field left out of the running total. A server could hold
// maxStreamBlocks ids of up to the line limit each — tens of gigabytes — with
// the byte cap reading as barely touched.
//
// The sizes here are a small multiple of the real cap so the test is quick; the
// property is that the stream stops, not how fast.
func TestStreamedIdentifiersCountAgainstTheResponseBudget(t *testing.T) {
	const idSize = 1 << 20
	bigID := strings.Repeat("i", idSize)
	// Enough ids to pass maxResponseBytes, and comfortably fewer than
	// maxStreamBlocks so that it is the byte cap under test and not the count.
	blocks := maxResponseBytes/idSize + 8
	if blocks >= maxStreamBlocks {
		t.Skipf("the block cap (%d) would fire before the byte cap here", maxStreamBlocks)
	}

	for _, tc := range []struct {
		name  string
		build func(w http.ResponseWriter)
		newP  func(url string) Provider
	}{
		{
			name: "anthropic content_block_start ids",
			build: func(w http.ResponseWriter) {
				fmt.Fprint(w, "data: {\"type\":\"message_start\",\"message\":{\"usage\":{}}}\n\n")
				for i := 0; i < blocks; i++ {
					fmt.Fprintf(w, "data: {\"type\":\"content_block_start\",\"index\":%d,"+
						"\"content_block\":{\"type\":\"tool_use\",\"id\":%q,\"name\":\"t\"}}\n\n", i, bigID)
				}
				fmt.Fprint(w, "data: {\"type\":\"message_stop\"}\n\n")
			},
			newP: func(url string) Provider {
				p, err := NewAnthropic("a", config.ProviderConfig{Type: "anthropic", BaseURL: url, APIKey: "k"})
				if err != nil {
					t.Fatal(err)
				}
				return p
			},
		},
		{
			name: "openai tool_call ids",
			build: func(w http.ResponseWriter) {
				for i := 0; i < blocks; i++ {
					fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"tool_calls\":"+
						"[{\"index\":%d,\"id\":%q,\"function\":{\"name\":\"t\",\"arguments\":\"{}\"}}]}}]}\n\n", i, bigID)
				}
				fmt.Fprint(w, "data: [DONE]\n\n")
			},
			newP: func(url string) Provider {
				return NewOpenAI("o", config.ProviderConfig{Type: "openai", BaseURL: url})
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
				tc.build(w)
			}))
			defer srv.Close()

			out := make(chan Event, 1<<16)
			done := make(chan struct{})
			go func() {
				defer close(done)
				for range out {
				}
			}()
			_, err := tc.newP(srv.URL).Stream(context.Background(),
				Request{Model: "m", Messages: []Message{TextMessage("user", "hi")}}, out)
			<-done

			if err == nil {
				t.Fatalf("%d identifiers of %d bytes were accepted whole, past the %d byte cap",
					blocks, idSize, maxResponseBytes)
			}
			if !strings.Contains(err.Error(), "exceeded") {
				t.Fatalf("the stream stopped for the wrong reason: %v", err)
			}
		})
	}
}
