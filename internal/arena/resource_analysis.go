package arena

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"sort"
	"time"

	"github.com/EFR-GmbH/eebus-arena/internal/resourceprobe"
)

func (c *Controller) acceptResourcesLocked(r *Run, n *Node, e Event) {
	if !e.At.IsZero() && e.At.Before(n.ResourceAt) || e.Incarnation == "" || e.Sequence == 0 {
		return
	}
	if n.ResourceIncarnation == e.Incarnation && e.Sequence <= n.ResourceSequence {
		return
	}
	gap := n.ResourceIncarnation == e.Incarnation && e.Sequence != n.ResourceSequence+1
	previous := n.ProcessMetrics
	if n.ResourceIncarnation != e.Incarnation {
		previous = nil
	}
	if e.Type != "resource.process" {
		n.ResourceIncarnation = e.Incarnation
		n.ResourceSequence = e.Sequence
		n.ResourceAt = e.At
		c.emitLocked(e)
		if c.resources != nil {
			c.resources.enqueue(r.ID, ResourceRecord{At: e.At, NodeID: n.ID, Container: n.Container, Type: "gap", Error: fmtResourceError(e.Data)})
		}
		return
	}
	blob, err := json.Marshal(e.Data)
	if err != nil {
		return
	}
	var sample resourceprobe.Sample
	if json.Unmarshal(blob, &sample) != nil || sample.At.IsZero() || sample.IntervalSeconds < 0 || len(sample.Processes) > 256 {
		c.emitLocked(Event{RunID: r.ID, NodeID: n.ID, Type: "resource.gap", Data: map[string]any{"reason": "invalid resource sample"}})
		if c.resources != nil {
			c.resources.enqueue(r.ID, ResourceRecord{At: time.Now().UTC(), NodeID: n.ID, Type: "gap", Error: "invalid resource sample"})
		}
		return
	}
	n.ResourceIncarnation = e.Incarnation
	n.ResourceSequence = e.Sequence
	n.ResourceAt = e.At
	if n.External != nil && e.SourceAt != nil && !e.SourceAt.IsZero() {
		sample.DeviceAt = e.SourceAt
		offset := e.At.Sub(*e.SourceAt).Seconds()
		sample.ClockOffsetSeconds = &offset
	}
	if gap {
		sample.Complete = false
		c.emitLocked(Event{RunID: r.ID, NodeID: n.ID, Type: "resource.gap", Data: map[string]any{"reason": "collector sequence gap"}})
	}
	if sample.Cgroup != nil && previous != nil && sample.IntervalSeconds > 0 && !gap {
		current, hasCurrent := sample.Cgroup["cpuUsageSeconds"].(float64)
		old, hasOld := nested(previous, "cgroup", "cpuUsageSeconds").(float64)
		if hasCurrent && hasOld && current >= old {
			sample.Cgroup["cpuPercent"] = (current - old) / sample.IntervalSeconds * 100
		}
		quota, _ := sample.Cgroup["cpuMillis"].(float64)
		if affinity, ok := sample.Cgroup["cpuAffinityMillis"].(float64); ok && affinity > 0 && (quota <= 0 || affinity < quota) {
			quota = affinity
		}
		if quota > 0 && sample.Cgroup["cpuPercent"] != nil {
			if affinity, ok := sample.Cgroup["cpuAffinityMillis"].(float64); ok && affinity > 0 && affinity < quota {
				quota = affinity
			}
			sample.Cgroup["cpuAllowancePercent"] = num(sample.Cgroup["cpuPercent"]) / (quota / 1000)
		}
		oldOOM, hasOld := nested(previous, "cgroup", "oomKills").(float64)
		if currentOOM, ok := sample.Cgroup["oomKills"].(float64); ok && hasOld && currentOOM > oldOOM {
			c.emitLocked(Event{RunID: r.ID, NodeID: n.ID, Type: "resource.oom", Data: map[string]any{"kills": currentOOM - oldOOM}})
		}
	}
	// Decode the derived values into a detached map; native snapshots are independent.
	encoded, err := json.Marshal(sample)
	if err != nil {
		return
	}
	var data map[string]any
	if json.Unmarshal(encoded, &data) != nil {
		return
	}
	n.ProcessMetrics = data
	if c.resources != nil {
		c.resources.enqueue(r.ID, ResourceRecord{At: sample.At, SourceAt: sample.DeviceAt, Scope: sample.Scope, NodeID: n.ID, Container: n.Container, Incarnation: e.Incarnation, Type: "process", IntervalSeconds: sample.IntervalSeconds, Processes: sample.Processes, Metrics: sample.Cgroup, Complete: sample.Complete})
	}
	c.dirty[r.ID] = true
}

