package arena

import (
	"strings"
	"testing"
)

func TestReliableClockRejectsBroadLegacyCapability(t *testing.T) {
	c := testController(t)
	b := testBundle()
	b.SchemaVersion = 3
	b.Definition.SchemaVersion = 3
	b.Definition.TimeScale = 10
	run, err := c.Create(b)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range c.runs[run].Nodes {
		n.Capabilities = []string{"timing.accelerated"}
	}
	if err = c.Start(run); err == nil || !strings.Contains(err.Error(), "choose 1×") {
		t.Fatal("partial clock accepted as reliable", err)
	}
}
func TestClockQualificationRequiresAllAreas(t *testing.T) {
	q := ClockQualification{Version: 1, Factors: []int{2, 5, 10}, Profiles: []string{"heartbeat"}, Coverage: append([]string{}, clockAreas...), Evidence: "qualified-1x-comparison", MaxDriftMillis: 20}
	if !clockQualified(q, 5, "heartbeat") {
		t.Fatal("complete qualification rejected")
	}
	q.Coverage = q.Coverage[:len(q.Coverage)-1]
	if clockQualified(q, 5, "heartbeat") {
		t.Fatal("partial dependency clock accepted")
	}
}
