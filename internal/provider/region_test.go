package provider

import "testing"

func TestRegionShapeAcceptsRealRegions(t *testing.T) {
	good := []string{"us-east-1", "us-east5", "europe-west4", "global", "ap-southeast-2", "us-gov-west-1"}
	bad := []string{"attacker.example.com/", "us-east-1/../..", "evil.com", "a b", "us-east-1?x=1", "..", "/etc"}
	for _, r := range good {
		if err := CheckRegion(r); err != nil {
			t.Errorf("legitimate region %q rejected: %v", r, err)
		}
	}
	for _, r := range bad {
		if err := CheckRegion(r); err == nil {
			t.Errorf("region %q accepted", r)
		}
	}
}
