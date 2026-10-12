package arena

import (
	"context"
	"testing"
	"time"
)

func TestCampaignValidationAndCancellation(t *testing.T) {
	b := testBundle()
	b.Definition.Scenario = "campaign"
	b.Definition.Campaign = &Campaign{Lanes: []CampaignLane{{ID: "load", Steps: []Step{{Name: "settle", Action: "wait", Seconds: 1}}}, {ID: "recovery", After: []string{"load"}, Steps: []Step{{Name: "settle", Action: "wait", Seconds: 1}}}}}
	if _, err := Resolve(&b, testVariants()); err != nil {
		t.Fatal(err)
	}
	b.Definition.Campaign.Lanes[0].After = []string{"recovery"}
	if _, err := Resolve(&b, testVariants()); err == nil {
		t.Fatal("cyclic campaign accepted")
	}
	b.Definition.Campaign.Lanes[0].After = nil
	c := testController(t)
	run, err := c.Create(b)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { c.campaign(ctx, run, b.Definition); close(done) }()
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("campaign cancellation leaked a lane")
	}
}

func TestMutationOrderingIsPerNode(t *testing.T) {
	c := testController(t)
	release, err := c.mutation(context.Background(), "run", "node-a")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	other, err := c.mutation(context.Background(), "run", "node-b")
	if err != nil {
		t.Fatal(err)
	}
	other()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if _, err = c.mutation(ctx, "run", "node-a"); err != context.DeadlineExceeded {
		t.Fatal("same-node mutation overtook active action")
	}
}

func TestCampaignWaitsUseTheirLaneClockDomain(t *testing.T) {
	c := testController(t)
	b := testBundle()
	run, err := c.Create(b)
	if err != nil {
		t.Fatal(err)
	}
	d := b.Definition
	// Exercise the scheduler independently of build qualification. This does
	// not permit an unqualified accelerated native stack to start.
	d.TimeScale = 10
	d.Campaign = &Campaign{Lanes: []CampaignLane{
		{ID: "real", Domain: "real", Steps: []Step{{Name: "wait", Action: "wait", Seconds: 1}}},
		{ID: "protocol", Domain: "protocol", Steps: []Step{{Name: "wait", Action: "wait", Seconds: 1}}},
	}}
	started := time.Now()
	c.campaign(context.Background(), run, d)
	if time.Since(started) < 900*time.Millisecond {
		t.Fatal("real lane wait was accelerated")
	}
}
