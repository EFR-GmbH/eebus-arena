package arena

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

type faultTransport func(*http.Request) (*http.Response, error)

func (f faultTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestAddressInspectionCleanupOnlyRemovesOwnedInspectors(t *testing.T) {
	for _, changed := range []bool{false, true} {
		t.Run(map[bool]string{false: "owned", true: "ownership changed"}[changed], func(t *testing.T) {
			c := testController(t)
			run, err := c.Create(testBundle())
			if err != nil {
				t.Fatal(err)
			}
			labels := map[string]string{"org.eebus-arena.helper.run": run, "org.eebus-arena.helper.node": "eg-001", "org.eebus-arena.address-inspection": "true"}
			removed := false
			c.docker = &Docker{client: &http.Client{Transport: faultTransport(func(r *http.Request) (*http.Response, error) {
				var value any
				switch {
				case r.URL.Path == "/containers/json":
					value = []any{map[string]any{"Id": "own", "Labels": labels}, map[string]any{"Id": "foreign", "Labels": map[string]string{"org.eebus-arena.helper.run": "other-controller", "org.eebus-arena.helper.node": "eg-001", "org.eebus-arena.address-inspection": "true"}}}
				case r.URL.Path == "/containers/own/json":
					copy := map[string]string{}
					for key, value := range labels {
						copy[key] = value
					}
					if changed {
						copy["org.eebus-arena.helper.run"] = "other-controller"
					}
					value = map[string]any{"Config": map[string]any{"Labels": copy}, "HostConfig": map[string]any{"NetworkMode": "container:owned-node"}}
				case r.Method == "DELETE" && r.URL.Path == "/containers/own":
					removed = true
					value = map[string]any{}
				default:
					t.Error("unexpected inspection access", r.Method, r.URL.Path)
					value = map[string]any{}
				}
				body, _ := json.Marshal(value)
				return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(body))}, nil
			})}}
			err = c.cleanupAddressInspections(context.Background())
			if removed == changed || (err != nil) != changed {
				t.Fatal("inspection cleanup ownership", removed, err)
			}
		})
	}
}

func TestAddressInspectionHasBoundedPrivilegesAndResources(t *testing.T) {
	host := addressInspectionConfig("node", "image", "run", "eg-001")["HostConfig"].(map[string]any)
	if host["Memory"] != 32<<20 || host["MemorySwap"] != 32<<20 || host["ReadonlyRootfs"] != true || host["PidsLimit"] != 16 {
		t.Fatal("unbounded inspection resources", host)
	}
	if len(host["CapDrop"].([]string)) != 1 || host["CapDrop"].([]string)[0] != "ALL" || host["CapAdd"] != nil || len(host["SecurityOpt"].([]string)) != 1 || host["LogConfig"] == nil {
		t.Fatal("inspection privilege or log bounds missing", host)
	}
}

func TestFaultRecoveryRequiresOwnershipAndNativeProof(t *testing.T) {
	for _, test := range []struct {
		name                                              string
		foreign, missingProof, missingHelper, missingNode bool
	}{
		{name: "verified recovery"}, {name: "foreign namespace", foreign: true}, {name: "no native restoration proof", missingProof: true},
		{name: "missing helper still needs native recovery", missingHelper: true},
		{name: "removed node and helper retire namespace", missingHelper: true, missingNode: true},
		{name: "removed node with stopped helper retires namespace", missingNode: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testController(t)
			run, err := c.Create(testBundle())
			if err != nil {
				t.Fatal(err)
			}
			node := c.runs[run].Nodes[0]
			node.Container = "owned-node"
			lease := &FaultLease{ID: "fault-aaaaaaaaaaaaaaaaaaaaaaaa", Run: run, Node: node.ID, Container: node.Container, Helper: "original", Image: "sha256:" + strings.Repeat("a", 64), State: "cleanup_unverified", Profile: NetworkFault{From: node.ID, To: "cs-001", Traffic: "unicast", DurationSeconds: 10}, Started: time.Now()}
			c.faults = map[string]*FaultLease{lease.ID: lease}
			created := false
			c.docker = &Docker{client: &http.Client{Transport: faultTransport(func(r *http.Request) (*http.Response, error) {
				var result any = map[string]any{}
				path := r.URL.Path
				if test.missingNode && path == "/containers/original/stop" {
					return &http.Response{StatusCode: 304, Body: http.NoBody}, nil
				}
				if path == "/containers/original/json" && test.missingHelper || path == "/containers/owned-node/json" && test.missingNode {
					return &http.Response{StatusCode: 404, Body: io.NopCloser(strings.NewReader(`{"message":"No such container"}`))}, nil
				}
				if strings.HasSuffix(path, "/json") {
					labels := map[string]string{"org.eebus-arena.helper.run": run, "org.eebus-arena.helper.node": node.ID, "org.eebus-arena.fault": lease.ID}
					if strings.Contains(path, "owned-node") {
						labels = map[string]string{"org.eebus-arena.run": run, "org.eebus-arena.node": node.ID}
						if test.foreign {
							labels["org.eebus-arena.run"] = "foreign"
						}
					}
					result = map[string]any{"Config": map[string]any{"Labels": labels}, "State": map[string]any{"Running": false}}
				}
				if path == "/containers/create" {
					created = true
					var config map[string]any
					_ = json.NewDecoder(r.Body).Decode(&config)
					if nested(config, "HostConfig", "NetworkMode") != "container:owned-node" || nested(config, "Image") != lease.Image {
						t.Fatal("recovery escaped pinned namespace", config)
					}
					result = map[string]string{"Id": "cleaner"}
				}
				if strings.HasSuffix(path, "/logs") {
					payload := "2026-10-10T00:00:00Z {\"type\":\"fault.restored\",\"recovered\":true}\n"
					if test.missingProof {
						payload = ""
					}
					var frame bytes.Buffer
					header := make([]byte, 8)
					header[0] = 1
					binary.BigEndian.PutUint32(header[4:], uint32(len(payload)))
					frame.Write(header)
					frame.WriteString(payload)
					return &http.Response{StatusCode: 200, Body: io.NopCloser(&frame)}, nil
				}
				if strings.HasSuffix(path, "/wait") {
					result = map[string]int{"StatusCode": 0}
				}
				blob, _ := json.Marshal(result)
				return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(blob))}, nil
			})}}
			err = c.recoverFault(context.Background(), lease.ID)
			if test.foreign || test.missingProof {
				if err == nil || c.faults[lease.ID] == nil {
					t.Fatal("unverified cleanup accepted")
				}
			} else if err != nil || c.faults[lease.ID] != nil {
				t.Fatal("verified cleanup failed", err)
			}
			if test.foreign && created {
				t.Fatal("foreign namespace received helper")
			}
			if test.missingNode && created {
				t.Fatal("recovery attempted in a removed namespace")
			}
			if test.missingHelper && !test.missingNode && !created {
				t.Fatal("missing helper was accepted without native cleanup")
			}
		})
	}
}

func TestFaultValidationRejectsUnscopedOrUnboundedProfiles(t *testing.T) {
	valid := NetworkFault{From: "eg-001", To: "cs-001", Traffic: "unicast", DelayMillis: 10, DurationSeconds: 10}
	for _, change := range []func(*NetworkFault){func(f *NetworkFault) { f.To = f.From }, func(f *NetworkFault) { f.Traffic = "all" }, func(f *NetworkFault) { f.DurationSeconds = 601 }, func(f *NetworkFault) { f.JitterMillis = 11 }} {
		f := valid
		change(&f)
		if f.Validate() == nil {
			t.Fatal("unsafe profile accepted", f)
		}
	}
}
