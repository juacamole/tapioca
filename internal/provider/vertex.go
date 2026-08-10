package provider

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"tapioca/internal/config"
)

// Vertex runs Anthropic models on Google Cloud. The payload is the Messages
// API and the response is ordinary SSE, so only authentication differs.
type Vertex struct {
	name         string
	project      string
	region       string
	anth         *Anthropic
	client       *http.Client
	credFile     string
	hostOverride string // private service endpoint or a proxy

	mu      sync.Mutex
	token   string
	expires time.Time
}

func NewVertex(name string, cfg config.ProviderConfig) (*Vertex, error) {
	project := firstNonEmpty(cfg.Project, osGetenv("GOOGLE_CLOUD_PROJECT"), osGetenv("GCLOUD_PROJECT"))
	if project == "" {
		return nil, fmt.Errorf("provider %q: vertex needs a project (set project or GOOGLE_CLOUD_PROJECT)", name)
	}
	region := firstNonEmpty(cfg.Region, osGetenv("GOOGLE_CLOUD_REGION"), "us-east5")
	return &Vertex{
		name: name, project: project, region: region,
		anth:         &Anthropic{name: name, client: httpClient},
		client:       httpClient,
		credFile:     firstNonEmpty(cfg.CredentialsFile, osGetenv("GOOGLE_APPLICATION_CREDENTIALS")),
		hostOverride: strings.TrimSuffix(cfg.BaseURL, "/"),
	}, nil
}

func (v *Vertex) Name() string { return v.name }

func (v *Vertex) endpoint(model string) string {
	host := v.hostOverride
	if host == "" {
		host = fmt.Sprintf("https://%s-aiplatform.googleapis.com", v.region)
	}
	return fmt.Sprintf("%s/v1/projects/%s/locations/%s/publishers/anthropic/models/%s:streamRawPredict",
		host, url.PathEscape(v.project), url.PathEscape(v.region), url.PathEscape(model))
}

