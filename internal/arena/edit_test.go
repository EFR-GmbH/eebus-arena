package arena

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func readEditJSON[T any](t *testing.T, path string) T {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var value T
	if err = json.Unmarshal(body, &value); err != nil {
		t.Fatal(err)
	}
	return value
}

func createEditable(t *testing.T, c *Controller) string {
	t.Helper()
	bundle := testBundle()
	bundle.Definition.Groups[1].Count = 1
	run, err := c.Create(bundle)
	if err != nil {
		t.Fatal(err)
	}
	return run
}

func TestEditAddsAndRemovesNodesWithoutChangingRetainedIdentities(t *testing.T) {
	c := testController(t)
	run := createEditable(t, c)
	original := readEditJSON[Bundle](t, c.path(run, "bundle.json"))
	previous := c.runs[run]
	created := previous.Created
	c.mu.Lock()
	previous.Status = "stopped"
	previous.Assertions = []Assertion{{ID: "prior-result", Outcome: "pass", At: time.Now()}}
	previous.MonitoringSequence = 42
	c.saveLocked(previous)
	c.mu.Unlock()
	c.flushSaves()
	state := c.path(run, "state/cs-001/pairing.json")
	if err := os.WriteFile(state, []byte("learned-state"), 0600); err != nil {
		t.Fatal(err)
	}
	definition := original.Definition
	definition.Name = "Expanded arena"
	definition.Groups[1].Count = 2
	definition.Groups[1].Resources = &ResourceLimits{CPUMillis: resourceInt(750), MemoryMiB: resourceInt(256)}
	if err := c.Update(context.Background(), run, EditRequest{Definition: definition}); err != nil {
		t.Fatal(err)
	}
	c.flushSaves()
	updated := readEditJSON[Bundle](t, c.path(run, "bundle.json"))
	r := readEditJSON[Run](t, c.path(run, "run.json"))
	if r.ID != run || r.Created != created || r.Status != "created" || r.SetupRevision != 1 || len(r.Nodes) != 3 || len(r.Assertions) != 0 || r.MonitoringSequence != 42 {
		t.Fatalf("incorrect updated run: %+v", r)
	}
	for node, identity := range original.Identities {
		if updated.Identities[node] != identity {
			t.Fatalf("identity changed for %s", node)
		}
	}
	if updated.Identities["cs-002"].SKI == "" || updated.Identities["cs-002"].SKI == updated.Identities["cs-001"].SKI || len(r.Nodes[0].TrustedSKIs) != 2 {
		t.Fatal("new identity or trust graph missing")
	}
	if body, err := os.ReadFile(state); err != nil || string(body) != "learned-state" {
		t.Fatal("compatible node state was lost", err)
	}
	archives, err := os.ReadDir(c.path(run, "revisions"))
	if err != nil || len(archives) != 1 {
		t.Fatal("previous setup not archived", err)
	}
	archived := readEditJSON[Run](t, c.path(run, filepath.Join("revisions", archives[0].Name(), "run.json")))
	if archived.Definition.Name != original.Definition.Name || len(archived.Assertions) != 1 || archived.Assertions[0].ID != "prior-result" {
		t.Fatal("previous setup and results not preserved")
	}
	compose := readEditJSON[map[string]any](t, c.path(run, "compose.json"))
	if len(compose["services"].(map[string]any)) != 3 {
		t.Fatal("compose not updated")
	}
	service := compose["services"].(map[string]any)["cs-002"].(map[string]any)
	if service["cpus"] != 0.75 || service["mem_limit"] != "256m" {
		t.Fatal("edited resources not applied", service)
	}
	definition.Groups[1].Count = 1
	if err = c.Update(context.Background(), run, EditRequest{Definition: definition, Revision: 1}); err != nil {
		t.Fatal(err)
	}
	updated = readEditJSON[Bundle](t, c.path(run, "bundle.json"))
	if _, exists := updated.Identities["cs-002"]; exists || len(updated.Identities) != 2 || updated.Identities["cs-001"] != original.Identities["cs-001"] {
		t.Fatal("removed node leaked into active setup or retained identity changed")
	}
	c.flushSaves()
	c.Close()
	reopened, err := NewController(c.data, c.hostData, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if reopened.runs[run].SetupRevision != 2 || len(reopened.runs[run].Nodes) != 2 {
		t.Fatal("updated setup did not survive restart")
	}
}

func TestEditPreservesPairingProvisioningAndResetsIncompatibleState(t *testing.T) {
	c := testController(t)
	bundle := testBundle()
	bundle.SchemaVersion, bundle.Definition.SchemaVersion = 3, 3
	bundle.Definition.Groups[1].Variant = "go"
	bundle.Definition.Groups[1].Count = 1
	bundle.Definition.Links = []Link{{From: "eg", To: "cs", Mode: "ship-pairing"}}
	run, err := c.Create(bundle)
	if err != nil {
		t.Fatal(err)
	}
	old := readEditJSON[Bundle](t, c.path(run, "bundle.json"))
	definition := old.Definition
	definition.Groups[1].Count = 2
	if err = c.Update(context.Background(), run, EditRequest{Definition: definition}); err != nil {
		t.Fatal(err)
	}
	next := readEditJSON[Bundle](t, c.path(run, "bundle.json"))
	if next.Pairing["cs-001"] != old.Pairing["cs-001"] || next.Pairing["cs-002"].Secret == "" || next.Pairing["cs-002"].Secret == old.Pairing["cs-001"].Secret {
		t.Fatal("pairing provisioning not retained/generated correctly")
	}
	definition.Links[0].Mode = "pretrusted"
	if err = c.Update(context.Background(), run, EditRequest{Definition: definition, Revision: 1}); err != nil {
		t.Fatal(err)
	}
	next = readEditJSON[Bundle](t, c.path(run, "bundle.json"))
	if len(next.Pairing) != 0 {
		t.Fatal("unused pairing credentials retained")
	}
}

func TestEditRejectsActiveStaleAndInvalidRequestsWithoutChangingFiles(t *testing.T) {
	for _, status := range []string{"running", "starting", "testing", "complete", "interrupted", "failed", "stopping", "dropping", "archived", "corrupt", "editing"} {
		t.Run(status, func(t *testing.T) {
			c := testController(t)
			run := createEditable(t, c)
			c.mu.Lock()
			c.runs[run].Status = status
			definition := c.runs[run].Definition
			c.mu.Unlock()
			if err := c.Update(context.Background(), run, EditRequest{Definition: definition}); !errors.Is(err, ErrEditActive) {
				t.Fatalf("status %s accepted: %v", status, err)
			}
		})
	}
	for _, test := range []struct {
		name   string
		change func(*Controller, string, *EditRequest)
	}{
		{"stale", func(c *Controller, run string, input *EditRequest) { input.Revision = 99 }},
		{"invalid", func(c *Controller, run string, input *EditRequest) { input.Definition.Name = "" }},
		{"container", func(c *Controller, run string, input *EditRequest) { c.runs[run].Nodes[0].Container = "still-present" }},
		{"cleanup", func(c *Controller, run string, input *EditRequest) { c.runs[run].Error = "cleanup incomplete" }},
		{"scenario", func(c *Controller, run string, input *EditRequest) { c.scenarios[run] = func() {} }},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testController(t)
			run := createEditable(t, c)
			before, err := os.ReadFile(c.path(run, "bundle.json"))
			if err != nil {
				t.Fatal(err)
			}
			input := EditRequest{Definition: readEditJSON[Bundle](t, c.path(run, "bundle.json")).Definition}
			c.mu.Lock()
			test.change(c, run, &input)
			c.mu.Unlock()
			if err = c.Update(context.Background(), run, input); err == nil {
				t.Fatal("unsafe edit accepted")
			}
			after, err := os.ReadFile(c.path(run, "bundle.json"))
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("failed edit changed setup", err)
			}
		})
	}
}

