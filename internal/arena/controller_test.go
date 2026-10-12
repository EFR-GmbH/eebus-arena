package arena

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"
)

func testController(t *testing.T) *Controller {
	t.Helper()
	dir := t.TempDir()
	vs := []Variant{}
	for _, v := range testVariants() {
		vs = append(vs, v)
	}
	if err := writeJSON(filepath.Join(dir, "variants.json"), vs); err != nil {
		t.Fatal(err)
	}
	c, err := NewController(dir, dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	return c
}

func TestVariantReloadIsAtomicAndRequiresImmutableImages(t *testing.T) {
	c := testController(t)
	good := Variant{ID: "new-revision", Stack: "enbility", Roles: []string{"EG", "CS"}, Image: "sha256:" + strings.Repeat("b", 64)}
	bad := good
	bad.ID = "mutable"
	bad.Image = "eebus-arena/enbility:local"
	if err := writeJSON(filepath.Join(c.data, "variants.json"), []Variant{good, bad}); err != nil {
		t.Fatal(err)
	}
	if err := c.LoadVariants(); err == nil {
		t.Fatal("mutable image tag accepted")
	}
	if _, exists := c.variants[good.ID]; exists {
		t.Fatal("failed catalog reload published a partial update")
	}
	if err := writeJSON(filepath.Join(c.data, "variants.json"), []Variant{good}); err != nil {
		t.Fatal(err)
	}
	if err := c.LoadVariants(); err != nil {
		t.Fatal(err)
	}
	good.Image = "sha256:" + strings.Repeat("c", 64)
	if err := writeJSON(filepath.Join(c.data, "variants.json"), []Variant{good}); err != nil {
		t.Fatal(err)
	}
	if err := c.LoadVariants(); err == nil || c.variants[good.ID].Image == good.Image {
		t.Fatal("existing revision's image was replaced")
	}
}
func TestTelemetryGapsRestartAndCommandCorrelation(t *testing.T) {
	c := testController(t)
	b := testBundle()
	b.Definition.Groups[1].Count = 1
	run, err := c.Create(b)
	if err != nil {
		t.Fatal(err)
	}
	event := Event{RunID: run, NodeID: "eg-001", Incarnation: "one", Sequence: 1, Type: "capabilities", Data: map[string]any{"commands": []any{"snapshot"}}}
	c.accept(event)
	event.Sequence = 3
	event.Type = "snapshot"
	event.Data = map[string]any{"x": 1}
	c.accept(event)
	before := c.index
	c.accept(event)
	if c.index != before {
		t.Fatal("duplicate event was retained")
	}
	found := false
	for _, e := range c.events {
		if e.Type == "telemetry.gap" {
			found = true
		}
	}
	if !found {
		t.Fatal("sequence loss must be explicit")
	}
	result := make(chan Event, 1)
	c.pending["request"] = pendingCommand{run: run, node: "eg-001", incarnation: "one", response: result}
	c.Emit(Event{RunID: run, NodeID: "cs-001", Incarnation: "one", Type: "command.result", CommandID: "request"})
	if len(result) != 0 {
		t.Fatal("wrong node satisfied command")
	}
	c.Emit(Event{RunID: run, NodeID: "eg-001", Incarnation: "one", Type: "command.result", CommandID: "request"})
	if len(result) != 1 {
		t.Fatal("matching command did not arrive")
	}
	event.Incarnation = "two"
	event.Sequence = 1
	event.Data = map[string]any{}
	c.accept(event)
	n := c.runs[run].Nodes[0]
	if len(n.Capabilities) != 0 || n.Sequence != 1 {
		t.Fatal("restart retained stale capabilities")
	}
}
func TestLifecycleCannotTargetForeignContainer(t *testing.T) {
	c := testController(t)
	b := testBundle()
	b.Definition.Groups[1].Count = 1
	run, err := c.Create(b)
	if err != nil {
		t.Fatal(err)
	}
	c.runs[run].Nodes[0].Container = "foreign"
	mutations := 0
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			mutations++
		}
		json.NewEncoder(w).Encode(map[string]any{"Config": map[string]any{"Labels": map[string]string{"org.eebus-arena.run": "another-run", "org.eebus-arena.node": "eg-001"}}})
	}))
	defer s.Close()
	address := strings.TrimPrefix(s.URL, "http://")
	c.docker = &Docker{client: &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", address)
	}}}}
	if err = c.Action(context.Background(), run, "eg-001", "kill"); err == nil || mutations != 0 {
		t.Fatal("foreign container was not protected")
	}
}
func TestHTTPRejectsCrossOriginAndExportIsExact(t *testing.T) {
	c := testController(t)
	handler := c.Handler(fstest.MapFS{"index.html": {Data: []byte("arena")}})
	b := testBundle()
	b.Definition.Groups[1].Count = 1
	run, err := c.Create(b)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("POST", "http://localhost/api/v1/runs", strings.NewReader("{}"))
	req.Header.Set("Origin", "http://foreign.test")
	req.Header.Set("Content-Type", "application/json")
	out := httptest.NewRecorder()
	handler.ServeHTTP(out, req)
	if out.Code != 403 {
		t.Fatal("cross-origin mutation accepted")
	}
	req = httptest.NewRequest("GET", "http://localhost/api/v1/runs/"+run+"/export", nil)
	out = httptest.NewRecorder()
	handler.ServeHTTP(out, req)
	saved, _ := os.ReadFile(c.path(run, "bundle.json"))
	response, _ := io.ReadAll(out.Result().Body)
	if string(response) != string(saved) {
		t.Fatal("export altered the identity bundle")
	}
	req = httptest.NewRequest("GET", "http://localhost/", nil)
	out = httptest.NewRecorder()
	handler.ServeHTTP(out, req)
	if out.Code != 200 || out.Body.String() != "arena" {
		t.Fatal("SPA root must serve HTML without a redirect loop")
	}
	req = httptest.NewRequest("GET", "http://attacker.test/api/v1/state", nil)
	out = httptest.NewRecorder()
	handler.ServeHTTP(out, req)
	if out.Code != 403 {
		t.Fatal("unrecognised Host exposed local state")
	}
}

