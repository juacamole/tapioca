package provider

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// token_uri decides where the service-account grant is POSTed — the same
// decision base_url makes, and it was made by the key file with nothing looking
// at it. A repository that commits a config and a key file beside it names the
// address, and the exchange runs on its own the first time a model answers.
func TestAKeyFileCannotAimTheTokenExchangeAnywhere(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	keyPEM := string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))

	write := func(tokenURI string) string {
		raw, _ := json.Marshal(map[string]string{
			"type":         "service_account",
			"client_email": "bot@example.iam.gserviceaccount.com",
			"private_key":  keyPEM,
			"token_uri":    tokenURI,
		})
		path := filepath.Join(t.TempDir(), "sa.json")
		if err := os.WriteFile(path, raw, 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}

	for _, uri := range []string{
		"http://169.254.169.254/latest/meta-data/", // cloud metadata, in the clear
		"http://attacker.example/token",            // the grant across the network unencrypted
		"file:///etc/passwd",                       // not a scheme that fetches anything
		"//attacker.example/token",                 // no scheme at all
	} {
		_, _, err := serviceAccountToken(context.Background(), write(uri), http.DefaultClient)
		if err == nil {
			t.Errorf("token_uri %q was accepted", uri)
			continue
		}
		if !strings.Contains(err.Error(), "token_uri") {
			t.Errorf("token_uri %q was refused for the wrong reason: %v", uri, err)
		}
	}

	// The ordinary spellings still work: Google's own endpoint, the default
	// when the file names none, and a local emulator on loopback, which is the
	// same allowance base_url makes.
	for _, uri := range []string{
		"https://oauth2.googleapis.com/token",
		"",
		"http://127.0.0.1:8087/token",
		"http://localhost:8087/token",
	} {
		_, _, err := serviceAccountToken(context.Background(), write(uri), http.DefaultClient)
		if err != nil && strings.Contains(err.Error(), "token_uri") {
			t.Errorf("token_uri %q was refused: %v", uri, err)
		}
	}
}
