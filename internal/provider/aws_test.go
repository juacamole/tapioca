package provider

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"tapioca/internal/config"
)

// AWS publishes signing test vectors; this is the get-vanilla case, which
// pins the canonical request, the string to sign and the key derivation. A
// hand-rolled signer is only trustworthy against a known answer.
func TestSigV4MatchesAWSTestVector(t *testing.T) {
	req, err := http.NewRequest("GET", "https://example.amazonaws.com/", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "example.amazonaws.com"
	when := time.Date(2015, 8, 30, 12, 36, 0, 0, time.UTC)
	creds := awsCreds{AccessKey: "AKIDEXAMPLE", SecretKey: "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY"}

	signV4(req, nil, creds, "us-east-1", "service", when)

	auth := req.Header.Get("Authorization")
	const want = "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20150830/us-east-1/service/aws4_request, " +
		"SignedHeaders=host;x-amz-date, " +
		"Signature=5fa00fa31553b73ebf1942676e86291e8372ff2a2260956d9b8aae1d763fbf31"
	if auth != want {
		t.Fatalf("signature does not match the AWS vector:\n got %s\nwant %s", auth, want)
	}
	// Deriving the same signature twice for the same inputs is the property
	// that matters for a signer.
	first := auth
	req2, _ := http.NewRequest("GET", "https://example.amazonaws.com/", nil)
	req2.Host = "example.amazonaws.com"
	signV4(req2, nil, creds, "us-east-1", "service", when)
	if req2.Header.Get("Authorization") != first {
		t.Error("signing is not deterministic for identical inputs")
	}
}

func TestSigV4CoversSessionTokenAndBody(t *testing.T) {
	req, _ := http.NewRequest("POST", "https://bedrock-runtime.us-east-1.amazonaws.com/model/x/invoke", nil)
	creds := awsCreds{AccessKey: "AK", SecretKey: "SK", SessionToken: "TOKEN"}
	signV4(req, []byte(`{"a":1}`), creds, "us-east-1", "bedrock", time.Now())

	if req.Header.Get("X-Amz-Security-Token") != "TOKEN" {
		t.Error("session token not sent")
	}
	if !strings.Contains(req.Header.Get("Authorization"), "x-amz-security-token") {
		t.Error("session token not covered by the signature")
	}
	// A different body must produce a different signature.
	before := req.Header.Get("Authorization")
	req2, _ := http.NewRequest("POST", "https://bedrock-runtime.us-east-1.amazonaws.com/model/x/invoke", nil)
	signV4(req2, []byte(`{"a":2}`), creds, "us-east-1", "bedrock", time.Now())
	if req2.Header.Get("Authorization") == before {
		t.Error("the signature does not depend on the body")
	}
}

func TestCredentialsFromSharedFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "credentials")
	os.WriteFile(path, []byte(`
# a comment
[default]
aws_access_key_id = DEFAULTKEY
aws_secret_access_key = DEFAULTSECRET

[work]
aws_access_key_id = WORKKEY
aws_secret_access_key = WORKSECRET
aws_session_token = WORKTOKEN
`), 0o600)
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", path)
	t.Setenv("AWS_ACCESS_KEY_ID", "")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "")

	c, err := awsCredentials("")
	if err != nil || c.AccessKey != "DEFAULTKEY" {
		t.Fatalf("default profile: %+v %v", c, err)
	}
	c, err = awsCredentials("work")
	if err != nil || c.AccessKey != "WORKKEY" || c.SessionToken != "WORKTOKEN" {
		t.Fatalf("named profile: %+v %v", c, err)
	}
	if _, err := awsCredentials("missing"); err == nil {
		t.Error("an absent profile should be an error, not empty credentials")
	}
}

func TestEnvironmentCredentialsWin(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "ENVKEY")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "ENVSECRET")
	c, err := awsCredentials("")
	if err != nil || c.AccessKey != "ENVKEY" {
		t.Fatalf("got %+v %v", c, err)
	}
}

// frame builds one AWS event-stream message the way Bedrock does.
func frame(t *testing.T, messageType string, payload []byte) []byte {
	t.Helper()
	var headers bytes.Buffer
	writeHeader := func(name, value string) {
		headers.WriteByte(byte(len(name)))
		headers.WriteString(name)
		headers.WriteByte(7) // string
		binary.Write(&headers, binary.BigEndian, uint16(len(value)))
		headers.WriteString(value)
	}
	writeHeader(":message-type", messageType)
	writeHeader(":event-type", "chunk")

	total := 12 + headers.Len() + len(payload) + 4
	var out bytes.Buffer
	binary.Write(&out, binary.BigEndian, uint32(total))
	binary.Write(&out, binary.BigEndian, uint32(headers.Len()))
	binary.Write(&out, binary.BigEndian, uint32(0)) // prelude CRC, unchecked
	out.Write(headers.Bytes())
	out.Write(payload)
	binary.Write(&out, binary.BigEndian, uint32(0)) // message CRC, unchecked
	return out.Bytes()
}