func fmtResourceError(data map[string]any) string {
	if text, ok := data["error"].(string); ok {
		return text
	}
	return "resource collection unavailable"
}

// ResourcePoint retains sampled peaks at every chart resolution.
type ResourcePoint struct {
	At      time.Time `json:"at"`
	Min     float64   `json:"min"`
	Max     float64   `json:"max"`
	Mean    float64   `json:"mean"`
	Last    float64   `json:"last"`
	Samples int       `json:"samples"`
	Seconds float64   `json:"seconds"`
	sum     float64
	weight  float64
}

// ResourceSeries is one metric for a container or process incarnation.
type ResourceSeries struct {
	ID        string          `json:"id"`
	NodeID    string          `json:"nodeId"`
	ProcessID string          `json:"processId,omitempty"`
	Name      string          `json:"name"`
	Component string          `json:"component"`
	Metric    string          `json:"metric"`
	Source    string          `json:"source"`
	Peak      float64         `json:"peak"`
	PeakAt    time.Time       `json:"peakAt"`
	Points    []ResourcePoint `json:"points"`
	buckets   map[int64]*ResourcePoint
}

// ResourceHistory carries retained series and explicit coverage gaps.
type ResourceHistory struct {
	From          time.Time         `json:"from"`
	To            time.Time         `json:"to"`
	BucketSeconds float64           `json:"bucketSeconds"`
	Status        ResourceStatus    `json:"status"`
	Gaps          []ResourceRecord  `json:"gaps"`
	Series        []*ResourceSeries `json:"series"`
	Truncated     bool              `json:"truncated"`
}

