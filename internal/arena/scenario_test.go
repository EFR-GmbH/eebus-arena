package arena

import (
	"context"
	"testing"
	"testing/synctest"
	"time"
)

func TestHeartbeatRequiresFreshObservedTransition(t *testing.T) {
	issued := time.Now()
	node := &Node{ID: "cs", Lifecycle: "running", SnapshotAt: issued.Add(time.Second), Snapshot: map[string]any{"lpc": map[string]any{"state": "FAILSAFE"}}}
	run := &Run{Nodes: []*Node{node}}
	if observedLPCState(run, "cs", issued, "CONTROLLED", "LIMITED") {
		t.Fatal("an already failsafe receiver cannot be a positive control")
	}
	if !observedLPCState(run, "cs", issued, "FAILSAFE") {
		t.Fatal("fresh native failsafe observation was lost")
	}
	node.SnapshotAt = issued.Add(-time.Second)
	if observedLPCState(run, "cs", issued, "FAILSAFE") {
		t.Fatal("stale state satisfied the heartbeat assertion")
	}
	node.SnapshotAt = issued.Add(time.Second)
	node.Lifecycle = "exited"
	if observedLPCState(run, "cs", issued, "FAILSAFE") {
		t.Fatal("a stopped receiver satisfied the heartbeat assertion")
	}
}

func TestHeartbeatSuppressionRequiresPositiveControl(t *testing.T) {
	for _, tc := range []struct {
		name     string
		states   []string
		stale    bool
		eligible bool
	}{
		{name: "all failed", states: []string{"FAILSAFE", "AUTONOMOUS"}},
		{name: "stale controlled", states: []string{"CONTROLLED"}, stale: true},
		{name: "one passed", states: []string{"FAILSAFE", "CONTROLLED"}, eligible: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				c := testController(t)
				b := testBundle()
				b.Definition.Groups[1].Count = len(tc.states)
				run, err := c.Create(b)
				if err != nil {
					t.Fatal(err)
				}
				baselineAt := time.Now()
				nodes := []Node{}
				for i, n := range c.runs[run].Nodes {
					if n.Role == "CS" {
						n.Lifecycle = "running"
						n.Capabilities = []string{"observe.state"}
						n.Snapshot = map[string]any{"lpc": map[string]any{"state": tc.states[i-1]}}
						n.SnapshotAt = baselineAt
						if tc.stale {
							n.SnapshotAt = baselineAt.Add(-time.Second)
						}
					}
					nodes = append(nodes, *n)
				}
				c.heartbeatScenario(context.Background(), run, Definition{TimeScale: 10}, nodes, baselineAt)
				controls, suppressions, restorations := 0, 0, 0
				for _, a := range c.runs[run].Assertions {
					switch a.ID {
					case "heartbeat-positive-control":
						controls++
					case "heartbeat-suppress":
						suppressions++
					case "heartbeat-resume":
						restorations++
					}
				}
				if controls != len(tc.states) {
					t.Fatalf("got %d positive controls, want %d", controls, len(tc.states))
				}
				// The EG has no heartbeat capability, so attempted commands leave assertions.
				want := 0
				if tc.eligible {
					want = 1
				}
				if suppressions != want || restorations != want {
					t.Fatalf("got %d suppression and %d restoration attempts, want %d each", suppressions, restorations, want)
				}
			})
		})
	}
}
