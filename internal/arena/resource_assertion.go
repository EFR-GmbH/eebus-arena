package arena

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"
)

// ResourceExpectation budgets a sampled metric in an explicit real-time window.
type ResourceExpectation struct {
	Metric           string  `json:"metric"`
	Component        string  `json:"component,omitempty"`
	Max              float64 `json:"max"`
	WindowSeconds    int     `json:"windowSeconds"`
	SustainedSeconds int     `json:"sustainedSeconds,omitempty"`
	Growth           bool    `json:"growth,omitempty"`
}

func (e ResourceExpectation) Validate() error {
	if !Contains([]string{"cpuPercent", "rssBytes", "workingSetBytes", "memoryBytes", "oomKills", "cpuThrottledSeconds"}, e.Metric) || math.IsNaN(e.Max) || math.IsInf(e.Max, 0) || e.Max < 0 || e.WindowSeconds < 2 || e.WindowSeconds > 600 || e.SustainedSeconds < 0 || e.SustainedSeconds > e.WindowSeconds {
		return errors.New("invalid resource expectation")
	}
	if e.Growth && !Contains([]string{"rssBytes", "workingSetBytes", "memoryBytes"}, e.Metric) {
		return errors.New("growth applies only to memory metrics")
	}
	return nil
}
func (c *Controller) resourceAssertion(ctx context.Context, run, node, name string, e ResourceExpectation) {
	start := time.Now().UTC()
	timer := time.NewTimer(time.Duration(e.WindowSeconds) * time.Second)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		c.assert(run, node, name, "inconclusive", "Resource observation interrupted", "")
		return
	case <-timer.C:
	}
	end := time.Now().UTC()
	type metricReading struct {
		first, last, peak, above, coverage float64
		at                                 time.Time
		initialized                        bool
		baseline                           bool
	}
	readings := map[string]*metricReading{}
	gap := false
	violated := false
	count := 0
	status, err := c.resources.read(ctx, run, func(record ResourceRecord) {
		if record.NodeID != node || record.At.After(end) {
			return
		}
		if !record.At.Before(start) && (!record.Complete || record.Type == "gap" || record.Type == "aggregate") {
			gap = true
		}
		recordMetrics(record, func(process, _, component, source, metric string, value float64) {
			if metric != e.Metric {
				return
			}
			if e.Component != "" && component != e.Component {
				return
			}
			if e.Component == "" && process != "" {
				return
			}
			// Prefer local cgroup counters; Docker provides fallback only for images
			// without a supervisor. Mixing sources would duplicate coverage.
			id := record.Container + "/" + process + "/" + source
			reading := readings[id]
			if reading == nil {
				reading = &metricReading{}
				readings[id] = reading
			}
			if record.At.Before(start) {
				if start.Sub(record.At) <= 10*time.Second && record.Complete {
					reading.first = value
					reading.initialized = true
					reading.baseline = true
				}
				return
			}
			if record.IntervalSeconds <= 0 {
				gap = true
				return
			}
			interval := math.Min(record.IntervalSeconds, record.At.Sub(start).Seconds())
			if !reading.initialized {
				reading.first = value
				reading.initialized = true
			}
			if !reading.at.IsZero() && record.At.Sub(reading.at).Seconds() > record.IntervalSeconds*1.5 {
				reading.above = 0
				gap = true
			}
			reading.last = value
			reading.peak = math.Max(reading.peak, value)
			reading.coverage += interval
			reading.at = record.At
			count++
			budget := value
			if e.Growth {
				if !reading.baseline {
					gap = true
				}
				budget = value - reading.first
			}
			if e.Metric == "oomKills" || e.Metric == "cpuThrottledSeconds" {
				if !reading.baseline {
					gap = true
				}
				budget = value - reading.first
			}
			if budget < 0 && (e.Metric == "oomKills" || e.Metric == "cpuThrottledSeconds") {
				gap = true
				return
			}
			if budget > e.Max {
				reading.above += interval
				if e.SustainedSeconds == 0 || reading.above >= float64(e.SustainedSeconds) {
					violated = true
				}
			} else {
				reading.above = 0
			}
		})
	})
	coverage := 0.0
	for _, reading := range readings {
		coverage = math.Max(coverage, reading.coverage)
	}
	outcome := "pass"
	if violated {
		outcome = "fail"
	} else if err != nil || gap || count < 2 || coverage < float64(e.WindowSeconds)*0.9 || status.QueueDrops > 0 || status.WriteErrors > 0 || status.RetentionDrops > 0 {
		outcome = "inconclusive"
	}
	c.assert(run, node, name, outcome, fmt.Sprintf("%s %s budget %g; %d readings, %.2f of %d real seconds covered; sustained threshold %d s; %v", e.Component, e.Metric, e.Max, count, coverage, e.WindowSeconds, e.SustainedSeconds, err), "")
}
