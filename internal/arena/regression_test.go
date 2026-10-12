package arena

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"testing/synctest"
	"time"
)

func fakeDocker(t *testing.T, handler http.HandlerFunc) *Docker {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	address := strings.TrimPrefix(server.URL, "http://")
	client := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", address)
	}}}
	t.Cleanup(client.CloseIdleConnections)
	return &Docker{client: client}
}
func regressionRun(t *testing.T, c *Controller) *Run {
	t.Helper()
	b := testBundle()
	b.Definition.Groups[1].Count = 1
	id, err := c.Create(b)
	if err != nil {
		t.Fatal(err)
	}
	return c.runs[id]
}
func dockerFrame(w io.Writer, stream byte, text string) {
	header := make([]byte, 8)
	header[0] = stream
	binary.BigEndian.PutUint32(header[4:], uint32(len(text)))
	w.Write(header)
	io.WriteString(w, text)
}
func TestDockerPartialLogTimestamps(t *testing.T) {
	stamp := "2026-10-08T10:11:12.123456789Z "
	payload := `{"type":"snapshot","data":{"large":"` + strings.Repeat("x", 40000) + `"}}`
	d := fakeDocker(t, func(w http.ResponseWriter, r *http.Request) {
		dockerFrame(w, 1, stamp+payload[:16000])
		dockerFrame(w, 2, stamp+"warning\n")
		dockerFrame(w, 1, stamp+payload[16000:32000])
		dockerFrame(w, 1, stamp+payload[32000:]+"\n")
	})
	var lines []string
	err := d.Logs(context.Background(), "node", time.Time{}, func(stream byte, line string) { lines = append(lines, fmt.Sprintf("%d:%s", stream, line)) })
	if err != io.EOF {
		t.Fatal(err)
	}
	if len(lines) != 2 || lines[0] != "2:"+stamp+"warning" || lines[1] != "1:"+stamp+payload {
		t.Fatal("fragmented JSON or stderr was corrupted")
	}
}
func TestLogCursorKeepsEqualTimestampRowsAndSkipsReplay(t *testing.T) {
	c := testController(t)
	r := regressionRun(t, c)
	n := r.Nodes[0]
	n.Container = "node"
	stamp := time.Now().UTC().Truncate(time.Second)
	n.LogAt = stamp
	first := "first"
	key := fmt.Sprintf("%d:%x", 1, sha256.Sum256([]byte(first)))
	n.LogRows = map[string]int{key: 1}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.docker = fakeDocker(t, func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Query().Get("since") == "" {
			t.Error("watch replayed without cursor")
		}
		for _, line := range []string{first, "second", "third"} {
			dockerFrame(w, 1, stamp.Format(time.RFC3339Nano)+" "+line+"\n")
		}
		w.(http.Flusher).Flush()
		<-req.Context().Done()
	})
	done := make(chan struct{})
	go func() { defer close(done); c.watch(ctx, r.ID, n.ID, n.Container) }()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		c.mu.Lock()
		count := len(c.events)
		c.mu.Unlock()
		if count >= 3 {
			break
		}
		time.Sleep(time.Millisecond * 10)
	}
	cancel()
	<-done
	c.mu.Lock()
	defer c.mu.Unlock()
	var got []string
	for _, e := range c.events {
		if e.Type == "log" {
			got = append(got, fmt.Sprint(e.Data["line"]))
		}
	}
	if strings.Join(got, ",") != "second,third" {
		t.Fatalf("log cursor dropped or replayed lines: %v", got)
	}
}
func TestCanceledWaitAndShutdownDoNotCompleteScenario(t *testing.T) {
	c := testController(t)
	r := regressionRun(t, c)
	r.Status = "running"
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if c.wait(ctx, r.ID, func(*Run) bool { return true }) {
		t.Fatal("canceled wait credited stale success")
	}
	if err := c.Test(r.ID); err != nil {
		t.Fatal(err)
	}
	c.Close()
	if r.Status != "interrupted" {
		t.Fatalf("shutdown status %s", r.Status)
	}
	for _, a := range r.Assertions {
		if a.Outcome == "fail" {
			t.Fatalf("shutdown produced protocol failure: %+v", a)
		}
	}
	data, err := os.ReadFile(c.path(r.ID, "run.json"))
	if err != nil {
		t.Fatal(err)
	}
	var saved Run
	json.Unmarshal(data, &saved)
	if saved.Status != "interrupted" {
		t.Fatal("final status not persisted")
	}
}
func TestRestartRetiresOldIncarnationAndClearsReadiness(t *testing.T) {
	c := testController(t)
	r := regressionRun(t, c)
	n := r.Nodes[0]
	c.accept(Event{RunID: r.ID, NodeID: n.ID, Incarnation: "old", Sequence: 1, Type: "capabilities", Data: map[string]any{"commands": []any{"snapshot"}}})
	c.accept(Event{RunID: r.ID, NodeID: n.ID, Incarnation: "old", Sequence: 2, Type: "snapshot", Data: map[string]any{"peers": map[string]any{"peer": "connected"}}})
	c.mu.Lock()
	c.resetNodeLocked(r.ID, n, time.Now())
	c.mu.Unlock()
	c.accept(Event{RunID: r.ID, NodeID: n.ID, Incarnation: "old", Sequence: 3, Type: "capabilities", Data: map[string]any{"commands": []any{"snapshot"}}})
	if len(n.Capabilities) != 0 || n.Incarnation != "" || !n.SnapshotAt.IsZero() {
		t.Fatal("retired process restored stale readiness")
	}
	c.accept(Event{RunID: r.ID, NodeID: n.ID, Incarnation: "new", Sequence: 1, Type: "capabilities", Data: map[string]any{"commands": []any{"snapshot"}}})
	if n.Incarnation != "new" || len(n.Capabilities) != 1 {
		t.Fatal("fresh process was rejected")
	}
}
func TestUnchangedSnapshotsDoNotFloodOrCreditLimitWrites(t *testing.T) {
	c := testController(t)
	r := regressionRun(t, c)
	n := r.Nodes[0]
	e := Event{RunID: r.ID, NodeID: n.ID, Incarnation: "one", Sequence: 1, Type: "snapshot", Data: map[string]any{"lpc": map[string]any{"value": 1200.0, "active": true}}}
	c.accept(e)
	before := c.index
	observed := n.LimitAt["lpc"]
	for i := 2; i < 1000; i++ {
		e.Sequence = uint64(i)
		c.accept(e)
	}
	if c.index != before {
		t.Fatalf("unchanged snapshots produced %d history entries", c.index-before)
	}
	if n.LimitAt["lpc"] != observed {
		t.Fatal("periodic snapshots credited an unchanged limit")
	}
	e.Sequence = 1000
	e.Type = "usecase.limit"
	e.Data = map[string]any{"usecase": "lpc", "value": 1200.0, "active": true}
	c.accept(e)
	if !n.LimitAt["lpc"].After(observed) {
		t.Fatal("native receive observation did not credit write")
	}
}
func TestLateReconcileCannotReviveStoppedRun(t *testing.T) {
	c := testController(t)
	r := regressionRun(t, c)
	r.Status = "running"
	entered, release := make(chan struct{}), make(chan struct{})
	c.docker = fakeDocker(t, func(w http.ResponseWriter, req *http.Request) {
		close(entered)
		<-release
		json.NewEncoder(w).Encode([]map[string]any{{"Id": "stale", "State": "exited", "Labels": map[string]string{"org.eebus-arena.node": r.Nodes[0].ID}}})
	})
	done := make(chan struct{})
	go func() { defer close(done); c.reconcile(r.ID) }()
	<-entered
	c.mu.Lock()
	r.Status = "stopped"
	r.generation++
	c.mu.Unlock()
	close(release)
	<-done
	if r.Nodes[0].Container != "" {
		t.Fatal("late reconciliation revived old container")
	}
}
func TestReconcileMarksAbsentContainersMissing(t *testing.T) {
	c := testController(t)
	r := regressionRun(t, c)
	r.Status = "running"
	n := r.Nodes[0]
	n.Container = "gone"
	n.Capabilities = []string{"snapshot"}
	c.docker = fakeDocker(t, func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "[]") })
	c.reconcile(r.ID)
	if n.Container != "" || n.Lifecycle != "missing" || len(n.Capabilities) != 0 {
		t.Fatal("missing container retained active state")
	}
}
func TestRestartedSSEHasEpochGapAndCompleteReplay(t *testing.T) {
	c := testController(t)
	for i := 0; i < 1000; i++ {
		c.Emit(Event{Type: "sample"})
	}
	ch, history := c.subscribe("old-process", 1000)
	defer c.Unsubscribe(ch)
	if len(history) != 1001 || history[0].Type != "telemetry.gap" || history[0].Epoch != c.epoch {
		t.Fatalf("incomplete replay or missing epoch gap: %d entries", len(history))
	}
	for _, e := range history[1:] {
		if e.Type != "sample" {
			t.Fatal(e)
		}
	}
	for i := 0; i < 1000; i++ {
		c.Emit(Event{Type: "live"})
	}
	for i := 0; i < 1000; i++ {
		select {
		case e := <-ch:
			if e.Type != "live" {
				t.Fatal(e)
			}
		default:
			t.Fatalf("live queue truncated at %d", i)
		}
	}
}
func TestCorruptRunAndCatalogRemainVisible(t *testing.T) {
	dir := t.TempDir()
	id := "run-corrupt"
	os.MkdirAll(filepath.Join(dir, "runs", id), 0700)
	os.WriteFile(filepath.Join(dir, "runs", id, "run.json"), []byte("{broken"), 0600)
	good := testVariants()["enbility"]
	// testVariants keys vary; take one real immutable catalog entry.
	for _, v := range testVariants() {
		good = v
		break
	}
	bad := good
	bad.ID = "../../bad"
	writeJSON(filepath.Join(dir, "variants.json"), []Variant{good, bad})
	c, err := NewController(dir, dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if c.runs[id] == nil || c.runs[id].Status != "corrupt" || len(c.catalogErrors) != 1 || len(c.variants) != 1 {
		t.Fatal("invalid state was silently hidden")
	}
	c.Close()
	saved, _ := os.ReadFile(filepath.Join(dir, "runs", id, "run.json"))
	if string(saved) != "{broken" {
		t.Fatal("corrupt evidence was overwritten")
	}
}
func TestSecurityHeadersAndEmptyHost(t *testing.T) {
	c := testController(t)
	h := c.Handler(fstest.MapFS{"index.html": {Data: []byte("arena")}})
	req := httptest.NewRequest("GET", "http://localhost/", nil)
	out := httptest.NewRecorder()
	h.ServeHTTP(out, req)
	if out.Header().Get("X-Frame-Options") != "DENY" || !strings.Contains(out.Header().Get("Content-Security-Policy"), "frame-ancestors 'none'") {
		t.Fatal("clickjacking policy missing")
	}
	req.Host = ""
	out = httptest.NewRecorder()
	h.ServeHTTP(out, req)
	if out.Code != 403 {
		t.Fatal("empty Host accepted")
	}
}
func TestCPUCountersResetWithoutNegativeUsage(t *testing.T) {
	m := dockerMetrics(map[string]any{"cpu_stats": map[string]any{"cpu_usage": map[string]any{"total_usage": 10}, "system_cpu_usage": 200, "online_cpus": 2}, "precpu_stats": map[string]any{"cpu_usage": map[string]any{"total_usage": 100}, "system_cpu_usage": 100}})
	if _, ok := m["cpuPercent"]; ok {
		t.Fatal("counter reset reported invalid CPU percentage")
	}
}

func TestStoppedWatcherDoesNotRetry(t *testing.T) {
	c := testController(t)
	r := regressionRun(t, c)
	n := r.Nodes[0]
	n.Container = "node"
	r.Status = "running"
	entered := make(chan struct{}, 1)
	c.docker = fakeDocker(t, func(w http.ResponseWriter, req *http.Request) {
		if strings.Contains(req.URL.Path, "/logs") {
			select {
			case entered <- struct{}{}:
			default:
			}
			<-req.Context().Done()
			return
		}
		io.WriteString(w, "[]")
	})
	c.mu.Lock()
	c.applyContainerLocked(r, n, "node", "running")
	c.mu.Unlock()
	<-entered
	c.mu.Lock()
	c.applyContainerLocked(r, n, "node", "exited")
	_, watching := c.watchers[r.ID+"/"+n.ID]
	c.mu.Unlock()
	if watching {
		t.Fatal("stopped node kept its watcher")
	}
	c.Close()
	for _, e := range c.events {
		if e.Type == "telemetry.disconnected" {
			t.Fatal("intentional stop produced disconnect noise")
		}
	}
}

func TestSlowDockerDropDoesNotHoldGlobalLock(t *testing.T) {
	c := testController(t)
	r := regressionRun(t, c)
	entered, release := make(chan struct{}, 1), make(chan struct{})
	c.docker = fakeDocker(t, func(w http.ResponseWriter, req *http.Request) {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-release
		io.WriteString(w, "[]")
	})
	done := make(chan error, 1)
	go func() { done <- c.Drop(r.ID) }()
	<-entered
	snapshot := make(chan struct{})
	go func() { c.Snapshot(); close(snapshot) }()
	select {
	case <-snapshot:
	case <-time.After(time.Second):
		close(release)
		t.Fatal("Docker check stalled state API")
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

type blockedResponse struct {
	header  http.Header
	entered chan struct{}
	release chan struct{}
	body    bytes.Buffer
	once    sync.Once
}

func (w *blockedResponse) Header() http.Header { return w.header }
func (w *blockedResponse) WriteHeader(int)     {}
func (w *blockedResponse) Write(data []byte) (int, error) {
	w.once.Do(func() { close(w.entered); <-w.release })
	return w.body.Write(data)
}

func TestSlowMetricsResponseDoesNotHoldGlobalLock(t *testing.T) {
	c := testController(t)
	regressionRun(t, c)
	w := &blockedResponse{header: http.Header{}, entered: make(chan struct{}), release: make(chan struct{})}
	done := make(chan struct{})
	go func() {
		defer close(done)
		c.Handler(fstest.MapFS{}).ServeHTTP(w, httptest.NewRequest("GET", "http://localhost/metrics", nil))
	}()
	<-w.entered
	snapshot := make(chan struct{})
	go func() { c.Snapshot(); close(snapshot) }()
	select {
	case <-snapshot:
	case <-time.After(time.Second):
		close(w.release)
		t.Fatal("scraper stalled state API")
	}
	close(w.release)
	<-done
}
func TestEvidencePinsSegmentsBeforeRotation(t *testing.T) {
	c := testController(t)
	r := regressionRun(t, c)
	original := make([]byte, 128<<10)
	if _, err := rand.Read(original); err != nil {
		t.Fatal(err)
	}
	// A large initial member makes the response block before reading journal members.
	os.WriteFile(c.path(r.ID, "bundle.json"), original, 0600)
	c.Emit(Event{RunID: r.ID, Type: "before-rotation"})
	before, err := os.ReadFile(c.path(r.ID, "events.ndjson"))
	if err != nil {
		t.Fatal(err)
	}
	w := &blockedResponse{header: http.Header{}, entered: make(chan struct{}), release: make(chan struct{})}
	done := make(chan struct{})
	go func() {
		defer close(done)
		c.Handler(fstest.MapFS{}).ServeHTTP(w, httptest.NewRequest("GET", "http://localhost/api/v1/runs/"+r.ID+"/evidence", nil))
	}()
	<-w.entered
	c.mu.Lock()
	c.journal[r.ID].Close()
	os.Rename(c.path(r.ID, "events.ndjson"), c.path(r.ID, "events.ndjson.1"))
	c.openJournal(r.ID)
	c.mu.Unlock()
	c.Emit(Event{RunID: r.ID, Type: "after-rotation"})
	close(w.release)
	<-done
	gz, err := gzip.NewReader(bytes.NewReader(w.body.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	defer gz.Close()
	archive := tar.NewReader(gz)
	found := false
	for {
		header, err := archive.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(archive)
		if err != nil {
			t.Fatal(err)
		}
		if header.Name == "events.ndjson" {
			found = true
			if !bytes.Equal(body, before) {
				t.Fatal("journal segment changed during rotation")
			}
		}
		if header.Name == "events.ndjson.1" {
			t.Fatal("rotation mixed new and old segment names")
		}
	}
	if !found {
		t.Fatal("journal absent from evidence archive")
	}
}

func TestCancelDuringProtocolPhasesNeverRecordsFailure(t *testing.T) {
	for _, phase := range []string{"discovery", "partners", "restart"} {
		t.Run(phase, func(t *testing.T) {
			c := testController(t)
			r := regressionRun(t, c)
			r.Status = "running"
			d := r.Definition
			d.Scenario = "lpc"
			if phase == "restart" {
				d.Scenario = "restart"
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			restartEntered := make(chan struct{}, 1)
			if phase == "restart" {
				c.docker = fakeDocker(t, func(w http.ResponseWriter, req *http.Request) {
					if req.Method == "POST" {
						restartEntered <- struct{}{}
						<-req.Context().Done()
						return
					}
					json.NewEncoder(w).Encode(map[string]any{"Config": map[string]any{"Labels": map[string]string{"org.eebus-arena.run": r.ID, "org.eebus-arena.node": r.Nodes[len(r.Nodes)-1].ID}}})
				})
			}
			for _, n := range r.Nodes {
				n.Lifecycle = "running"
				n.Container = "node"
			}
			done := make(chan struct{})
			go func() { defer close(done); c.scenario(ctx, r.ID, d) }()
			// Let readiness establish observations after the scenario began.
			time.Sleep(10 * time.Millisecond)
			for _, n := range r.Nodes {
				caps := []any{"observe.limit"}
				if n.Role == "EG" && phase == "partners" {
					caps = append(caps, "observe.partners")
				}
				c.accept(Event{RunID: r.ID, NodeID: n.ID, Incarnation: "one", Sequence: 1, Type: "capabilities", Data: map[string]any{"commands": caps}})
				peers := map[string]any{}
				if phase != "discovery" {
					for _, ski := range n.TrustedSKIs {
						peers[ski] = "connected"
					}
				}
				c.accept(Event{RunID: r.ID, NodeID: n.ID, Incarnation: "one", Sequence: 2, Type: "snapshot", Data: map[string]any{"peers": peers}})
			}
			if phase == "restart" {
				select {
				case <-restartEntered:
				case <-time.After(2 * time.Second):
					t.Fatal("restart phase not reached")
				}
			} else if phase == "partners" {
				deadline := time.Now().Add(2 * time.Second)
				reached := false
				for time.Now().Before(deadline) {
					c.mu.Lock()
					for _, a := range r.Assertions {
						if a.ID == "discovery-trust" && a.Outcome == "pass" {
							reached = true
						}
					}
					c.mu.Unlock()
					if reached {
						break
					}
					time.Sleep(10 * time.Millisecond)
				}
				if !reached {
					t.Fatal("partner phase not reached")
				}
			} else {
				time.Sleep(150 * time.Millisecond)
			}
			cancel()
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Fatal("cancellation did not stop scenario")
			}
			inconclusive := false
			for _, a := range r.Assertions {
				if a.Outcome == "fail" {
					t.Fatalf("cancellation produced protocol failure: %+v", a)
				}
				if a.Outcome == "inconclusive" {
					inconclusive = true
				}
			}
			if !inconclusive {
				t.Fatalf("no interrupted observation recorded in %s", phase)
			}
		})
	}
}

type emptyLogTransport struct{}

func (emptyLogTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("")), Header: http.Header{}}, nil
}
func TestRepeatedLogEOFReportsOneOutage(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		n := &Node{ID: "node", Container: "container"}
		c := &Controller{runs: map[string]*Run{"run": {ID: "run", Nodes: []*Node{n}}}, dirty: map[string]bool{}, subscribers: map[chan Event]bool{}, docker: &Docker{client: &http.Client{Transport: emptyLogTransport{}}}}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { defer close(done); c.watch(ctx, "run", "node", "container") }()
		time.Sleep(15 * time.Second)
		cancel()
		<-done
		count := 0
		for _, e := range c.events {
			if e.Type == "telemetry.disconnected" {
				count++
			}
		}
		if count != 1 {
			t.Fatalf("one outage produced %d disconnect events", count)
		}
	})
}

func TestRestartAcceptsCapabilitiesBeforeDockerReturns(t *testing.T) {
	c := testController(t)
	r := regressionRun(t, c)
	r.Status = "running"
	n := r.Nodes[0]
	n.Container = "node"
	n.Lifecycle = "running"
	c.accept(Event{RunID: r.ID, NodeID: n.ID, Incarnation: "old", Sequence: 1, Type: "capabilities", Data: map[string]any{"commands": []any{"snapshot"}}})
	c.docker = fakeDocker(t, func(w http.ResponseWriter, req *http.Request) {
		if req.Method == "POST" {
			c.acceptFrom(Event{RunID: r.ID, NodeID: n.ID, Incarnation: "new", Sequence: 1, Type: "capabilities", Data: map[string]any{"commands": []any{"snapshot"}}}, "node")
			w.WriteHeader(204)
			return
		}
		if strings.Contains(req.URL.Path, "/logs") {
			<-req.Context().Done()
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"State": map[string]string{"Status": "running"}, "Config": map[string]any{"Labels": map[string]string{"org.eebus-arena.run": r.ID, "org.eebus-arena.node": n.ID}}})
	})
	if err := c.Action(context.Background(), r.ID, n.ID, "restart"); err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	incarnation, capabilities := n.Incarnation, len(n.Capabilities)
	c.mu.Unlock()
	if incarnation != "new" || capabilities != 1 {
		t.Fatal("restart retired the fresh process while handling Docker response")
	}
	c.Close()
}
func TestFailedLifecycleActionRestoresCurrentProcessReadiness(t *testing.T) {
	c := testController(t)
	r := regressionRun(t, c)
	r.Status = "running"
	n := r.Nodes[0]
	n.Container = "node"
	n.Lifecycle = "running"
	c.accept(Event{RunID: r.ID, NodeID: n.ID, Incarnation: "old", Sequence: 1, Type: "capabilities", Data: map[string]any{"commands": []any{"snapshot"}}})
	c.docker = fakeDocker(t, func(w http.ResponseWriter, req *http.Request) {
		if req.Method == "POST" {
			http.Error(w, "restart rejected", 500)
			return
		}
		if strings.Contains(req.URL.Path, "/logs") {
			<-req.Context().Done()
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"State": map[string]string{"Status": "running"}, "Config": map[string]any{"Labels": map[string]string{"org.eebus-arena.run": r.ID, "org.eebus-arena.node": n.ID}}})
	})
	if err := c.Action(context.Background(), r.ID, n.ID, "restart"); err == nil {
		t.Fatal("restart error lost")
	}
	c.mu.Lock()
	incarnation, capabilities := n.Incarnation, len(n.Capabilities)
	c.mu.Unlock()
	if incarnation != "old" || capabilities != 1 {
		t.Fatal("failed restart permanently cleared live process readiness")
	}
	c.Close()
}

