package arena

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/EFR-GmbH/eebus-arena/internal/resourceprobe"
)

func resourceInt(value int) *int { return &value }

func TestCollectorGapDoesNotInventCPUPeak(t *testing.T) {
	c := testController(t)
	run, err := c.Create(testBundle())
	if err != nil {
		t.Fatal(err)
	}
	n := c.runs[run].Nodes[0]
	start := time.Now().UTC()
	for _, seq := range []uint64{1, 2, 7, 8} {
		sample := resourceprobe.Sample{At: start.Add(time.Duration(seq) * time.Second), IntervalSeconds: 1, Complete: true, Cgroup: map[string]any{"cpuUsageSeconds": float64(seq) * .5, "cpuMillis": float64(500)}}
		blob, _ := json.Marshal(sample)
		var data map[string]any
		_ = json.Unmarshal(blob, &data)
		c.accept(Event{RunID: run, NodeID: n.ID, At: sample.At, Type: "resource.process", Incarnation: "collector", Sequence: seq, Data: data})
		if seq == 7 && nested(n.ProcessMetrics, "cgroup", "cpuPercent") != nil {
			t.Fatal("missing intervals created a false CPU peak")
		}
	}
	if nested(n.ProcessMetrics, "cgroup", "cpuPercent") != float64(50) {
		t.Fatal("counter baseline did not recover")
	}
}

func TestResourceInheritanceAndCompose(t *testing.T) {
	b := testBundle()
	b.Definition.Groups[1].Count = 2
	b.Definition.Groups[1].Resources = &ResourceLimits{CPUMillis: resourceInt(500), MemoryMiB: resourceInt(256), PIDsLimit: resourceInt(128)}
	b.Definition.NodeOverrides = map[string]NodeOverride{"cs-002": {Resources: ResourceLimits{CPUMillis: resourceInt(1000), MemoryMiB: resourceInt(0)}}}
	nodes, err := Resolve(&b, testVariants())
	if err != nil {
		t.Fatal(err)
	}
	if *nodes[1].Resources.MemoryMiB != 256 || *nodes[2].Resources.MemoryMiB != 0 || *nodes[2].Resources.CPUMillis != 1000 || *nodes[2].Resources.PIDsLimit != 128 {
		t.Fatal("override inheritance failed")
	}
	c := testController(t)
	run, err := c.Create(b)
	if err != nil {
		t.Fatal(err)
	}
	blob, err := os.ReadFile(filepath.Join(c.data, "runs", run, "compose.json"))
	if err != nil {
		t.Fatal(err)
	}
	var compose map[string]any
	if err := json.Unmarshal(blob, &compose); err != nil {
		t.Fatal(err)
	}
	if nested(compose, "services", "cs-001", "cpus") != float64(0.5) || nested(compose, "services", "cs-001", "memswap_limit") != "256m" || nested(compose, "services", "cs-002", "mem_limit") != nil {
		t.Fatal("limits were not applied to the exact node")
	}
}

func TestResourceValidation(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*Definition)
	}{
		{"ambiguous RAM", func(d *Definition) {
			d.Groups[0].MemoryMB = 256
			d.Groups[0].Resources = &ResourceLimits{MemoryMiB: resourceInt(512)}
		}},
		{"invalid quota", func(d *Definition) { d.Groups[0].Resources = &ResourceLimits{CPUMillis: resourceInt(-1)} }},
		{"unknown override", func(d *Definition) { d.NodeOverrides = map[string]NodeOverride{"foreign": {}} }},
		{"duplicate diagnostic", func(d *Definition) {
			d.Sampling = &SamplingProfile{IntervalMillis: 1000, DiagnosticNodes: []string{"eg-001", "eg-001"}}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			b := testBundle()
			test.change(&b.Definition)
			if _, err := Resolve(&b, testVariants()); err == nil {
				t.Fatal("invalid resources accepted")
			}
		})
	}
}

