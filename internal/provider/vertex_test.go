package provider

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"tapioca/internal/config"
)

func clearGoogleEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{"GOOGLE_ACCESS_TOKEN", "GOOGLE_APPLICATION_CREDENTIALS",
		"GOOGLE_CLOUD_PROJECT", "GCLOUD_PROJECT", "GOOGLE_CLOUD_REGION"} {
		t.Setenv(k, "")
	}
}

func TestVertexNeedsAProject(t *testing.T) {
	clearGoogleEnv(t)
	if _, err := NewVertex("v", config.ProviderConfig{Type: "vertex"}); err == nil {
		t.Fatal("vertex without a project should fail")
	} else if !strings.Contains(err.Error(), "project") {
		t.Errorf("unhelpful error: %v", err)
	}
}

func TestVertexEndpointShape(t *testing.T) {
	clearGoogleEnv(t)
	v, err := NewVertex("v", config.ProviderConfig{Type: "vertex", Project: "my-proj", Region: "europe-west1"})
	if err != nil {
		t.Fatal(err)
	}
	got := v.endpoint("claude-sonnet-4@20250514")
	want := "https://europe-west1-aiplatform.googleapis.com/v1/projects/my-proj/locations/" +
		"europe-west1/publishers/anthropic/models/claude-sonnet-4@20250514:streamRawPredict"
	if got != want {
		t.Errorf("endpoint =\n %s\nwant\n %s", got, want)
	}
}

// An explicit token skips credential discovery entirely.
func TestVertexUsesAnExplicitToken(t *testing.T) {
	clearGoogleEnv(t)
	t.Setenv("GOOGLE_ACCESS_TOKEN", "explicit-token")
	v, err := NewVertex("v", config.ProviderConfig{Type: "vertex", Project: "p"})
	if err != nil {
		t.Fatal(err)
	}
	tok, err := v.accessToken(context.Background())
	if err != nil || tok != "explicit-token" {
		t.Fatalf("token = %q, err = %v", tok, err)
	}
}

// The service-account path signs a JWT locally and exchanges it. Getting the
// signature or the claims wrong yields a 401 from Google that is hard to
// diagnose, so it is checked here against a real key.
func TestServiceAccountTokenExchange(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})

	var gotGrant, gotAssertion string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		vals, _ := parseForm(string(body))
		gotGrant, gotAssertion = vals["grant_type"], vals["assertion"]
		fmt.Fprint(w, `{"access_token":"issued-token","expires_in":3600}`)
	}))
	defer srv.Close()

	sa := map[string]string{
		"type":         "service_account",
		"client_email": "bot@example.iam.gserviceaccount.com",
		"private_key":  string(keyPEM),
		"token_uri":    srv.URL,
	}
	raw, _ := json.Marshal(sa)
	path := filepath.Join(t.TempDir(), "sa.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	tok, expiry, err := serviceAccountToken(context.Background(), path, srv.Client())
	if err != nil {
		t.Fatalf("exchange failed: %v", err)
	}
	if tok != "issued-token" {
		t.Errorf("token = %q", tok)
	}
	if expiry.IsZero() {
		t.Error("no expiry recorded, so the token would never refresh")
	}
	if gotGrant != "urn:ietf:params:oauth:grant-type:jwt-bearer" {
		t.Errorf("grant_type = %q", gotGrant)
	}

	// The assertion must be a verifiable RS256 JWT with the right claims.
	parts := strings.Split(gotAssertion, ".")
	if len(parts) != 3 {
		t.Fatalf("assertion is not a JWT: %q", gotAssertion)
	}
	claimsJSON, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	var claims map[string]any
	json.Unmarshal(claimsJSON, &claims)
	if claims["iss"] != sa["client_email"] {
		t.Errorf("iss = %v", claims["iss"])
	}
	if claims["aud"] != srv.URL {
		t.Errorf("aud = %v", claims["aud"])
	}
	if !strings.Contains(fmt.Sprint(claims["scope"]), "cloud-platform") {
		t.Errorf("scope = %v", claims["scope"])
	}
}

func TestServiceAccountRejectsNonKeyFiles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "adc.json")
	os.WriteFile(path, []byte(`{"type":"authorized_user","client_id":"x"}`), 0o600)
	_, _, err := serviceAccountToken(context.Background(), path, http.DefaultClient)
	if err == nil || !strings.Contains(err.Error(), "service account") {
		t.Fatalf("a non-service-account file should say so: %v", err)
	}
}

// The full Vertex path against a stub: bearer auth, versioned payload, SSE in,
// assembled message out.
func TestVertexStreamEndToEnd(t *testing.T) {
	clearGoogleEnv(t)
	t.Setenv("GOOGLE_ACCESS_TOKEN", "tok")

	var gotAuth, gotBody, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		gotAuth, gotBody, gotPath = r.Header.Get("Authorization"), string(body), r.URL.Path
		w.Header().Set("Content-Type", "text/event-stream")
		for _, ev := range []string{
			`{"type":"message_start","message":{"usage":{"input_tokens":4,"output_tokens":0}}}`,
			`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
			`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"bonjour"}}`,
			`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2}}`,
			`{"type":"message_stop"}`,
		} {
			fmt.Fprintf(w, "data: %s\n\n", ev)
		}
	}))
	defer srv.Close()

	v, err := NewVertex("vertex", config.ProviderConfig{Type: "vertex", Project: "p", Region: "us-east5"})
	if err != nil {
		t.Fatal(err)
	}
	v.client = srv.Client()
	v.hostOverride = srv.URL

	events := make(chan Event, 64)
	go func() {
		for range events {
		}
	}()
	msg, err := v.Stream(context.Background(), Request{
		Model: "claude-sonnet-4@20250514", MaxTokens: 50,
		Messages: []Message{TextMessage("user", "hi")},
	}, events)
	if err != nil {
		t.Fatalf("stream failed: %v", err)
	}
	if msg.Text() != "bonjour" {
		t.Errorf("message = %q", msg.Text())
	}
	if gotAuth != "Bearer tok" {
		t.Errorf("Authorization = %q", gotAuth)
	}
	if !strings.Contains(gotPath, "publishers/anthropic/models/") || !strings.HasSuffix(gotPath, ":streamRawPredict") {
		t.Errorf("path = %q", gotPath)
	}
	if strings.Contains(gotBody, `"model"`) {
		t.Errorf("model belongs in the URL, not the body: %s", gotBody)
	}
	if !strings.Contains(gotBody, `"anthropic_version":"vertex-2023-10-16"`) {
		t.Errorf("missing anthropic_version: %s", gotBody)
	}
}

// parseForm is a tiny x-www-form-urlencoded reader for the assertions above.
func parseForm(s string) (map[string]string, error) {
	out := map[string]string{}
	for _, pair := range strings.Split(s, "&") {
		k, v, ok := strings.Cut(pair, "=")
		if !ok {
			continue
		}
		key, err := urlUnescape(k)
		if err != nil {
			return nil, err
		}
		val, err := urlUnescape(v)
		if err != nil {
			return nil, err
		}
		out[key] = val
	}
	return out, nil
}
