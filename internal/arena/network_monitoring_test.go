package arena

import (
	"encoding/json"
	"strings"
	"testing"
)

func monitoringVariants() map[string]Variant {
	variants := testVariants()
	v := variants["go"]
	v.Provenance = json.RawMessage(`{"arenaFeatures":{"networkModes":["ipv4","ipv6","dual-stack"],"monitoringSelection":1,"monitoringProviders":["mgcp","mpc"]}}`)
	variants["go"] = v
	return variants
}

func TestStressFamilyIsRejectedBeforeStartingAnIncompatibleArena(t *testing.T) {
	for _, mode := range []string{"ipv4", "ipv6", "dual-stack"} {
		for _, family := range []string{"", "ipv4", "ipv6", "dual-stack"} {
			t.Run(mode+"/"+family, func(t *testing.T) {
				variants := monitoringVariants()
				variants["noise"] = Variant{ID: "noise", Stack: "mdns-stresser", Roles: []string{"STRESSER"}, Image: "sha256:" + strings.Repeat("a", 64), Provenance: json.RawMessage(`{"arenaFeatures":{"networkModes":["ipv4","ipv6","dual-stack"]}}`)}
				d := testBundle().Definition
				d.Network = &LabNetwork{Mode: mode}
				d.Groups[1].Count = 1
				d.Groups[1].Variant = "go"
				d.Groups = append(d.Groups, Group{ID: "noise", Variant: "noise", Role: "STRESSER", Count: 1, Stress: &StressProfile{Family: family, Version: 1, Services: 1, Rate: 1, Burst: 1, TTL: 120, DurationSeconds: 1, Pattern: "advertise", Service: "_ship._tcp"}})
				err := d.Validate(variants)
				valid := family == "" || mode == "dual-stack" || mode == family
				if (err == nil) != valid {
					t.Fatal("family compatibility preflight", err)
				}
			})
		}
	}
}
func TestMonitoringSelectionAndImport(t *testing.T) {
	d := Definition{SchemaVersion: 3, Name: "monitoring", TimeScale: 1, Scenario: "monitoring", Groups: []Group{{ID: "eg", Variant: "go", Role: "EG", Count: 2, Monitoring: "mpc"}, {ID: "cs", Variant: "go", Role: "CS", Count: 2, Monitoring: "mpc"}}, Links: []Link{{From: "eg", To: "cs"}}, NodeOverrides: map[string]NodeOverride{"eg-002": {Monitoring: monitoringPtr("mgcp")}}}
	b := Bundle{SchemaVersion: 3, Definition: d}
	nodes, err := Resolve(&b, monitoringVariants())
	if err != nil {
		t.Fatal(err)
	}
	if nodes[0].Monitoring != "mpc" || nodes[1].Monitoring != "mgcp" || nodes[2].Monitoring != "mpc" {
		t.Fatal(nodes)
	}
	encoded, _ := json.Marshal(b)
	var restored Bundle
	if err = json.Unmarshal(encoded, &restored); err != nil {
		t.Fatal(err)
	}
	again, err := Resolve(&restored, monitoringVariants())
	if err != nil || again[1].Monitoring != "mgcp" || again[1].SKI != nodes[1].SKI {
		t.Fatal("lost provider or identity on import", err)
	}
	for _, test := range []struct {
		name   string
		change func(*Definition)
		want   string
	}{
		{"multiple", func(d *Definition) { d.NodeOverrides["eg-001"] = NodeOverride{Monitoring: monitoringPtr("mgcp")} }, "at most one"},
		{"group fans out", func(d *Definition) { d.Groups[0].Monitoring = "mgcp" }, "at most one"},
		{"CS cannot GCP", func(d *Definition) { d.NodeOverrides["cs-001"] = NodeOverride{Monitoring: monitoringPtr("mgcp")} }, "only be offered by an EG"},
		{"Java unsupported", func(d *Definition) { d.Groups[1].Variant = "java" }, "no selectable native"},
		{"unknown selection", func(d *Definition) { d.Groups[1].Monitoring = "wat" }, "monitoring must be"},
		{"unknown node", func(d *Definition) { d.NodeOverrides["missing-001"] = NodeOverride{Monitoring: monitoringPtr("mgcp")} }, "unknown"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var copy Definition
			blob, _ := json.Marshal(d)
			_ = json.Unmarshal(blob, &copy)
			test.change(&copy)
			if err := copy.Validate(monitoringVariants()); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("want %s, got %v", test.want, err)
			}
		})
	}
}
func TestNetworkModesAndCompose(t *testing.T) {
	for _, mode := range []string{"ipv4", "ipv6", "dual-stack"} {
		t.Run(mode, func(t *testing.T) {
			d := Definition{Network: &LabNetwork{Mode: mode}}
			if err := d.validateNetwork(); err != nil {
				t.Fatal(err)
			}
			c := &Controller{}
			n := c.composeNetwork(&Run{ID: "first", Definition: d})
			if n["internal"] != true || (mode != "ipv4" && n["enable_ipv6"] != true) || (mode == "ipv6" && n["enable_ipv4"] != false) {
				t.Fatal(n)
			}
			if mode != "ipv4" {
				other := c.composeNetwork(&Run{ID: "second", Definition: d})
				a, _ := json.Marshal(n["ipam"])
				b, _ := json.Marshal(other["ipam"])
				if string(a) == string(b) {
					t.Fatal("run ULA reused")
				}
			}
		})
	}
	for _, network := range []*LabNetwork{
		{Mode: "wat"}, {Mode: "ipv6", Subnet: "192.168.1.0/24"}, {Mode: "ipv4", IPv6Subnet: "fd10::/64"},
		{Mode: "ipv6", IPv6Subnet: "2001:db8::/64"}, {Mode: "ipv6", IPv6Subnet: "fd10::1/64"},
		{Mode: "ipv6", Interface: "eth0"}, {Mode: "dual-stack", Interface: "eth0", Subnet: "192.168.1.0/24", IPRange: "192.168.1.128/28"},
		{Mode: "ipv6", IPv6Subnet: "fd10::/64", IPv6Range: "fd11::/120"},
	} {
		if err := (Definition{Network: network}).validateNetwork(); err == nil {
			t.Fatalf("accepted %#v", network)
		}
	}
	d := Definition{Network: &LabNetwork{Mode: "dual-stack", Interface: "eth0", Subnet: "192.168.1.0/24", IPRange: "192.168.1.128/28", IPv6Subnet: "fd10::/64", IPv6Range: "fd10::100/120"}}
	if err := d.validateNetwork(); err != nil {
		t.Fatal(err)
	}
	n := (&Controller{}).composeNetwork(&Run{ID: "macvlan", Definition: d})
	if n["driver"] != "macvlan" || n["internal"] != nil {
		t.Fatal(n)
	}
}