func TestEditRecoveryRestoresEveryInterruptedPublication(t *testing.T) {
	for moved := 1; moved <= len(setupParts); moved++ {
		t.Run(setupParts[moved-1], func(t *testing.T) {
			c := testController(t)
			run := createEditable(t, c)
			c.Close()
			directory := c.path(run, "")
			transaction := editTransaction{Archive: "edit-test", Staging: ".edit-test"}
			archive := filepath.Join(directory, "revisions", transaction.Archive)
			if err := os.MkdirAll(archive, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Join(directory, transaction.Staging), 0700); err != nil {
				t.Fatal(err)
			}
			if err := writeJSON(filepath.Join(directory, ".edit.json"), transaction); err != nil {
				t.Fatal(err)
			}
			for _, name := range setupParts[:moved] {
				if err := os.Rename(filepath.Join(directory, name), filepath.Join(archive, name)); err != nil {
					t.Fatal(err)
				}
				if name == "state" || name == "identities" {
					if err := os.Mkdir(filepath.Join(directory, name), 0700); err != nil {
						t.Fatal(err)
					}
				} else if err := os.WriteFile(filepath.Join(directory, name), []byte("partial-new-setup"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			reopened, err := NewController(c.data, c.hostData, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			if reopened.runs[run].Status != "created" || reopened.runs[run].SetupRevision != 0 || len(reopened.runs[run].Nodes) != 2 {
				t.Fatal("partial edit survived recovery")
			}
			if _, err = os.Stat(filepath.Join(directory, "identities", "cs-001", "cert.pem")); err != nil {
				t.Fatal("identity not restored", err)
			}
			if _, err = os.Stat(filepath.Join(directory, ".edit.json")); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("recovery marker retained", err)
			}
		})
	}
}

func TestEditHTTPStatusAndOptimisticConcurrency(t *testing.T) {
	c := testController(t)
	run := createEditable(t, c)
	input := EditRequest{Definition: readEditJSON[Bundle](t, c.path(run, "bundle.json")).Definition}
	input.Definition.Groups[1].Count = 2
	for _, test := range []struct {
		name, run string
		status    int
	}{
		{"missing", "missing", 404}, {"save", run, 200}, {"stale", run, 409},
	} {
		t.Run(test.name, func(t *testing.T) {
			body, err := json.Marshal(input)
			if err != nil {
				t.Fatal(err)
			}
			req := httptest.NewRequest("PUT", "http://localhost/api/v1/runs/"+test.run, bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()
			c.Handler(nil).ServeHTTP(response, req)
			if response.Code != test.status {
				t.Fatalf("%d: %s", response.Code, response.Body.String())
			}
		})
	}
}

func TestEditChecksDaemonBeforeRemovingNodes(t *testing.T) {
	c := testController(t)
	run := createEditable(t, c)
	c.docker = fakeDocker(t, func(w http.ResponseWriter, request *http.Request) {
		jsonResponse(w, 200, []map[string]any{{"Id": "out-of-band-container"}})
	})
	input := EditRequest{Definition: readEditJSON[Bundle](t, c.path(run, "bundle.json")).Definition}
	if err := c.Update(context.Background(), run, input); !errors.Is(err, ErrEditActive) {
		t.Fatal(err)
	}
}

func TestEditRetainsRedactedDeviceTokenOnlyForSameEndpoint(t *testing.T) {
	for _, changeEndpoint := range []bool{false, true} {
		t.Run(map[bool]string{false: "same device", true: "new endpoint"}[changeEndpoint], func(t *testing.T) {
			c := testController(t)
			bundle := testBundle()
			bundle.Definition.SchemaVersion = 3
			bundle.Definition.Groups[1].Count = 1
			bundle.Definition.Groups[0].External = &ExternalNode{Label: "stub", Endpoint: "https://192.168.238.8:4713", TLSFingerprint: strings.Repeat("a", 64), Token: strings.Repeat("b", 64), Interface: "eth0", Address: "192.168.238.8"}
			bundle.Definition.Network = &LabNetwork{Interface: "eth0", Subnet: "192.168.238.0/24", IPRange: "192.168.238.128/28"}
			run, err := c.Create(bundle)
			if err != nil {
				t.Fatal(err)
			}
			definition := readEditJSON[Bundle](t, c.path(run, "bundle.json")).Definition
			definition.Groups[0].External.Token = ""
			definition.Name = "Device edit"
			definition.Groups[0].ID = "renamed-guard"
			definition.Links[0].From = "renamed-guard"
			if changeEndpoint {
				definition.Groups[0].External.Endpoint = "https://192.168.238.9:4713"
			}
			err = c.Update(context.Background(), run, EditRequest{Definition: definition})
			if changeEndpoint {
				if err == nil {
					t.Fatal("saved credential forwarded to new endpoint")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			next := readEditJSON[Bundle](t, c.path(run, "bundle.json"))
			if next.Definition.Groups[0].External.Token != strings.Repeat("b", 64) {
				t.Fatal("redacted token lost")
			}
			body, err := json.Marshal(c.Snapshot())
			if err != nil || strings.Contains(string(body), strings.Repeat("b", 64)) {
				t.Fatal("edit exposed device token", err)
			}
		})
	}
}

func TestEditExcludesLifecycleChangesWhilePreparing(t *testing.T) {
	c := testController(t)
	run := createEditable(t, c)
	entered, release := make(chan struct{}), make(chan struct{})
	c.docker = fakeDocker(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-entered:
		default:
			close(entered)
		}
		<-release
		jsonResponse(w, 200, []any{})
	})
	input := EditRequest{Definition: readEditJSON[Bundle](t, c.path(run, "bundle.json")).Definition}
	done := make(chan error, 1)
	go func() { done <- c.Update(context.Background(), run, input) }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("edit did not reach preflight")
	}
	if err := c.Start(run); err == nil {
		t.Error("Start raced with edit")
	}
	if err := c.Down(run); err == nil {
		t.Error("Stop raced with edit")
	}
	if err := c.Drop(run); !errors.Is(err, ErrRunActive) {
		t.Error("Drop raced with edit", err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestEditCancellationAndStateMigration(t *testing.T) {
	c := testController(t)
	run := createEditable(t, c)
	input := EditRequest{Definition: readEditJSON[Bundle](t, c.path(run, "bundle.json")).Definition}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := c.Update(ctx, run, input); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	state := c.path(run, "state/cs-001/pairing.json")
	if err := os.WriteFile(state, []byte("old-stack-state"), 0600); err != nil {
		t.Fatal(err)
	}
	input.Definition.Groups[1].Variant = "go"
	if err := c.Update(context.Background(), run, input); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(state); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("incompatible stack state retained", err)
	}
	if c.runs[run].Nodes[1].SKI == "" {
		t.Fatal("identity not retained across variant change")
	}
}

func TestEditEvidenceRoundTripKeepsPriorSetupAndResults(t *testing.T) {
	c := testController(t)
	run := createEditable(t, c)
	c.mu.Lock()
	c.runs[run].Assertions = []Assertion{{ID: "before-edit", Outcome: "pass"}}
	c.saveLocked(c.runs[run])
	c.mu.Unlock()
	c.flushSaves()
	input := EditRequest{Definition: readEditJSON[Bundle](t, c.path(run, "bundle.json")).Definition}
	input.Definition.Name = "after edit"
	if err := c.Update(context.Background(), run, input); err != nil {
		t.Fatal(err)
	}
	c.flushSaves()
	response := httptest.NewRecorder()
	c.Handler(nil).ServeHTTP(response, httptest.NewRequest("GET", "http://localhost/api/v1/runs/"+run+"/evidence", nil))
	if response.Code != 200 {
		t.Fatal(response.Code, response.Body.String())
	}
	imported, err := c.ImportEvidence(context.Background(), bytes.NewReader(response.Body.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	archives, err := os.ReadDir(c.path(imported, "revisions"))
	if err != nil || len(archives) != 1 {
		t.Fatal("prior setup missing after evidence import", err)
	}
	previous := readEditJSON[Run](t, c.path(imported, filepath.Join("revisions", archives[0].Name(), "run.json")))
	if len(previous.Assertions) != 1 || previous.Assertions[0].ID != "before-edit" || previous.Definition.Name == input.Definition.Name || previous.Status != "archived" {
		t.Fatal("historical evidence changed or was made executable")
	}
	if err = c.Update(context.Background(), imported, input); !errors.Is(err, ErrEditActive) {
		t.Fatal("archived evidence became editable", err)
	}
}

func TestEditPublicationFailureRollsBackMovedParts(t *testing.T) {
	c := testController(t)
	run := createEditable(t, c)
	before := readEditJSON[Bundle](t, c.path(run, "bundle.json"))
	if err := os.Remove(c.path(run, "compose.json")); err != nil {
		t.Fatal(err)
	}
	input := EditRequest{Definition: before.Definition}
	input.Definition.Name = "must not be published"
	if err := c.Update(context.Background(), run, input); err == nil {
		t.Fatal("publication without original compose succeeded")
	}
	if after := readEditJSON[Bundle](t, c.path(run, "bundle.json")); after.Definition.Name != before.Definition.Name || after.Identities["cs-001"] != before.Identities["cs-001"] {
		t.Fatal("failed publication was not rolled back")
	}
	if c.runs[run].SetupRevision != 0 || c.runs[run].Status != "created" {
		t.Fatal("failed publication changed saved run")
	}
	if _, err := os.Stat(c.path(run, ".edit.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("rollback marker retained", err)
	}
}