// Stream implements Provider.
func (v *Vertex) Stream(ctx context.Context, req Request, out chan<- Event) (Message, error) {
	defer close(out)

	token, err := v.accessToken(ctx)
	if err != nil {
		return Message{}, &APIError{Provider: v.name, Status: 401, Message: err.Error()}
	}

	body := v.anth.buildBody(req)
	// Vertex takes the model in the URL and versions the payload instead.
	body.Model = ""
	body.AnthropicVersion = "vertex-2023-10-16"
	payload, err := json.Marshal(body)
	if err != nil {
		return Message{}, err
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", v.endpoint(req.Model), bytes.NewReader(payload))
	if err != nil {
		return Message{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+token)

	resp, err := v.client.Do(httpReq)
	if err != nil {
		return Message{}, fmt.Errorf("vertex: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return Message{}, newAPIError(v.name, resp, data)
	}
	return v.anth.streamAnthropicSSE(ctx, req.Model, resp.Body, out)
}

// ListModels implements Provider; Vertex model ids are configured explicitly.
func (v *Vertex) ListModels(ctx context.Context) ([]string, error) { return nil, nil }

// accessToken returns a cached Google OAuth token, refreshing it when close to
// expiry. Service-account JSON is signed locally; otherwise gcloud is asked,
// which covers the usual developer login.
func (v *Vertex) accessToken(ctx context.Context) (string, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.token != "" && time.Until(v.expires) > time.Minute {
		return v.token, nil
	}
	if t := osGetenv("GOOGLE_ACCESS_TOKEN"); t != "" {
		v.token, v.expires = t, time.Now().Add(time.Hour)
		return v.token, nil
	}

	var token string
	var expiry time.Time
	var err error
	if v.credFile != "" {
		token, expiry, err = serviceAccountToken(ctx, v.credFile, v.client)
	} else {
		token, expiry, err = gcloudToken(ctx)
	}
	if err != nil {
		return "", err
	}
	v.token, v.expires = token, expiry
	return token, nil
}

// gcloudToken shells out to the CLI a developer has already logged in with.
func gcloudToken(ctx context.Context) (string, time.Time, error) {
	bin, err := exec.LookPath("gcloud")
	if err != nil {
		return "", time.Time{}, fmt.Errorf("no Google credentials: set GOOGLE_APPLICATION_CREDENTIALS, GOOGLE_ACCESS_TOKEN, or install gcloud and run `gcloud auth application-default login`")
	}
	cmd := exec.CommandContext(ctx, bin, "auth", "print-access-token")
	outBytes, err := cmd.Output()
	if err != nil {
		return "", time.Time{}, fmt.Errorf("gcloud auth print-access-token failed: %w", err)
	}
	token := strings.TrimSpace(string(outBytes))
	if token == "" {
		return "", time.Time{}, fmt.Errorf("gcloud returned an empty token")
	}
	// gcloud does not report expiry here; an hour is its usual lifetime.
	return token, time.Now().Add(55 * time.Minute), nil
}

// serviceAccountToken performs the JWT bearer grant: sign a claim set with the
// account's private key and exchange it for an access token.
func serviceAccountToken(ctx context.Context, path string, client *http.Client) (string, time.Time, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("reading %s: %w", path, err)
	}
	var sa struct {
		Type        string `json:"type"`
		ClientEmail string `json:"client_email"`
		PrivateKey  string `json:"private_key"`
		TokenURI    string `json:"token_uri"`
	}
	if err := json.Unmarshal(raw, &sa); err != nil {
		return "", time.Time{}, fmt.Errorf("parsing %s: %w", path, err)
	}
	if sa.ClientEmail == "" || sa.PrivateKey == "" {
		return "", time.Time{}, fmt.Errorf("%s is not a service account key (only those can be signed locally; use gcloud otherwise)", path)
	}
	if sa.TokenURI == "" {
		sa.TokenURI = "https://oauth2.googleapis.com/token"
	}

	key, err := parsePrivateKey(sa.PrivateKey)
	if err != nil {
		return "", time.Time{}, err
	}
	now := time.Now()
	claims := map[string]any{
		"iss":   sa.ClientEmail,
		"scope": "https://www.googleapis.com/auth/cloud-platform",
		"aud":   sa.TokenURI,
		"iat":   now.Unix(),
		"exp":   now.Add(time.Hour).Unix(),
	}
	assertion, err := signJWT(claims, key)
	if err != nil {
		return "", time.Time{}, err
	}

	form := url.Values{
		"grant_type": {"urn:ietf:params:oauth:grant-type:jwt-bearer"},
		"assertion":  {assertion},
	}
	req, err := http.NewRequestWithContext(ctx, "POST", sa.TokenURI, strings.NewReader(form.Encode()))
	if err != nil {
		return "", time.Time{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := client.Do(req)
	if err != nil {
		return "", time.Time{}, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return "", time.Time{}, fmt.Errorf("token exchange failed (%s): %s", resp.Status, strings.TrimSpace(string(data)))
	}
	var tok struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.Unmarshal(data, &tok); err != nil || tok.AccessToken == "" {
		return "", time.Time{}, fmt.Errorf("token exchange returned no access token")
	}
	ttl := time.Duration(tok.ExpiresIn) * time.Second
	if ttl <= 0 {
		ttl = time.Hour
	}
	return tok.AccessToken, time.Now().Add(ttl), nil
}

func parsePrivateKey(pemText string) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(pemText))
	if block == nil {
		return nil, fmt.Errorf("private_key is not valid PEM")
	}
	if key, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		if rsaKey, ok := key.(*rsa.PrivateKey); ok {
			return rsaKey, nil
		}
		return nil, fmt.Errorf("private_key is not RSA")
	}
	return x509.ParsePKCS1PrivateKey(block.Bytes)
}

func signJWT(claims map[string]any, key *rsa.PrivateKey) (string, error) {
	header, err := json.Marshal(map[string]string{"alg": "RS256", "typ": "JWT"})
	if err != nil {
		return "", err
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	enc := base64.RawURLEncoding
	signing := enc.EncodeToString(header) + "." + enc.EncodeToString(payload)
	sum := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if err != nil {
		return "", err
	}
	return signing + "." + enc.EncodeToString(sig), nil
}

func osGetenv(k string) string { return os.Getenv(k) }

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func urlUnescape(s string) (string, error) { return url.QueryUnescape(s) }
