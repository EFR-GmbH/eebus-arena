package arena

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
)

func TestDropPreservesBundleAndSurvivesRestart(t *testing.T) {
	c := testController(t)
	bundle := testBundle()
	bundle.Definition.Groups[1].Count = 1
	id, err := c.Create(bundle)
	if err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(c.path(id, "bundle.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Drop(id); err != nil {
		t.Fatal(err)
	}
	if c.runs[id] != nil || c.journal[id] != nil || c.dirty[id] {
		t.Fatal("dropped arena remained live")
	}
	if _, err := os.Stat(c.path(id, "")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("dropped arena remained in runs")
	}
	archived, err := os.ReadFile(filepath.Join(c.data, "dropped", id, "bundle.json"))
	if err != nil || string(archived) != string(original) {
		t.Fatal("identity bundle was not preserved exactly")
	}
	c.Close()
	restarted, err := NewController(c.data, c.hostData, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	if restarted.runs[id] != nil {
		t.Fatal("restart resurrected dropped arena")
	}
	if err := restarted.Restore(id); err != nil {
		t.Fatal(err)
	}
	if restarted.runs[id] == nil || restarted.journal[id] == nil {
		t.Fatal("restore did not reopen arena and evidence")
	}
	restored, _ := os.ReadFile(restarted.path(id, "bundle.json"))
	if string(restored) != string(original) {
		t.Fatal("restore changed SKIs or identities")
	}
	if err := restarted.Restore(id); err == nil {
		t.Fatal("duplicate restore accepted")
	}
	if err := restarted.Drop(id); err != nil {
		t.Fatal("restored arena cannot be dropped again:", err)
	}
}

func TestDropRejectsActiveAndBusyArenas(t *testing.T) {
	for _, state := range []string{"starting", "running", "testing", "stopping", "complete", "interrupted", "failed"} {
		t.Run(state, func(t *testing.T) {
			c := testController(t)
			id, err := c.Create(testBundle())
			if err != nil {
				t.Fatal(err)
			}
			c.runs[id].Status = state
			if !errors.Is(c.Drop(id), ErrRunActive) {
				t.Fatal("active arena was dropped")
			}
			if _, err := os.Stat(c.path(id, "bundle.json")); err != nil {
				t.Fatal("rejected drop changed files")
			}
		})
	}
	c := testController(t)
	id, err := c.Create(testBundle())
	if err != nil {
		t.Fatal(err)
	}
	c.scenarios[id] = func() {}
	if !errors.Is(c.Drop(id), ErrRunActive) {
		t.Fatal("scenario still finishing was dropped")
	}
	delete(c.scenarios, id)
	c.starts[id] = startOperation{}
	if !errors.Is(c.Drop(id), ErrRunActive) {
		t.Fatal("start still finishing was dropped")
	}
	delete(c.starts, id)
	c.runs[id].Nodes[0].Container = "not-removed"
	if !errors.Is(c.Drop(id), ErrRunActive) {
		t.Fatal("arena with a container was dropped")
	}
}

func TestFailedDropRetainsArena(t *testing.T) {
	c := testController(t)
	id, err := c.Create(testBundle())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(c.data, "dropped"), []byte("blocked"), 0600); err != nil {
		t.Fatal(err)
	}
	if c.Drop(id) == nil {
		t.Fatal("archive failure was hidden")
	}
	if c.runs[id] == nil || c.journal[id] == nil {
		t.Fatal("failed drop removed live state")
	}
	if _, err := os.Stat(c.path(id, "bundle.json")); err != nil {
		t.Fatal("failed drop lost identities")
	}
}

func TestDropHTTPAndLateTelemetry(t *testing.T) {
	c := testController(t)
	id, err := c.Create(testBundle())
	if err != nil {
		t.Fatal(err)
	}
	h := c.Handler(fstest.MapFS{})
	request := func(method, path, origin string) int {
		r := httptest.NewRequest(method, "http://localhost"+path, strings.NewReader("{}"))
		r.Header.Set("Content-Type", "application/json")
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}
	path := "/api/v1/runs/" + id
	if request("DELETE", path, "http://foreign.test") != 403 {
		t.Fatal("cross-origin drop accepted")
	}
	c.runs[id].Status = "running"
	if request("DELETE", path, "") != 409 {
		t.Fatal("active drop did not return conflict")
	}
	c.runs[id].Status = "created"
	if request("DELETE", path, "") != 200 {
		t.Fatal("drop failed")
	}
	c.accept(Event{RunID: id, NodeID: "eg-001", Incarnation: "late", Sequence: 1, Type: "snapshot", Data: map[string]any{"state": "ready"}})
	if c.runs[id] != nil || c.dirty[id] {
		t.Fatal("late telemetry resurrected arena")
	}
	if request("GET", path+"/export", "") != 404 {
		t.Fatal("dropped arena still exports")
	}
	if request("DELETE", path, "") != 404 {
		t.Fatal("missing drop did not return not found")
	}
	if request("POST", path+"/restore", "") != 200 {
		t.Fatal("restore failed")
	}
	if err := c.Restore("../outside"); !errors.Is(err, ErrRunNotFound) {
		t.Fatal("restore accepted path traversal")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := c.Action(ctx, id, "eg-001", "start"); err == nil {
		t.Fatal("restore set a container reference")
	}
}

func TestDropRejectsContainersMissingFromSavedState(t *testing.T) {
	c := testController(t)
	id, err := c.Create(testBundle())
	if err != nil {
		t.Fatal(err)
	}
	mutations := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			mutations++
		}
		json.NewEncoder(w).Encode([]map[string]any{{"Id": "external-container", "State": "running"}})
	}))
	defer server.Close()
	address := strings.TrimPrefix(server.URL, "http://")
	c.docker = &Docker{client: &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", address)
	}}}}
	if !errors.Is(c.Drop(id), ErrRunActive) || mutations != 0 {
		t.Fatal("drop ignored the daemon or mutated a live container")
	}
	if c.runs[id] == nil {
		t.Fatal("rejected drop removed saved arena")
	}
}
