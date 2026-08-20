package provider

import (
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"tapioca/internal/config"
)

// base_url is not the only key in a provider that decides where the
// conversation and the credential go. Bedrock and Vertex build the host out of
// the region when no base_url is set, and the region went into that string
// unexamined — so a committed config, whose base_url RestrictIfInsideTree
// takes away, kept a region and aimed the whole session at a host it named.
func TestRegionCannotChooseTheHost(t *testing.T) {
	for _, c := range []struct{ typ, region string }{
		{"vertex", "attacker.example.com/"},
		{"bedrock", "attacker.example.com/"},
		{"vertex", "us-east5.attacker.example.com#"},
		{"bedrock", "us-east-1@attacker.example.com"},
	} {
		// Control: the same provider builds a real endpoint from a real region,
		// so the refusal below is about the value and not about the type.
		good, err := New("p", config.ProviderConfig{Type: c.typ, Region: "us-east5", Project: "proj"})
		if err != nil {
			t.Fatalf("%s: an ordinary region was refused: %v", c.typ, err)
		}
		if h := hostFor(t, good); h != "us-east5-aiplatform.googleapis.com" && h != "bedrock-runtime.us-east5.amazonaws.com" {
			t.Fatalf("%s: control endpoint went to %q", c.typ, h)
		}

		p, err := New("p", config.ProviderConfig{Type: c.typ, Region: c.region, Project: "proj"})
		if err == nil {
			t.Errorf("%s: region %q was accepted, endpoint host %q", c.typ, c.region, hostFor(t, p))
		}
	}
}

// A base_url still overrides the region, which is what a PrivateLink endpoint
// or a proxy needs, and it answers to the checks base_url already has.
func TestOrdinaryCloudRegionsStillWork(t *testing.T) {
	for _, region := range []string{"us-east-1", "eu-west-3", "europe-west4", "us-east5", "ap-southeast-2"} {
		if err := CheckRegion(region); err != nil {
			t.Errorf("CheckRegion(%q) refused an ordinary region: %v", region, err)
		}
	}
}

// The whole point of the key: a committed config cannot reach a credential
// through the region either.
func TestCommittedConfigCannotAimTheTokenOffMachineByRegion(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	tree := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tree, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(tree, "config.toml")
	body := "[providers.vertex]\ntype = \"vertex\"\nproject = \"p\"\nregion = \"attacker.example.com/\"\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Providers["vertex"].Region != "attacker.example.com/" {
		t.Fatal("control failed: the region did not load")
	}
	cfg.RestrictIfInsideTree(tree)
	if _, err := New("vertex", cfg.Providers["vertex"]); err == nil {
		t.Error("a committed region built a provider")
	}
}

func hostFor(t *testing.T, p Provider) string {
	t.Helper()
	var raw string
	switch v := p.(type) {
	case *Vertex:
		raw = v.endpoint("m")
	case *Bedrock:
		raw = v.endpoint("m")
	default:
		t.Fatalf("unexpected provider %T", p)
	}
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parsing %q: %v", raw, err)
	}
	return u.Host
}
