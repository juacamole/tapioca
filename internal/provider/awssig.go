package provider

import (
	"bufio"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Signature Version 4, implemented here rather than by pulling in the AWS SDK:
// this is the only AWS call Tapioca makes, and the SDK would dwarf the rest of
// the dependencies. The algorithm is fixed and covered by AWS's own published
// test vectors, which the tests use.

type awsCreds struct {
	AccessKey, SecretKey, SessionToken string
}

func (c awsCreds) ok() bool { return c.AccessKey != "" && c.SecretKey != "" }

// awsCredentials resolves credentials the way the CLI does, minus the parts
// that need a network call (no SSO, no instance metadata): environment first,
// then the shared credentials file for the selected profile.
func awsCredentials(profile string) (awsCreds, error) {
	c := awsCreds{
		AccessKey:    os.Getenv("AWS_ACCESS_KEY_ID"),
		SecretKey:    os.Getenv("AWS_SECRET_ACCESS_KEY"),
		SessionToken: os.Getenv("AWS_SESSION_TOKEN"),
	}
	if c.ok() {
		return c, nil
	}
	if profile == "" {
		profile = os.Getenv("AWS_PROFILE")
	}
	if profile == "" {
		profile = "default"
	}
	path := os.Getenv("AWS_SHARED_CREDENTIALS_FILE")
	if path == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return awsCreds{}, err
		}
		path = filepath.Join(home, ".aws", "credentials")
	}
	c, err := credsFromFile(path, profile)
	if err != nil {
		return awsCreds{}, err
	}
	if !c.ok() {
		return awsCreds{}, fmt.Errorf("no AWS credentials: set AWS_ACCESS_KEY_ID/AWS_SECRET_ACCESS_KEY or add profile %q to %s", profile, path)
	}
	return c, nil
}

// credsFromFile reads one profile out of the INI-style shared credentials file.
func credsFromFile(path, profile string) (awsCreds, error) {
	f, err := os.Open(path)
	if err != nil {
		return awsCreds{}, fmt.Errorf("reading AWS credentials: %w", err)
	}
	defer f.Close()

	var c awsCreds
	inProfile := false
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			inProfile = strings.TrimSpace(strings.Trim(line, "[]")) == profile
			continue
		}
		if !inProfile {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		switch strings.TrimSpace(strings.ToLower(key)) {
		case "aws_access_key_id":
			c.AccessKey = strings.TrimSpace(value)
		case "aws_secret_access_key":
			c.SecretKey = strings.TrimSpace(value)
		case "aws_session_token":
			c.SessionToken = strings.TrimSpace(value)
		}
	}
	return c, sc.Err()
}

func hmacSHA256(key []byte, data string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(data))
	return h.Sum(nil)
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// signV4 adds the SigV4 Authorization header (and the headers it covers) to a
// request whose body is payload.
func signV4(req *http.Request, payload []byte, c awsCreds, region, service string, now time.Time) {
	// Re-signing must not fold a previous signature into the new one.
	req.Header.Del("Authorization")
	now = now.UTC()
	amzDate := now.Format("20060102T150405Z")
	dateOnly := now.Format("20060102")

	req.Header.Set("X-Amz-Date", amzDate)
	if c.SessionToken != "" {
		req.Header.Set("X-Amz-Security-Token", c.SessionToken)
	}
	if req.Host != "" {
		req.Header.Set("Host", req.Host)
	} else {
		req.Header.Set("Host", req.URL.Host)
	}
	payloadHash := sha256Hex(payload)

	// Every header we send is signed; sorted, lower-cased, values trimmed.
	var names []string
	for name := range req.Header {
		names = append(names, strings.ToLower(name))
	}
	sort.Strings(names)
	var canonHeaders strings.Builder
	for _, n := range names {
		canonHeaders.WriteString(n)
		canonHeaders.WriteByte(':')
		canonHeaders.WriteString(strings.TrimSpace(req.Header.Get(n)))
		canonHeaders.WriteByte('\n')
	}
	signedHeaders := strings.Join(names, ";")

	canonicalURI := req.URL.EscapedPath()
	if canonicalURI == "" {
		canonicalURI = "/"
	}
	canonicalQuery := req.URL.Query().Encode()

	canonicalRequest := strings.Join([]string{
		req.Method, canonicalURI, canonicalQuery,
		canonHeaders.String(), signedHeaders, payloadHash,
	}, "\n")

	scope := strings.Join([]string{dateOnly, region, service, "aws4_request"}, "/")
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256", amzDate, scope, sha256Hex([]byte(canonicalRequest)),
	}, "\n")

	key := hmacSHA256([]byte("AWS4"+c.SecretKey), dateOnly)
	key = hmacSHA256(key, region)
	key = hmacSHA256(key, service)
	key = hmacSHA256(key, "aws4_request")
	signature := hex.EncodeToString(hmacSHA256(key, stringToSign))

	req.Header.Set("Authorization", fmt.Sprintf(
		"AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		c.AccessKey, scope, signedHeaders, signature))
}
