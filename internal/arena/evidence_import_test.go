package arena

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"
)

func TestEvidenceArchiveReopensPeaksWithoutControl(t *testing.T) {
	c := testController(t)
	run, err := c.Create(testBundle())
	if err != nil {
		t.Fatal(err)
	}
	node := c.runs[run].Nodes[0].ID
	start := time.Now().UTC()
	c.runs[run].Created = start.Add(-time.Second)
	c.resources.enqueue(run, ResourceRecord{At: start, NodeID: node, Type: "process", IntervalSeconds: 1, Complete: true, Metrics: map[string]any{"cpuPercent": float64(90)}})
	w := httptest.NewRecorder()
	c.Handler(nil).ServeHTTP(w, httptest.NewRequest("GET", "http://localhost/api/v1/runs/"+run+"/evidence", nil))
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	id, err := c.ImportEvidence(context.Background(), bytes.NewReader(w.Body.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	history, err := c.ResourceHistory(context.Background(), id, node, start.Add(-time.Second), start.Add(time.Second), 16)
	if err != nil || len(history.Series) != 1 || history.Series[0].Peak != 90 {
		t.Fatalf("archive peak lost: %+v %v", history, err)
	}
	if c.Start(id) == nil || c.Down(id) == nil {
		t.Fatal("evidence archive permitted live control")
	}
	if err = c.Drop(id); err != nil {
		t.Fatal(err)
	}
	if err = c.Restore(id); err != nil {
		t.Fatal("dropped evidence archive cannot be restored", err)
	}
	history, err = c.ResourceHistory(context.Background(), id, node, start.Add(-time.Second), start.Add(time.Second), 16)
	if err != nil || len(history.Series) != 1 || history.Series[0].Peak != 90 {
		t.Fatal("restoring dropped archive lost retained peak", err)
	}
	if c.runs[id].Status != "archived" || c.Start(id) == nil || c.Down(id) == nil {
		t.Fatal("restoring evidence archive enabled live control")
	}
}

func TestEvidenceRejectsTraversalAndDuplicateSegments(t *testing.T) {
	c := testController(t)
	for _, name := range []string{"../run.json", "/run.json", "resources.ndjson"} {
		var body bytes.Buffer
		gz := gzip.NewWriter(&body)
		tarw := tar.NewWriter(gz)
		for i := 0; i < 2; i++ {
			_ = tarw.WriteHeader(&tar.Header{Name: name, Mode: 0600, Size: 2})
			_, _ = tarw.Write([]byte("{}"))
		}
		_ = tarw.Close()
		_ = gz.Close()
		if _, err := c.ImportEvidence(context.Background(), &body); err == nil {
			t.Fatalf("unsafe archive accepted: %s", name)
		}
	}
}

func TestEvidenceRejectsCorruptGzipTrailer(t *testing.T) {
	c := testController(t)
	run, err := c.Create(testBundle())
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	c.Handler(nil).ServeHTTP(w, httptest.NewRequest("GET", "http://localhost/api/v1/runs/"+run+"/evidence", nil))
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	archive := append([]byte(nil), w.Body.Bytes()...)
	archive[len(archive)-8] ^= 1 // Damage CRC after the tar end marker.
	if _, err = c.ImportEvidence(context.Background(), bytes.NewReader(archive)); err == nil {
		t.Fatal("archive with a corrupt gzip checksum was accepted")
	}
	if len(c.runs) != 1 {
		t.Fatal("failed import left a visible run")
	}
}

func TestEvidenceHTTPAcceptsGzipAndPreservesOriginChecks(t *testing.T) {
	c := testController(t)
	run, err := c.Create(testBundle())
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	c.Handler(nil).ServeHTTP(w, httptest.NewRequest("GET", "http://localhost/api/v1/runs/"+run+"/evidence", nil))
	archive := append([]byte(nil), w.Body.Bytes()...)
	for _, test := range []struct {
		name, origin, contentType string
		status                    int
	}{
		{"same origin gzip", "http://localhost", "application/gzip", 201},
		{"cross origin gzip", "http://foreign.test", "application/gzip", 403},
		{"json is not gzip", "http://localhost", "application/json", 415},
		{"invalid media type", "http://localhost", "application/gzipgarbage", 415},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest("POST", "http://localhost/api/v1/evidence", bytes.NewReader(archive))
			request.Header.Set("Content-Type", test.contentType)
			request.Header.Set("Origin", test.origin)
			response := httptest.NewRecorder()
			c.Handler(nil).ServeHTTP(response, request)
			if response.Code != test.status {
				t.Fatalf("status %d, expected %d: %s", response.Code, test.status, response.Body.String())
			}
		})
	}
}

func TestResourceBudgetUsesPreWindowCounterBaseline(t *testing.T) {
	t.Parallel()
	c := testController(t)
	run, err := c.Create(testBundle())
	if err != nil {
		t.Fatal(err)
	}
	node := c.runs[run].Nodes[0].ID
	start := time.Now().UTC()
	for i, value := range []float64{5, 6, 6} {
		at := start.Add(time.Duration(i)*time.Second - time.Millisecond)
		c.resources.enqueue(run, ResourceRecord{At: at, NodeID: node, Type: "process", IntervalSeconds: 1, Complete: true, Metrics: map[string]any{"oomKills": value}})
	}
	c.resourceAssertion(context.Background(), run, node, "oom-budget", ResourceExpectation{Metric: "oomKills", Max: 0, WindowSeconds: 2})
	c.mu.Lock()
	defer c.mu.Unlock()
	assertions := c.runs[run].Assertions
	if len(assertions) != 1 || assertions[0].Outcome != "fail" {
		blob, _ := json.Marshal(assertions)
		t.Fatalf("observed OOM inside window was missed: %s", blob)
	}
}
