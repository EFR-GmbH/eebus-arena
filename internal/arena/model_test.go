package arena

import (
	"encoding/json"
	"strings"
	"testing"
)

func testVariants() map[string]Variant {
	return map[string]Variant{"go": {ID: "go", Stack: "enbility", Roles: []string{"EG", "CS"}, Image: "sha256:" + strings.Repeat("a", 64)}, "java": {ID: "java", Stack: "jeebus", Roles: []string{"CS"}, Image: "sha256:" + strings.Repeat("d", 64)}}
}
func testBundle() Bundle {
	return Bundle{SchemaVersion: 1, Definition: Definition{SchemaVersion: 1, Name: "test", Groups: []Group{{ID: "eg", Variant: "go", Role: "EG", Count: 1}, {ID: "cs", Variant: "java", Role: "CS", Count: 50}}}}
}
func TestIdentityAndImportPreserveTrust(t *testing.T) {
	b := testBundle()
	nodes, err := Resolve(&b, testVariants())
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 51 || len(nodes[0].TrustedSKIs) != 50 {
		t.Fatal("incorrect fan-out")
	}
	for _, n := range nodes[1:] {
		if len(n.TrustedSKIs) != 1 || n.TrustedSKIs[0] != nodes[0].SKI {
			t.Fatal("trust must be bilateral and restricted to explicit links")
		}
	}
	encoded, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	var imported Bundle
	if err = json.Unmarshal(encoded, &imported); err != nil {
		t.Fatal(err)
	}
	restored, err := Resolve(&imported, testVariants())
	if err != nil {
		t.Fatal(err)
	}
	for i, n := range nodes {
		if n.SKI != restored[i].SKI || b.Identities[n.ID].PrivateKey != imported.Identities[n.ID].PrivateKey {
			t.Fatal("identity changed during import")
		}
	}
	wrong := imported.Identities["cs-001"]
	wrong.SKI = nodes[0].SKI
	imported.Identities["cs-001"] = wrong
	if _, err = Resolve(&imported, testVariants()); err == nil {
		t.Fatal("accepted forged SKI")
	}
}
func TestRejectAmbiguousTrustAndUnsupportedRole(t *testing.T) {
	b := testBundle()
	b.Definition.Groups[0].Count = 2
	if _, err := Resolve(&b, testVariants()); err == nil {
		t.Fatal("multiple EGs require explicit trust")
	}
	b.Definition.Links = []Link{{From: "eg", To: "cs"}}
	if _, err := Resolve(&b, testVariants()); err != nil {
		t.Fatal(err)
	}
	b.Definition.Groups[0].Variant = "java"
	if _, err := Resolve(&b, testVariants()); err == nil {
		t.Fatal("jEEBUS public CS demo cannot be an EG")
	}
}
func TestDuplicateIdentityRejected(t *testing.T) {
	b := testBundle()
	b.Definition.Groups[1].Count = 2
	_, err := Resolve(&b, testVariants())
	if err != nil {
		t.Fatal(err)
	}
	b.Identities["cs-002"] = b.Identities["cs-001"]
	if _, err := Resolve(&b, testVariants()); err == nil {
		t.Fatal("accepted duplicate identity")
	}
}