func monitoringPtr(value string) *string { return &value }

func TestMDNSIPv6SourceIsObservedLinkLocal(t *testing.T) {
	source, err := ipv6LinkLocal([]string{"fd11::2/64", "fe80::aabb/64", "192.168.2.3/24"})
	if err != nil || source != "fe80::aabb" {
		t.Fatal(source, err)
	}
	for _, addresses := range [][]string{{"fd11::2/64"}, {"fe80::1/64", "fe80::2/64"}} {
		if _, err := ipv6LinkLocal(addresses); err == nil {
			t.Fatal("accepted missing or ambiguous source")
		}
	}
}

func TestRetainedMonitoringCannotSatisfyLiveAssertions(t *testing.T) {
	c := testController(t)
	b := testBundle()
	b.Definition.Groups[1].Count = 1
	run, err := c.Create(b)
	if err != nil {
		t.Fatal(err)
	}
	values := map[string]any{"offering": "mgcp", "local": map[string]any{"power": map[string]any{"available": true, "source": "native", "value": 1234.0}}}
	c.accept(Event{RunID: run, NodeID: "eg-001", Incarnation: "one", Sequence: 1, Type: "snapshot", Data: map[string]any{"monitoring": values}})
	node := c.runs[run].Nodes[0]
	saved := node.MonitoringObservation
	if saved == nil || saved.At.IsZero() || saved.Incarnation != "one" {
		t.Fatal("missing retained sample")
	}
	resetObservation(node)
	if len(node.Snapshot) != 0 || !node.SnapshotAt.IsZero() || node.MonitoringObservation != saved {
		t.Fatal("history leaked into live snapshot or was lost")
	}
}