func TestOversizeLogsKeepOriginalTimestampAndDiscardRemainder(t *testing.T) {
	stamp := "2020-01-02T03:04:05Z "
	next := "2020-01-02T03:04:06Z "
	d := fakeDocker(t, func(w http.ResponseWriter, r *http.Request) {
		dockerFrame(w, 1, stamp+strings.Repeat("x", 600000))
		dockerFrame(w, 1, stamp+strings.Repeat("x", 600000))
		dockerFrame(w, 1, stamp+"remaining fragment\n")
		dockerFrame(w, 1, next+`{"type":"capabilities"}`+"\n")
	})
	var rows []string
	if err := d.Logs(context.Background(), "node", time.Time{}, func(_ byte, line string) { rows = append(rows, line) }); err != io.EOF {
		t.Fatal(err)
	}
	if len(rows) != 2 || !strings.HasPrefix(rows[0], stamp) || rows[1] != next+`{"type":"capabilities"}` {
		t.Fatalf("oversize line lost backlog or leaked fragments: %v", rows)
	}
}
func TestRestartOutcomeRequiresIdentityEvidence(t *testing.T) {
	for _, tc := range []struct {
		ski       any
		connected bool
		err       error
		want      string
	}{{nil, true, nil, "inconclusive"}, {"", true, nil, "inconclusive"}, {"expected", true, nil, "pass"}, {"other", true, nil, "fail"}, {"expected", false, nil, "fail"}, {"expected", true, context.Canceled, "inconclusive"}} {
		got, _ := restartOutcome(tc.connected, tc.err, tc.ski, "expected")
		if got != tc.want {
			t.Fatalf("identity %v connected=%v error=%v: %s, want %s", tc.ski, tc.connected, tc.err, got, tc.want)
		}
	}
}
func TestDurableSaveDoesNotHoldGlobalLock(t *testing.T) {
	c := testController(t)
	r := regressionRun(t, c)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	c.mu.Lock()
	c.persist = func(path string, body []byte) error {
		once.Do(func() { close(entered); <-release })
		return writeJSONBytes(path, body)
	}
	r.Status = "complete"
	c.saveLocked(r)
	c.mu.Unlock()
	<-entered
	snapshot := make(chan struct{})
	go func() { c.Snapshot(); close(snapshot) }()
	select {
	case <-snapshot:
	case <-time.After(time.Second):
		close(release)
		t.Fatal("disk sync stalled state API")
	}
	c.mu.Lock()
	r.Status = "stopped"
	c.saveLocked(r)
	c.mu.Unlock()
	close(release)
	c.Close()
	body, err := os.ReadFile(c.path(r.ID, "run.json"))
	if err != nil {
		t.Fatal(err)
	}
	var saved Run
	if err = json.Unmarshal(body, &saved); err != nil {
		t.Fatal(err)
	}
	if saved.Status != "stopped" {
		t.Fatal("older save overwrote final state")
	}
}
func TestCorruptRunCanBeArchivedAndRestoredWithoutChangingEvidence(t *testing.T) {
	for _, contents := range []string{"{broken", ""} {
		t.Run(fmt.Sprintf("bytes-%d", len(contents)), func(t *testing.T) {
			dir := t.TempDir()
			id := "run-corrupt"
			os.MkdirAll(filepath.Join(dir, "runs", id), 0700)
			if contents != "" {
				os.WriteFile(filepath.Join(dir, "runs", id, "run.json"), []byte(contents), 0600)
			}
			c, err := NewController(dir, dir, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			if err = c.Drop(id); err != nil {
				t.Fatal(err)
			}
			if err = c.Restore(id); err != nil {
				t.Fatal(err)
			}
			if c.runs[id].Status != "corrupt" {
				t.Fatal("restored corrupt state claimed readiness")
			}
			body, err := os.ReadFile(c.path(id, "run.json"))
			if contents == "" {
				if !os.IsNotExist(err) {
					t.Fatal("missing metadata was silently fabricated")
				}
			} else if err != nil || string(body) != contents {
				t.Fatal("corrupt source evidence changed")
			}
		})
	}
}
func TestDroppingStatusCannotBePersistedAndLegacyRecoveryIsUsable(t *testing.T) {
	c := testController(t)
	r := regressionRun(t, c)
	c.mu.Lock()
	r.Status = "dropping"
	c.saveLocked(r)
	c.mu.Unlock()
	c.flushSaves()
	body, _ := os.ReadFile(c.path(r.ID, "run.json"))
	var saved Run
	json.Unmarshal(body, &saved)
	if saved.Status == "dropping" {
		t.Fatal("transient drop reservation was persisted")
	}
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "runs", r.ID), 0700)
	writeJSON(filepath.Join(dir, "runs", r.ID, "run.json"), r)
	reopened, err := NewController(dir, dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if reopened.runs[r.ID].Status != "interrupted" {
		t.Fatal("legacy dropping state was stranded")
	}
}