func TestReplacedContainerCannotOverwriteNewTelemetry(t *testing.T) {
	c := testController(t)
	b := testBundle()
	b.Definition.Groups[1].Count = 1
	run, err := c.Create(b)
	if err != nil {
		t.Fatal(err)
	}
	n := c.runs[run].Nodes[0]
	n.Container = "new-container"
	e := Event{RunID: run, NodeID: n.ID, Incarnation: "old", Sequence: 1, Type: "snapshot", Data: map[string]any{"value": 123}}
	c.acceptFrom(e, "old-container")
	if !n.SnapshotAt.IsZero() {
		t.Fatal("old container overwrote current state")
	}
	e.Incarnation = "new"
	c.acceptFrom(e, "new-container")
	if n.SnapshotAt.IsZero() || n.Snapshot["value"] != 123 {
		t.Fatal("current container state was not observed")
	}
}

func TestSnapshotWithoutDataPreservesObservation(t *testing.T) {
	for _, payload := range []string{`{"type":"snapshot"}`, `{"type":"snapshot","data":null}`} {
		t.Run(payload, func(t *testing.T) {
			c := testController(t)
			b := testBundle()
			b.Definition.Groups[1].Count = 1
			run, err := c.Create(b)
			if err != nil {
				t.Fatal(err)
			}
			n := c.runs[run].Nodes[0]
			c.accept(Event{RunID: run, NodeID: n.ID, Incarnation: "one", Sequence: 1, Type: "snapshot", Data: map[string]any{"lpc": map[string]any{"state": "CONTROLLED"}, "peers": map[string]any{"peer": "disconnected"}}})
			observedAt := n.SnapshotAt
			var malformed Event
			if err := json.Unmarshal([]byte(payload), &malformed); err != nil {
				t.Fatal(err)
			}
			malformed.RunID, malformed.NodeID, malformed.Incarnation, malformed.Sequence = run, n.ID, "one", 2
			c.accept(malformed)
			if nested(n.Snapshot, "lpc", "state") != "CONTROLLED" || !n.SnapshotAt.Equal(observedAt) {
				t.Fatal("missing snapshot data replaced or refreshed the observation")
			}
			if n.Sequence != 2 || c.events[len(c.events)-1].Type != "telemetry.gap" {
				t.Fatal("malformed snapshot was not recorded as a telemetry gap")
			}
			c.accept(Event{RunID: run, NodeID: n.ID, Incarnation: "one", Sequence: 3, Type: "usecase.limit", Data: map[string]any{"usecase": "LPC", "value": 1200}})
			c.accept(Event{RunID: run, NodeID: n.ID, Incarnation: "one", Sequence: 4, Type: "ship.state", Data: map[string]any{"ski": "peer", "state": "connected"}})
			if nested(n.Snapshot, "lpc", "value") != 1200 || nested(n.Snapshot, "peers", "peer") != "connected" {
				t.Fatal("valid telemetry stopped updating after a malformed snapshot")
			}
			c.accept(Event{RunID: run, NodeID: n.ID, Incarnation: "one", Sequence: 5, Type: "snapshot", Data: map[string]any{}})
			if n.Snapshot == nil || len(n.Snapshot) != 0 || c.events[len(c.events)-1].Type != "snapshot" {
				t.Fatal("an explicit empty snapshot must still replace the observation")
			}
		})
	}
}