// chunk wraps an Anthropic event the way Bedrock does: base64 under "bytes".
func chunk(t *testing.T, event string) []byte {
	t.Helper()
	body, err := json.Marshal(map[string]any{"bytes": []byte(event)})
	if err != nil {
		t.Fatal(err)
	}
	return frame(t, "event", body)
}

func TestEventStreamDecodesToSSE(t *testing.T) {
	var in bytes.Buffer
	in.Write(chunk(t, `{"type":"message_start"}`))
	in.Write(chunk(t, `{"type":"content_block_delta","delta":{"type":"text_delta","text":"hi"}}`))

	var out bytes.Buffer
	if err := eventStreamToSSE(&in, &out); err != nil {
		t.Fatalf("decode failed: %v", err)
	}
	got := out.String()
	for _, want := range []string{`data: {"type":"message_start"}`, `"text":"hi"`} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if strings.Count(got, "data: ") != 2 {
		t.Errorf("expected two SSE events:\n%s", got)
	}
}

func TestEventStreamSurfacesExceptions(t *testing.T) {
	var in bytes.Buffer
	in.Write(frame(t, "exception", []byte(`{"message":"throttled"}`)))
	err := eventStreamToSSE(&in, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "throttled") {
		t.Fatalf("an exception frame should surface: %v", err)
	}
}

func TestEventStreamRejectsMalformedFrames(t *testing.T) {
	bad := []byte{0, 0, 0, 4, 0, 0, 0, 0, 0, 0, 0, 0} // total length below the prelude
	if err := eventStreamToSSE(bytes.NewReader(bad), io.Discard); err == nil {
		t.Error("a malformed frame should be an error, not silence")
	}
}

// The whole Bedrock path: signed request, event stream in, assembled message
// out — against a stub standing in for the service.
func TestBedrockStreamEndToEnd(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "AK")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "SK")

	var gotPath, gotAuth, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		gotPath, gotAuth, gotBody = r.URL.Path, r.Header.Get("Authorization"), string(body)
		w.Header().Set("Content-Type", "application/vnd.amazon.eventstream")
		w.Write(chunk(t, `{"type":"message_start","message":{"usage":{"input_tokens":10,"output_tokens":0}}}`))
		w.Write(chunk(t, `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`))
		w.Write(chunk(t, `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hello"}}`))
		w.Write(chunk(t, `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":3}}`))
		w.Write(chunk(t, `{"type":"message_stop"}`))
	}))
	defer srv.Close()

	b, err := NewBedrock("bedrock", config.ProviderConfig{Type: "bedrock", Region: "us-east-1"})
	if err != nil {
		t.Fatal(err)
	}
	b.client = srv.Client()
	// Point the runtime endpoint at the stub.
	b.baseURL = srv.URL

	events := make(chan Event, 64)
	var text strings.Builder
	done := make(chan struct{})
	go func() {
		for ev := range events {
			if ev.Type == EventTextDelta {
				text.WriteString(ev.Text)
			}
		}
		close(done)
	}()
	msg, err := b.Stream(context.Background(), Request{
		Model: "anthropic.claude-3-5-sonnet-20240620-v1:0", MaxTokens: 100,
		Messages: []Message{TextMessage("user", "hi")},
	}, events)
	<-done
	if err != nil {
		t.Fatalf("stream failed: %v", err)
	}
	if msg.Text() != "hello" || text.String() != "hello" {
		t.Errorf("message=%q streamed=%q", msg.Text(), text.String())
	}
	if !strings.Contains(gotPath, "/model/") || !strings.Contains(gotPath, "invoke-with-response-stream") {
		t.Errorf("wrong endpoint path: %s", gotPath)
	}
	if !strings.HasPrefix(gotAuth, "AWS4-HMAC-SHA256 Credential=AK/") {
		t.Errorf("request was not signed: %q", gotAuth)
	}
	// The model goes in the URL and the payload is versioned instead.
	if strings.Contains(gotBody, `"model"`) {
		t.Errorf("model should not be in the body: %s", gotBody)
	}
	if !strings.Contains(gotBody, `"anthropic_version":"bedrock-2023-05-31"`) {
		t.Errorf("missing anthropic_version: %s", gotBody)
	}
	if msg.Usage == nil || msg.Usage.InputTokens != 10 {
		t.Errorf("usage not carried through: %+v", msg.Usage)
	}
}

func TestBedrockNeedsARegion(t *testing.T) {
	t.Setenv("AWS_REGION", "")
	t.Setenv("AWS_DEFAULT_REGION", "")
	if _, err := NewBedrock("b", config.ProviderConfig{Type: "bedrock"}); err == nil {
		t.Fatal("bedrock without a region should fail")
	} else if !strings.Contains(err.Error(), "region") {
		t.Errorf("unhelpful error: %v", err)
	}
}