func TestResourceHistoryKeepsPeaksAndNativeIncarnation(t *testing.T) {
	c := testController(t)
	b := testBundle()
	b.Definition.Groups[1].Count = 1
	run, err := c.Create(b)
	if err != nil {
		t.Fatal(err)
	}
	n := c.runs[run].Nodes[0]
	n.Incarnation = "native"
	n.Sequence = 7
	start := time.Now().UTC()
	c.runs[run].Created = start.Add(-time.Second)
	for i, value := range []float64{5, 90, 5} {
		sample := resourceprobe.Sample{At: start.Add(time.Duration(i) * time.Second), IntervalSeconds: 1, Complete: true, Processes: []resourceprobe.Process{{ID: "27:42", PID: 27, Name: "avahi-daemon", Component: "Avahi", CPUPercent: &value, RSSBytes: 4096}}}
		blob, err := json.Marshal(sample)
		if err != nil {
			t.Fatal(err)
		}
		var data map[string]any
		if err = json.Unmarshal(blob, &data); err != nil {
			t.Fatal(err)
		}
		c.accept(Event{RunID: run, NodeID: n.ID, At: sample.At, Type: "resource.process", Incarnation: "collector-one", Sequence: uint64(i + 1), Data: data})
	}
	if n.Incarnation != "native" || n.Sequence != 7 {
		t.Fatal("collector interfered with native telemetry")
	}
	history, err := c.ResourceHistory(context.Background(), run, n.ID, start.Add(-time.Second), start.Add(time.Minute), 16)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, s := range history.Series {
		if s.Component == "Avahi" && s.Metric == "cpuPercent" {
			found = true
			if s.Peak != 90 || len(s.Points) != 1 || s.Points[0].Max != 90 || s.Points[0].Samples != 3 {
				t.Fatalf("downsampling lost peak: %+v", s)
			}
		}
	}
	if !found {
		t.Fatal("no attributed series")
	}
	c.resources.close()
	c.resources = newResourceStore(c.data)
	restored, err := c.ResourceHistory(context.Background(), run, n.ID, start.Add(-time.Second), start.Add(time.Minute), 16)
	if err != nil || len(restored.Series) != len(history.Series) {
		t.Fatalf("history did not survive reopening: %v", err)
	}
}

func TestResourceRotationAndDropKeepPeaks(t *testing.T) {
	c := testController(t)
	b := testBundle()
	b.Definition.Groups[1].Count = 1
	run, err := c.Create(b)
	if err != nil {
		t.Fatal(err)
	}
	c.resources.segmentBytes = 600
	start := time.Now().UTC().Truncate(time.Minute).Add(time.Second)
	c.runs[run].Created = start.Add(-time.Second)
	for i := 0; i < 40; i++ {
		value := float64(5)
		if i == 1 {
			value = 90
		}
		c.resources.enqueue(run, ResourceRecord{At: start.Add(time.Duration(i) * time.Second), NodeID: "eg-001", Type: "process", Incarnation: "collector", Complete: true, IntervalSeconds: 1, Processes: []resourceprobe.Process{{ID: "27:42", Name: "avahi-daemon", Component: "Avahi", CPUPercent: &value, RSSBytes: 4096}}})
	}
	history, err := c.ResourceHistory(context.Background(), run, "eg-001", start.Add(-time.Second), start.Add(time.Minute), 16)
	if err != nil {
		t.Fatal(err)
	}
	if history.Status.AggregateBytes == 0 {
		t.Fatal("rotation did not preserve minute aggregates")
	}
	for _, series := range history.Series {
		if series.Metric == "cpuPercent" && series.Peak != 90 {
			t.Fatal("rotation lost Avahi CPU spike")
		}
	}
	if err = c.Drop(run); err != nil {
		t.Fatal(err)
	}
	if err = c.Restore(run); err != nil {
		t.Fatal(err)
	}
	history, err = c.ResourceHistory(context.Background(), run, "eg-001", start.Add(-time.Second), start.Add(time.Minute), 16)
	if err != nil || len(history.Series) == 0 {
		t.Fatal("dropped resource evidence was not restored", err)
	}
}

func TestResourceHistoryDoesNotCropMinuteExtrema(t *testing.T) {
	c := testController(t)
	run, err := c.Create(testBundle())
	if err != nil {
		t.Fatal(err)
	}
	minute := time.Now().UTC().Truncate(time.Minute).Add(-2 * time.Minute)
	c.runs[run].Created = minute.Add(-time.Minute)
	c.resources.enqueue(run, ResourceRecord{
		At: minute, NodeID: "eg-001", Type: "aggregate", Complete: true,
		Aggregates: []ResourceAggregate{{Name: "eg-001", Component: "Container", Source: "cgroup", Metric: "cpuPercent", Min: 5, Max: 90, Mean: 10, Last: 5, Samples: 60, Seconds: 60, Weight: 60, PeakAt: minute.Add(time.Second)}},
	})
	full, err := c.ResourceHistory(context.Background(), run, "eg-001", minute, minute.Add(time.Minute), 16)
	if err != nil || len(full.Series) != 1 || full.Series[0].Peak != 90 {
		t.Fatalf("complete minute lost peak: %+v %v", full, err)
	}
	partial, err := c.ResourceHistory(context.Background(), run, "eg-001", minute.Add(30*time.Second), minute.Add(time.Minute), 16)
	if err != nil || len(partial.Series) != 0 || len(partial.Gaps) != 1 {
		t.Fatalf("partial minute invented boundary data: %+v %v", partial, err)
	}
}
