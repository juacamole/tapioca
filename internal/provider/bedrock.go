package provider

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"tapioca/internal/config"
)

// Bedrock runs Anthropic models on AWS. The request and the event payloads are
// the Messages API, so only the transport differs: SigV4 for auth, and AWS's
// binary event stream instead of SSE.
type Bedrock struct {
	name    string
	region  string
	profile string
	baseURL string     // overridable for PrivateLink or a proxy
	anth    *Anthropic // body building and stream assembly
	client  *http.Client
}

// NewBedrock builds the provider. Credentials are resolved per request so a
// refreshed session token is picked up without a restart.
func NewBedrock(name string, cfg config.ProviderConfig) (*Bedrock, error) {
	region := cfg.Region
	if region == "" {
		region = firstNonEmpty(osGetenv("AWS_REGION"), osGetenv("AWS_DEFAULT_REGION"))
	}
	if region == "" {
		return nil, fmt.Errorf("provider %q: bedrock needs a region (set region, AWS_REGION or AWS_DEFAULT_REGION)", name)
	}
	return &Bedrock{
		name: name, region: region, profile: cfg.Profile,
		baseURL: strings.TrimSuffix(cfg.BaseURL, "/"),
		anth:    &Anthropic{name: name, client: httpClient},
		client:  httpClient,
	}, nil
}

func (b *Bedrock) Name() string { return b.name }

func (b *Bedrock) endpoint(model string) string {
	base := b.baseURL
	if base == "" {
		base = fmt.Sprintf("https://bedrock-runtime.%s.amazonaws.com", b.region)
	}
	return base + "/model/" + url.PathEscape(model) + "/invoke-with-response-stream"
}

// Stream implements Provider.
func (b *Bedrock) Stream(ctx context.Context, req Request, out chan<- Event) (Message, error) {
	defer close(out)

	creds, err := awsCredentials(b.profile)
	if err != nil {
		return Message{}, &APIError{Provider: b.name, Status: 401, Message: err.Error()}
	}

	body := b.anth.buildBody(req)
	// Bedrock takes the model in the URL and versions the payload instead.
	body.Model = ""
	body.Stream = false // the streaming is the endpoint's, not a body flag
	body.AnthropicVersion = "bedrock-2023-05-31"
	payload, err := json.Marshal(body)
	if err != nil {
		return Message{}, err
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", b.endpoint(req.Model), bytes.NewReader(payload))
	if err != nil {
		return Message{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/vnd.amazon.eventstream")
	signV4(httpReq, payload, creds, b.region, "bedrock", time.Now())

	resp, err := b.client.Do(httpReq)
	if err != nil {
		return Message{}, fmt.Errorf("bedrock: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return Message{}, newAPIError(b.name, resp, data)
	}

	// Re-frame the event stream as the SSE the shared parser reads.
	pr, pw := io.Pipe()
	go func() {
		pw.CloseWithError(eventStreamToSSE(resp.Body, pw))
	}()
	return b.anth.streamAnthropicSSE(ctx, req.Model, pr, out)
}

// ListModels implements Provider. Listing needs the bedrock control plane
// rather than the runtime, and the ids are long and stable, so this stays
// empty and the model is configured explicitly.
func (b *Bedrock) ListModels(ctx context.Context) ([]string, error) { return nil, nil }

// AWS event stream framing: a 12-byte prelude (total length, headers length,
// prelude CRC), then headers, payload, and a trailing message CRC. The CRCs are
// not verified — a corrupted frame fails when its JSON is parsed, and this is a
// TLS stream, not a wire protocol needing integrity checks of its own.
func eventStreamToSSE(r io.Reader, w io.Writer) error {
	header := make([]byte, 12)
	for {
		if _, err := io.ReadFull(r, header); err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				return nil
			}
			return err
		}
		total := binary.BigEndian.Uint32(header[0:4])
		headersLen := binary.BigEndian.Uint32(header[4:8])
		if total < 16 || uint64(headersLen)+16 > uint64(total) {
			return fmt.Errorf("bedrock: malformed event frame")
		}
		rest := make([]byte, total-12)
		if _, err := io.ReadFull(r, rest); err != nil {
			return err
		}
		headers := parseEventHeaders(rest[:headersLen])
		payload := rest[headersLen : len(rest)-4] // minus the message CRC

		switch headers[":message-type"] {
		case "exception", "error":
			return fmt.Errorf("bedrock: %s: %s", headers[":exception-type"], strings.TrimSpace(string(payload)))
		}

		// Each chunk wraps the Anthropic event as base64 under "bytes".
		var wrapper struct {
			Bytes []byte `json:"bytes"` // encoding/json base64-decodes []byte
		}
		if json.Unmarshal(payload, &wrapper) != nil || len(wrapper.Bytes) == 0 {
			continue
		}
		if _, err := fmt.Fprintf(w, "data: %s\n\n", wrapper.Bytes); err != nil {
			return err
		}
	}
}

// parseEventHeaders reads the string-valued headers; the others carry nothing
// this client needs.
func parseEventHeaders(b []byte) map[string]string {
	out := map[string]string{}
	for len(b) > 1 {
		nameLen := int(b[0])
		if len(b) < 1+nameLen+1 {
			return out
		}
		name := string(b[1 : 1+nameLen])
		valueType := b[1+nameLen]
		b = b[2+nameLen:]
		if valueType != 7 { // 7 = string; anything else is skipped wholesale
			return out
		}
		if len(b) < 2 {
			return out
		}
		valueLen := int(binary.BigEndian.Uint16(b[:2]))
		if len(b) < 2+valueLen {
			return out
		}
		out[name] = string(b[2 : 2+valueLen])
		b = b[2+valueLen:]
	}
	return out
}