func TestDropRejectsFailedStatePersistence(t *testing.T) {
	c := testController(t)
	r := regressionRun(t, c)
	c.mu.Lock()
	c.persist = func(string, []byte) error { return fmt.Errorf("disk full") }
	r.Status = "stopped"
	c.saveLocked(r)
	c.mu.Unlock()
	c.flushSaves()
	if err := c.Drop(r.ID); err == nil {
		t.Fatal("unsaved run was archived with obsolete metadata")
	}
	if _, err := os.Stat(c.path(r.ID, "run.json")); err != nil {
		t.Fatal("failed drop lost run")
	}
	if r.Status != "stopped" {
		t.Fatal("failed drop stranded its reservation")
	}
}

func TestFailedArenaStartRecreatesExistingProcesses(t *testing.T) {
	c := testController(t)
	r := regressionRun(t, c)
	r.Status = "failed"
	r.Definition.Scenario = "manual"
	n := r.Nodes[0]
	n.Container = "old"
	n.Incarnation = "old-process"
	n.Capabilities = []string{"snapshot"}
	bin := t.TempDir()
	args := filepath.Join(bin, "args")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$ARENA_TEST_ARGS\"\n"
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	t.Setenv("ARENA_TEST_ARGS", args)
	if err := c.Start(r.ID); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	var body []byte
	for time.Now().Before(deadline) {
		body, _ = os.ReadFile(args)
		if len(body) > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !strings.Contains(string(body), "--force-recreate\n") {
		t.Fatal("failed Start reused retired processes")
	}
	c.Close()
}
func TestLegacySSECursorReportsUnknownEpoch(t *testing.T) {
	c := testController(t)
	for i := 0; i < 10; i++ {
		c.Emit(Event{Type: "sample"})
	}
	ch, history := c.subscribe("legacy", 5)
	defer c.Unsubscribe(ch)
	if len(history) != 11 || history[0].Type != "telemetry.gap" || !strings.Contains(fmt.Sprint(history[0].Data["reason"]), "lacks controller epoch") {
		t.Fatal("numeric cursor resumed silently from wrong epoch")
	}
}

func TestRecreationChecksOwnershipBeforeInvokingCompose(t *testing.T) {
	c := testController(t)
	r := regressionRun(t, c)
	r.Status = "failed"
	r.Definition.Scenario = "manual"
	c.docker = fakeDocker(t, func(w http.ResponseWriter, req *http.Request) {
		if strings.HasSuffix(req.URL.Path, "/containers/json") {
			json.NewEncoder(w).Encode([]map[string]any{{"Id": "foreign", "Labels": map[string]string{"org.eebus-arena.node": r.Nodes[0].ID}}})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"Config": map[string]any{"Labels": map[string]string{"org.eebus-arena.run": "other-run", "org.eebus-arena.node": r.Nodes[0].ID}}})
	})
	bin := t.TempDir()
	marker := filepath.Join(bin, "invoked")
	os.WriteFile(filepath.Join(bin, "docker"), []byte("#!/bin/sh\ntouch \"$ARENA_TEST_MARKER\"\n"), 0700)
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	t.Setenv("ARENA_TEST_MARKER", marker)
	if err := c.Start(r.ID); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	finished := false
	for time.Now().Before(deadline) {
		c.mu.Lock()
		finished = r.Status == "failed"
		c.mu.Unlock()
		if finished {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !finished {
		t.Fatal("foreign-container validation did not finish")
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("Compose could recreate a foreign container")
	}
	c.Close()
}