func TestCustomAssertionRequiresFreshRunningObservation(t *testing.T) {
	c := testController(t)
	b := testBundle()
	b.Definition.Groups[1].Count = 1
	run, err := c.Create(b)
	if err != nil {
		t.Fatal(err)
	}
	n := c.runs[run].Nodes[0]
	n.Lifecycle = "running"
	n.Snapshot = map[string]any{"state": "ready"}
	n.SnapshotAt = time.Now().Add(-time.Minute)
	d := Definition{TimeScale: 10, Steps: []Step{{Name: "fresh state", Target: n.ID, Action: "assert", Expect: &Expectation{Path: []string{"state"}, Equals: "ready", WithinSeconds: 1}}}}
	c.steps(context.Background(), run, d)
	if c.runs[run].Assertions[0].Outcome != "fail" {
		t.Fatal("stale snapshot produced a pass")
	}
}

func TestSSEHistoryExpirationIsExplicit(t *testing.T) {
	c := testController(t)
	for i := 0; i < 4002; i++ {
		c.Emit(Event{Type: "example"})
	}
	ch, history := c.Subscribe(1)
	defer c.Unsubscribe(ch)
	if len(history) == 0 || history[0].Type != "telemetry.gap" {
		t.Fatal("expired replay was silently truncated")
	}
}

func TestLiveEventsDoNotShareMutableNodeSnapshots(t *testing.T) {
	c := testController(t)
	b := testBundle()
	b.Definition.Groups[1].Count = 1
	run, err := c.Create(b)
	if err != nil {
		t.Fatal(err)
	}
	base := Event{RunID: run, NodeID: "eg-001", Incarnation: "one", Sequence: 1, Type: "snapshot", Data: map[string]any{"peers": map[string]any{"test": "disconnected"}}}
	c.accept(base)
	ch, history := c.Subscribe(0)
	defer c.Unsubscribe(ch)
	var published Event
	for _, e := range history {
		if e.Type == "snapshot" {
			published = e
		}
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for e := range ch {
			json.Marshal(e)
		}
	}()
	for i := uint64(2); i < 500; i++ {
		c.accept(Event{RunID: run, NodeID: "eg-001", Incarnation: "one", Sequence: i, Type: "ship.state", Data: map[string]any{"ski": "test", "state": "connected"}})
	}
	c.Unsubscribe(ch)
	<-done
	if nested(published.Data, "peers", "test") != "disconnected" {
		t.Fatal("published event changed after a later observation")
	}
}

func TestAbsentDockerStatisticsAreUnavailable(t *testing.T) {
	metrics := dockerMetrics(map[string]any{"cpu_stats": map[string]any{"system_cpu_usage": float64(100), "cpu_usage": map[string]any{"total_usage": float64(10)}, "online_cpus": float64(2)}})
	for _, key := range []string{"cpuPercent", "memoryBytes", "rxBytes", "blockReadBytes"} {
		if metrics[key] != nil {
			t.Fatalf("%s was reported without counters", key)
		}
	}
}

func TestPrometheusOmitsStoppedResourceGauges(t *testing.T) {
	c := testController(t)
	b := testBundle()
	b.Definition.Groups[1].Count = 1
	run, err := c.Create(b)
	if err != nil {
		t.Fatal(err)
	}
	n := c.runs[run].Nodes[0]
	n.Lifecycle = "removed"
	n.Metrics = map[string]any{"memoryBytes": float64(1234), "at": time.Now()}
	handler := c.Handler(fstest.MapFS{})
	out := httptest.NewRecorder()
	handler.ServeHTTP(out, httptest.NewRequest("GET", "http://localhost/metrics", nil))
	if strings.Contains(out.Body.String(), "arena_node_memory_bytes") {
		t.Fatal("stopped node inflated active resource gauges")
	}
	if !strings.Contains(out.Body.String(), "arena_node_metrics_available") {
		t.Fatal("metric availability was not exposed")
	}
}

func TestWatcherPreservesNativeCapabilitiesAcrossStreamTimestampOrder(t *testing.T) {
	c := testController(t)
	b := testBundle()
	b.Definition.Groups[1].Count = 1
	run, err := c.Create(b)
	if err != nil {
		t.Fatal(err)
	}
	node := c.runs[run].Nodes[0]
	node.Container = "owned"
	node.LogAt = time.Now().UTC()
	event := Event{Type: "capabilities", Incarnation: "native", Sequence: 1, Data: map[string]any{"commands": []any{"snapshot"}}}
	body, _ := json.Marshal(event)
	line := append([]byte(node.LogAt.Add(-time.Millisecond).Format(time.RFC3339Nano)+" "), body...)
	line = append(line, '\n')
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		header := make([]byte, 8)
		header[0] = 1
		binary.BigEndian.PutUint32(header[4:], uint32(len(line)))
		_, _ = w.Write(header)
		_, _ = w.Write(line)
	}))
	defer server.Close()
	address := strings.TrimPrefix(server.URL, "http://")
	c.docker = &Docker{client: &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", address)
	}}}}
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	c.watch(ctx, run, node.ID, node.Container)
	if !Contains(node.Capabilities, "snapshot") || node.Sequence != 1 {
		t.Fatal("native record was lost behind the stderr watermark", node.Capabilities, node.Sequence)
	}
}
