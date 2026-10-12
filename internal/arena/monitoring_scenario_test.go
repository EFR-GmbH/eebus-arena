package arena

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestMonitoringInputsChangeAcrossReceiversAndReruns(t *testing.T) {
	c := testController(t)
	run, err := c.Create(testBundle())
	if err != nil {
		t.Fatal(err)
	}
	seen := map[float64]bool{}
	for repeat := 0; repeat < 3; repeat++ {
		for _, nominal := range []float64{1234, -321, 4567, -890} {
			value, err := c.nextMonitoringValue(run, nominal, nil)
			if err != nil || seen[value] || math.Signbit(value) != math.Signbit(nominal) {
				t.Fatal("reused or invalid input", value, err)
			}
			seen[value] = true
		}
	}
	// A manually published value that collides with the next counter is skipped.
	value, err := c.nextMonitoringValue(run, 1234, map[string]any{"value": 1247.0})
	if err != nil || value != 1248 {
		t.Fatal("cached manual value reused", value, err)
	}
	blob, err := json.Marshal(c.runs[run])
	if err != nil {
		t.Fatal(err)
	}
	var restored Run
	if err := json.Unmarshal(blob, &restored); err != nil {
		t.Fatal(err)
	}
	c.runs[run] = &restored
	value, err = c.nextMonitoringValue(run, 1234, nil)
	if err != nil || value != 1249 {
		t.Fatal("persisted counter was reset", value, err)
	}
}

func TestMonitoringScenarioCannotPassFromFreshSnapshotsOfCachedValues(t *testing.T) {
	c := testController(t)
	b := testBundle()
	b.Definition.Groups[1].Count = 1
	run, err := c.Create(b)
	if err != nil {
		t.Fatal(err)
	}
	provider, receiver := c.runs[run].Nodes[0], c.runs[run].Nodes[1]
	provider.Monitoring = "mgcp"
	values := map[string]any{}
	for metric, value := range map[string]float64{"power": 1234, "energyConsumed": 4567, "energyProduced": -890} {
		values[metric] = map[string]any{"available": true, "source": "native", "value": value}
	}
	monitoring := map[string]any{"remote": map[string]any{provider.SKI: map[string]any{"mgcp": values}}}
	var published atomic.Bool
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var command Command
		if err := json.NewDecoder(r.Body).Decode(&command); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		node := r.URL.Query().Get("node")
		if command.Command == "measurement.set" {
			published.Store(true)
			// A successful provider ACK and newer snapshot must not prove delivery.
			c.accept(Event{RunID: run, NodeID: receiver.ID, Type: "snapshot", Incarnation: "native", Sequence: 1, Data: map[string]any{"monitoring": monitoring}})
		}
		c.Emit(Event{RunID: run, NodeID: node, Type: "command.result", Incarnation: "native", CommandID: command.ID, Data: map[string]any{"ok": true, "monitoring": monitoring}})
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	for _, node := range c.runs[run].Nodes {
		node.External = pinnedTestAgent(server)
		node.Incarnation = "native"
		node.Lifecycle = "running"
		node.Capabilities = []string{"measurement.set", "measurement.read"}
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	c.monitoringScenario(ctx, run, []Node{*provider, *receiver})
	assertions := c.runs[run].Assertions
	if !published.Load() || len(assertions) != 1 || assertions[0].Outcome == "pass" {
		t.Fatal("cached readings passed as new delivery", assertions)
	}
}
