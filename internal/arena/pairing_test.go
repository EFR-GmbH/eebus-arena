package arena

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func pairingBundle() Bundle {
	b := testBundle()
	b.SchemaVersion = 2
	b.Definition.SchemaVersion = 2
	b.Definition.Groups[1].Variant = "go"
	b.Definition.Groups[1].Count = 1
	b.Definition.Links = []Link{{From: "eg", To: "cs", Mode: "ship-pairing"}}
	return b
}
func TestPairingProvisioningRoundTrip(t *testing.T) {
	b := pairingBundle()
	nodes, err := Resolve(&b, testVariants())
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range nodes {
		if len(n.TrustedSKIs) != 0 || len(n.IntendedPeers) != 1 || len(n.PairingPeers) != 1 {
			t.Fatalf("pretrusted pairing: %+v", n)
		}
	}
	if len(b.Pairing["cs-001"].Secret) != 32 || b.Pairing["eg-001"].Secret != "" {
		t.Fatal("incorrect credential roles")
	}
	data, _ := json.Marshal(b)
	var imported Bundle
	json.Unmarshal(data, &imported)
	again, err := Resolve(&imported, testVariants())
	if err != nil {
		t.Fatal(err)
	}
	if !jsonEqual(b.Pairing, imported.Pairing) || again[1].Fingerprint != nodes[1].Fingerprint {
		t.Fatal("identities changed on import")
	}
	cred := imported.Pairing["cs-001"]
	cred.Fingerprint = strings.Repeat("A", 64)
	imported.Pairing["cs-001"] = cred
	if _, err = Resolve(&imported, testVariants()); err == nil {
		t.Fatal("mismatched fingerprint accepted")
	}
}
func TestMigrationAndStresserTopology(t *testing.T) {
	b := testBundle()
	b.Definition.Groups[1].Count = 1
	nodes, err := Resolve(&b, testVariants())
	if err != nil {
		t.Fatal(err)
	}
	if b.SchemaVersion != SchemaVersion || b.Definition.SchemaVersion != SchemaVersion || len(nodes[0].TrustedSKIs) != 1 {
		t.Fatal("v1 trust changed")
	}
	vs := testVariants()
	vs["noise"] = Variant{ID: "noise", Stack: "mdns-stresser", Roles: []string{"STRESSER"}}
	b.Definition.Groups = append(b.Definition.Groups, Group{ID: "noise", Variant: "noise", Role: "STRESSER", Count: 1})
	nodes, err = Resolve(&b, vs)
	if err != nil {
		t.Fatal(err)
	}
	if nodes[2].SKI != "" || len(b.Identities) != 2 || len(nodes[2].TrustedSKIs) > 0 {
		t.Fatal("stresser acquired identity or trust")
	}
	b.Definition.Links = append(b.Definition.Links, Link{From: "eg", To: "noise"})
	if _, err = Resolve(&b, vs); err == nil {
		t.Fatal("stresser trust link accepted")
	}
}
func TestPairingResetRequiresStoppedNodeAndPreservesIdentity(t *testing.T) {
	c := testController(t)
	run, err := c.Create(pairingBundle())
	if err != nil {
		t.Fatal(err)
	}
	p := c.path(run, filepath.Join("state", "cs-001", "pairing.json"))
	if err = os.WriteFile(p, []byte(`{"trusted":{}}`), 0600); err != nil {
		t.Fatal(err)
	}
	c.runs[run].Nodes[1].Lifecycle = "running"
	if c.ResetPairing(context.Background(), run, "cs-001") == nil {
		t.Fatal("reset running node")
	}
	c.runs[run].Nodes[1].Lifecycle = "removed"
	before, err := os.ReadFile(c.path(run, "bundle.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err = c.ResetPairing(context.Background(), run, "cs-001"); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(c.path(run, "bundle.json"))
	if string(before) != string(after) {
		t.Fatal("reset changed provisioning")
	}
	if _, err = os.Stat(p); !os.IsNotExist(err) {
		t.Fatal("state retained")
	}
}
func TestLearnedTrustRecognizesFingerprintBeforeSKI(t *testing.T) {
	peer := &Node{SKI: "ski", Fingerprint: "AABB"}
	n := &Node{Snapshot: map[string]any{"pairing": map[string]any{"trusted": map[string]any{"a": map[string]any{"fingerprint": "aabb"}}}}}
	if !observedTrust(n, peer) {
		t.Fatal("learned fingerprint ignored")
	}
	n.Snapshot = map[string]any{}
	if observedTrust(n, peer) {
		t.Fatal("missing trust accepted")
	}
}
func TestEventAbsenceRequiresCompleteTelemetry(t *testing.T) {
	for _, tc := range []struct {
		name  string
		event string
		want  string
	}{{"quiet", "", "pass"}, {"gap", "telemetry.gap", "inconclusive"}, {"suppressed", "pairing.suppressed", "inconclusive"}, {"forbidden", "pairing.trust", "fail"}} {
		t.Run(tc.name, func(t *testing.T) {
			c := testController(t)
			run, err := c.Create(pairingBundle())
			if err != nil {
				t.Fatal(err)
			}
			n := c.runs[run].Nodes[1]
			n.Lifecycle = "running"
			n.Incarnation = "test"
			n.LastSeen = time.Now()
			n.Capabilities = []string{"observe.pairing"}
			cursor := c.index
			if tc.event != "" {
				c.Emit(Event{RunID: run, NodeID: n.ID, Incarnation: "test", Type: tc.event, Data: map[string]any{"trusted": true}})
			}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
			defer cancel()
			outcome, _ := c.observeEvent(ctx, run, n.ID, &Expectation{Event: "pairing.trust", Match: map[string]any{"trusted": true}, Absent: true}, cursor, "test")
			if outcome != tc.want {
				t.Fatalf("got %s want %s", outcome, tc.want)
			}
		})
	}
}

func TestScenarioRequiresWorkload(t *testing.T) {
	for _, scenario := range []string{"pairing", "mdns-stress"} {
		t.Run(scenario, func(t *testing.T) {
			b := testBundle()
			b.Definition.Scenario = scenario
			if _, err := Resolve(&b, testVariants()); err == nil {
				t.Fatal("empty workload accepted")
			}
		})
	}
}
func TestStresserPublicConfigWithoutIdentityFiles(t *testing.T) {
	c := testController(t)
	c.variants["noise"] = Variant{ID: "noise", Stack: "mdns-stresser", Roles: []string{"STRESSER"}}
	b := pairingBundle()
	b.Definition.Groups = append(b.Definition.Groups, Group{ID: "noise", Variant: "noise", Role: "STRESSER", Count: 1})
	id, err := c.Create(b)
	if err != nil {
		t.Fatal(err)
	}
	dir := c.path(id, filepath.Join("identities", "noise-001"))
	for _, f := range []string{"cert.pem", "key.pem"} {
		if _, err := os.Stat(filepath.Join(dir, f)); !os.IsNotExist(err) {
			t.Fatal("stresser identity", f, err)
		}
	}
	info, err := os.Stat(filepath.Join(dir, "config.json"))
	if err != nil || info.Mode().Perm() != 0644 {
		t.Fatal("public config inaccessible", err)
	}
	info, err = os.Stat(dir)
	if err != nil || info.Mode().Perm() != 0755 {
		t.Fatal("public config directory inaccessible", err)
	}
}