// ResourceHistory reads retained data without holding the controller lock.
func (c *Controller) ResourceHistory(ctx context.Context, run, node string, from, to time.Time, points int) (ResourceHistory, error) {
	c.mu.Lock()
	r := c.runs[run]
	if r == nil {
		c.mu.Unlock()
		return ResourceHistory{}, errors.New("run not found")
	}
	if node != "" {
		if _, _, err := c.nodeLocked(run, node); err != nil {
			c.mu.Unlock()
			return ResourceHistory{}, err
		}
	}
	if from.IsZero() {
		from = r.Created
	}
	status := r.Status
	c.mu.Unlock()
	c.resources.mu.Lock()
	coverage := c.resources.status[run]
	c.resources.mu.Unlock()
	if to.IsZero() {
		to = time.Now().UTC()
		if Contains([]string{"stopped", "created", "archived"}, status) && !coverage.LastAt.IsZero() {
			to = coverage.LastAt.Add(time.Second)
		}
	}
	if from.IsZero() {
		from = to.Add(-24 * time.Hour)
	}
	if !to.After(from) || to.Sub(from) > 365*24*time.Hour {
		return ResourceHistory{}, errors.New("resource interval must be positive and at most one year")
	}
	if points < 16 || points > 2000 {
		return ResourceHistory{}, errors.New("points must be 16–2000")
	}
	width := to.Sub(from) / time.Duration(points)
	if width < 250*time.Millisecond {
		width = 250 * time.Millisecond
	}
	if coverage.AggregateBytes > 0 && width < time.Minute {
		width = time.Minute
	}
	result := ResourceHistory{From: from, To: to, BucketSeconds: width.Seconds(), Series: []*ResourceSeries{}, Gaps: []ResourceRecord{}}
	series := map[string]*ResourceSeries{}
	pointCount := 0
	add := func(record ResourceRecord, process, name, component, source, metric string, value float64) {
		if math.IsNaN(value) || math.IsInf(value, 0) {
			return
		}
		id := record.NodeID + "/" + record.Container + "/" + process + "/" + source + "/" + metric
		s := series[id]
		if s == nil {
			if len(series) >= 4096 {
				result.Truncated = true
				return
			}
			s = &ResourceSeries{ID: id, NodeID: record.NodeID, ProcessID: process, Name: name, Component: component, Metric: metric, Source: source, Peak: value, PeakAt: record.At, Points: []ResourcePoint{}, buckets: map[int64]*ResourcePoint{}}
			series[id] = s
		}
		if value > s.Peak {
			s.Peak = value
			s.PeakAt = record.At
		}
		index := record.At.Sub(from).Nanoseconds() / width.Nanoseconds()
		p := s.buckets[index]
		if p == nil {
			if pointCount >= 200000 {
				result.Truncated = true
				return
			}
			p = &ResourcePoint{At: from.Add(time.Duration(index) * width), Min: value, Max: value}
			s.buckets[index] = p
			pointCount++
		}
		if value < p.Min {
			p.Min = value
		}
		if value > p.Max {
			p.Max = value
		}
		weight := record.IntervalSeconds
		if weight <= 0 {
			weight = 1
		}
		p.Last = value
		p.Samples++
		p.Seconds += record.IntervalSeconds
		p.weight += weight
		p.sum += value * weight
		p.Mean = p.sum / p.weight
	}
	retention, err := c.resources.read(ctx, run, func(record ResourceRecord) {
		if record.Type == "checkpoint" {
			return
		}
		if record.Type == "aggregate" && (node == "" || record.NodeID == node) && record.At.Before(to) && record.At.Add(time.Minute).After(from) && (record.At.Before(from) || record.At.Add(time.Minute).After(to)) {
			// Minute extrema cannot be cropped honestly into a partial minute.
			// Expose missing boundary coverage rather than importing a peak from
			// outside the requested window or silently losing it.
			at := record.At
			if at.Before(from) {
				at = from
			}
			if len(result.Gaps) < 2000 {
				result.Gaps = append(result.Gaps, ResourceRecord{At: at, NodeID: record.NodeID, Type: "gap", Error: "partial minute aggregate lies outside the requested boundary"})
			} else {
				result.Truncated = true
			}
			return
		}
		if record.At.Before(from) || record.At.After(to) || node != "" && record.NodeID != node {
			return
		}
		if !record.Complete || record.Type == "gap" {
			if len(result.Gaps) < 2000 {
				gap := record
				gap.Metrics = nil
				gap.Processes = nil
				result.Gaps = append(result.Gaps, gap)
			}
			if len(result.Gaps) == 2000 {
				result.Truncated = true
			}
		}
		recordMetrics(record, func(process, name, component, source, metric string, value float64) {
			if process == "" || node != "" {
				add(record, process, name, component, source, metric, value)
			}
		})
		for _, a := range record.Aggregates {
			if a.ProcessID != "" && node == "" {
				continue
			}
			id := record.NodeID + "/" + record.Container + "/" + a.ProcessID + "/" + a.Source + "/" + a.Metric
			index := record.At.Sub(from).Nanoseconds() / width.Nanoseconds()
			var old ResourcePoint
			if s := series[id]; s != nil {
				if p := s.buckets[index]; p != nil {
					old = *p
				}
			}
			add(record, a.ProcessID, a.Name, a.Component, a.Source, a.Metric, a.Max)
			s := series[id]
			if s == nil {
				continue
			}
			p := s.buckets[index]
			if p == nil {
				continue
			}
			if old.Samples == 0 {
				p.Min = a.Min
				p.Max = a.Max
			} else {
				p.Min = math.Min(old.Min, a.Min)
				p.Max = math.Max(old.Max, a.Max)
			}
			p.Samples = old.Samples + a.Samples
			p.Seconds = old.Seconds + a.Seconds
			p.sum = old.sum + a.Mean*a.Weight
			p.weight = old.weight + a.Weight
			if p.weight > 0 {
				p.Mean = p.sum / p.weight
			}
			p.Last = a.Last
			if a.Max >= s.Peak {
				s.PeakAt = a.PeakAt
			}
		}
	})
	if err != nil {
		return result, err
	}
	result.Status = retention
	for _, s := range series {
		keys := make([]int64, 0, len(s.buckets))
		for key := range s.buckets {
			keys = append(keys, key)
		}
		sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
		for _, key := range keys {
			s.Points = append(s.Points, *s.buckets[key])
		}
		s.buckets = nil
		result.Series = append(result.Series, s)
	}
	sort.Slice(result.Series, func(i, j int) bool { return result.Series[i].ID < result.Series[j].ID })
	return result, nil
}
