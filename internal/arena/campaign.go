package arena

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// Campaign overlaps independently scheduled lanes with explicit dependencies.
type Campaign struct {
	Lanes []CampaignLane `json:"lanes"`
}
type CampaignLane struct {
	ID            string   `json:"id"`
	After         []string `json:"after,omitempty"`
	OffsetSeconds int      `json:"offsetSeconds,omitempty"`
	Domain        string   `json:"domain,omitempty"`
	Steps         []Step   `json:"steps"`
}

func (d *Definition) validateCampaign(variants map[string]Variant) error {
	if d.Campaign == nil {
		if d.Scenario == "campaign" {
			return errors.New("campaign requires lanes")
		}
		return nil
	}
	if d.Scenario != "campaign" || len(d.Campaign.Lanes) < 1 || len(d.Campaign.Lanes) > 8 {
		return errors.New("campaign requires 1–8 lanes and the campaign scenario")
	}
	lanes := map[string]CampaignLane{}
	total := 0
	for _, lane := range d.Campaign.Lanes {
		if !safeID.MatchString(lane.ID) {
			return errors.New("invalid campaign lane id")
		}
		if _, ok := lanes[lane.ID]; ok {
			return errors.New("duplicate campaign lane")
		}
		if lane.OffsetSeconds < 0 || lane.OffsetSeconds > 86400 || !Contains([]string{"", "real", "protocol"}, lane.Domain) {
			return errors.New("invalid campaign offset or clock domain")
		}
		copy := *d
		copy.Campaign = nil
		copy.Scenario = "custom"
		copy.Steps = lane.Steps
		if err := copy.Validate(variants); err != nil {
			return fmt.Errorf("lane %s: %w", lane.ID, err)
		}
		lanes[lane.ID] = lane
		total += len(lane.Steps)
	}
	if total > 1000 {
		return errors.New("campaign permits at most 1000 total steps")
	}
	visiting, visited := map[string]bool{}, map[string]bool{}
	var visit func(string) error
	visit = func(id string) error {
		if visiting[id] {
			return errors.New("campaign dependency cycle")
		}
		if visited[id] {
			return nil
		}
		lane, ok := lanes[id]
		if !ok {
			return errors.New("unknown campaign dependency")
		}
		visiting[id] = true
		seen := map[string]bool{}
		for _, parent := range lane.After {
			if seen[parent] {
				return errors.New("duplicate campaign dependency")
			}
			seen[parent] = true
			if err := visit(parent); err != nil {
				return err
			}
		}
		visiting[id] = false
		visited[id] = true
		return nil
	}
	for id := range lanes {
		if err := visit(id); err != nil {
			return err
		}
	}
	return nil
}

func (c *Controller) campaign(ctx context.Context, run string, d Definition) {
	type completion struct {
		done chan struct{}
		ok   bool
	}
	finished := map[string]*completion{}
	for _, lane := range d.Campaign.Lanes {
		finished[lane.ID] = &completion{done: make(chan struct{})}
	}
	start := time.Now()
	var workers sync.WaitGroup
	for _, lane := range d.Campaign.Lanes {
		workers.Add(1)
		go func(lane CampaignLane) {
			defer workers.Done()
			result := finished[lane.ID]
			defer close(result.done)
			for _, dependency := range lane.After {
				select {
				case <-ctx.Done():
					return
				case <-finished[dependency].done:
				}
				if !finished[dependency].ok {
					c.assert(run, "", lane.ID, "inconclusive", "Prerequisite lane "+dependency+" did not pass", "")
					return
				}
			}
			scale := 1
			if lane.Domain == "protocol" {
				scale = d.TimeScale
			}
			deadline := start.Add(time.Duration(lane.OffsetSeconds) * time.Second / time.Duration(scale))
			timer := time.NewTimer(time.Until(deadline))
			defer timer.Stop()
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
			}
			c.Emit(Event{RunID: run, Type: "campaign.lane.started", Data: map[string]any{"lane": lane.ID, "domain": lane.Domain, "offsetSeconds": lane.OffsetSeconds, "latenessMillis": time.Since(deadline).Seconds() * 1000}})
			copy := d
			copy.Campaign = nil
			copy.TimeScale = scale
			copy.Steps = append([]Step(nil), lane.Steps...)
			for i := range copy.Steps {
				copy.Steps[i].Name = lane.ID + "/" + copy.Steps[i].Name
			}
			c.steps(ctx, run, copy)
			result.ok = ctx.Err() == nil
			c.mu.Lock()
			if r := c.runs[run]; r != nil {
				for _, a := range r.Assertions {
					if strings.HasPrefix(a.ID, lane.ID+"/") && a.Outcome != "pass" {
						result.ok = false
					}
				}
			} else {
				result.ok = false
			}
			c.mu.Unlock()
			c.Emit(Event{RunID: run, Type: "campaign.lane.finished", Data: map[string]any{"lane": lane.ID, "passed": result.ok}})
		}(lane)
	}
	workers.Wait()
}

// mutation serializes native commands and lifecycle changes to the same node.
func (c *Controller) mutation(ctx context.Context, run, node string) (func(), error) {
	key := run + "/" + node
	c.mutationMu.Lock()
	if c.mutations == nil {
		c.mutations = map[string]chan struct{}{}
	}
	gate := c.mutations[key]
	if gate == nil {
		gate = make(chan struct{}, 1)
		c.mutations[key] = gate
	}
	c.mutationMu.Unlock()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.ctx.Done():
		return nil, errors.New("controller closed")
	case gate <- struct{}{}:
		return func() { <-gate }, nil
	}
}
