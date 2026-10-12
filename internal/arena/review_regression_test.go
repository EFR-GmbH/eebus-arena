package arena

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/EFR-GmbH/eebus-arena/internal/labagent"
)

func pinnedTestAgent(server *httptest.Server) *ExternalNode {
	hash := sha256.Sum256(server.Certificate().Raw)
	return &ExternalNode{Endpoint: server.URL, TLSFingerprint: hex.EncodeToString(hash[:]), Token: strings.Repeat("a", 64), Interface: "lo", Label: "test device"}
}

func TestSnapshotRedactsDeviceTokensWithoutChangingSavedDefinition(t *testing.T) {
	c := testController(t)
	run, err := c.Create(testBundle())
	if err != nil {
		t.Fatal(err)
	}
	external := &ExternalNode{Token: "private-control-token", TLSFingerprint: "public-pin"}
	c.runs[run].Nodes[0].External = external
	c.runs[run].Definition.Groups[0].External = external
	blob, _ := json.Marshal(c.Snapshot())
	if strings.Contains(string(blob), external.Token) {
		t.Fatal("state exposes device token")
	}
	if external.Token != "private-control-token" || !strings.Contains(string(blob), "public-pin") {
		t.Fatal("redaction damaged controller credentials or public certificate pin")
	}
}

func TestCleanupFailureKeepsStoppedRunRetryableAndDroppable(t *testing.T) {
	c := testController(t)
	run, err := c.Create(testBundle())
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "device unavailable", 503) }))
	defer server.Close()
	c.runs[run].Nodes[0].External = pinnedTestAgent(server)
	c.docker = &Docker{client: &http.Client{Transport: faultTransport(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("[]"))}, nil
	})}}
	bin := t.TempDir()
	if err = os.WriteFile(filepath.Join(bin, "docker"), []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	if c.Down(run) == nil {
		t.Fatal("cleanup error disappeared")
	}
	if c.runs[run].Status != "stopped" || !strings.Contains(c.runs[run].Error, "cleanup incomplete") || reconcilable(c.runs[run].Status) {
		t.Fatal("removed arena became active again")
	}
	if err = c.Drop(run); err != nil {
		t.Fatal("stopped arena cannot be dropped", err)
	}
}

func TestCompactionFailureKeepsNewRawSamplesWithoutRepeatedScans(t *testing.T) {
	c := testController(t)
	run, err := c.Create(testBundle())
	if err != nil {
		t.Fatal(err)
	}
	c.resources.segmentBytes = 512
	path := c.path(run, "resources.ndjson")
	if err = os.WriteFile(path+".2", []byte("invalid\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 7; i++ {
		c.resources.enqueue(run, ResourceRecord{At: time.Now(), NodeID: "eg-001", Type: "process", Complete: true})
	}
	if err = c.resources.flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	blob, _ := os.ReadFile(path)
	if strings.Count(string(blob), "\n") != 7 {
		t.Fatal("new samples lost after compaction failure")
	}
	c.resources.mu.Lock()
	first := c.resources.status[run]
	c.resources.mu.Unlock()
	if first.WriteErrors != 1 {
		t.Fatal("compaction retried per sample", first)
	}
	for i := 0; i < 30; i++ {
		c.resources.enqueue(run, ResourceRecord{At: time.Now(), NodeID: "eg-001", Type: "process", Complete: true})
	}
	if err = c.resources.flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	c.resources.mu.Lock()
	final := c.resources.status[run]
	c.resources.mu.Unlock()
	info, _ := os.Stat(path)
	if info.Size() > 1024 || final.RetentionDrops == 0 || final.WriteErrors != first.WriteErrors {
		t.Fatal("headroom or retry bound violated", final, info.Size())
	}
}

func TestResourceReadsDoNotPersistUnchangedMetadata(t *testing.T) {
	c := testController(t)
	run, err := c.Create(testBundle())
	if err != nil {
		t.Fatal(err)
	}
	c.resources.enqueue(run, ResourceRecord{At: time.Now(), NodeID: "eg-001", Type: "process", Complete: true})
	if err = c.resources.flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	before, _ := os.Stat(c.path(run, "resource-meta.json"))
	for i := 0; i < 4; i++ {
		if _, err = c.resources.read(context.Background(), run, func(ResourceRecord) {}); err != nil {
			t.Fatal(err)
		}
	}
	if err = c.resources.flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	after, _ := os.Stat(c.path(run, "resource-meta.json"))
	if !before.ModTime().Equal(after.ModTime()) {
		t.Fatal("read or clean flush rewrote metadata")
	}
}

func TestEvidencePinningDoesNotHoldControllerWhileWaitingForStorage(t *testing.T) {
	c := testController(t)
	run, err := c.Create(testBundle())
	if err != nil {
		t.Fatal(err)
	}
	c.resources.fileMu.Lock()
	requestDone := make(chan struct{})
	go func() {
		defer close(requestDone)
		c.Handler(nil).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/api/v1/runs/"+run+"/evidence", nil))
	}()
	// Give the request time to reach its file pinning lock.
	time.Sleep(30 * time.Millisecond)
	snapshotDone := make(chan struct{})
	go func() { c.Snapshot(); close(snapshotDone) }()
	select {
	case <-snapshotDone:
	case <-time.After(200 * time.Millisecond):
		c.resources.fileMu.Unlock()
		<-requestDone
		t.Fatal("storage wait blocked the controller mutex")
	}
	c.resources.fileMu.Unlock()
	<-requestDone
}

func TestPhysicalWatcherReusesTLSConnection(t *testing.T) {
	c := testController(t)
	run, err := c.Create(testBundle())
	if err != nil {
		t.Fatal(err)
	}
	var connections, requests atomic.Int32
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		_ = json.NewEncoder(w).Encode(labagent.Batch{Events: []labagent.Envelope{}})
	}))
	server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			connections.Add(1)
		}
	}
	server.StartTLS()
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 1100*time.Millisecond)
	defer cancel()
	c.watchPhysical(ctx, run, c.runs[run].Nodes[0].ID, pinnedTestAgent(server))
	if requests.Load() < 2 || connections.Load() != 1 {
		t.Fatal("watcher performs repeated TLS handshakes", requests.Load(), connections.Load())
	}
}

func TestExplorationOptionsFitTimelineFrameAtMaximumPopulation(t *testing.T) {
	options := make([]explorationOption, 0, 20100)
	for i := 0; i < 100; i++ {
		for n := 0; n < 201; n++ {
			options = append(options, explorationOption{ActionIndex: i, Target: strings.Repeat("n", 64) + "-001"})
		}
	}
	blob, err := json.Marshal(Event{Type: "exploration.choice", Data: map[string]any{"enabled": options, "action": strings.Repeat("x", 16<<10)}})
	if err != nil || len(blob) >= (2<<20)-4096 {
		t.Fatal("maximum choice breaks timeline framing", len(blob), err)
	}
	d := testBundle().Definition
	d.Scenario = "explore"
	d.Exploration = &Exploration{Rounds: 1, SettleSeconds: 1, Actions: []Step{{Name: "oversized", Target: "eg", Action: "command", Command: "snapshot", Params: map[string]any{"large": strings.Repeat("x", 16<<10)}}}}
	if err := d.Validate(testVariants()); err == nil || !strings.Contains(err.Error(), "16 KiB") {
		t.Fatal("oversized action accepted", err)
	}
}
