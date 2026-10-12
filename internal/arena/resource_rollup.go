package arena

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"
)

func recordMetrics(record ResourceRecord, consume func(process, name, component, source, metric string, value float64)) {
	source, component := "docker", "Container"
	if record.Type == "process" {
		source = "cgroup"
	}
	if record.Scope == "device" {
		source = "device"
		component = "Device"
	}
	for _, metric := range []string{"cpuPercent", "cpuAllowancePercent", "memoryBytes", "workingSetBytes", "cacheBytes", "anonymousBytes", "cpuThrottledSeconds", "cpuThrottledPeriods", "oomKills", "rxBytes", "txBytes", "deviceUsedBytes", "cpuPressure_some_avg10", "memoryPressure_some_avg10", "memoryPressure_full_avg10", "ioPressure_some_avg10"} {
		if value, ok := record.Metrics[metric].(float64); ok {
			consume("", record.NodeID, component, source, metric, value)
		}
	}
	for _, process := range record.Processes {
		id := record.Incarnation + "/" + process.ID
		consume(id, process.Name, process.Component, "proc", "rssBytes", float64(process.RSSBytes))
		if process.CPUPercent != nil {
			consume(id, process.Name, process.Component, "proc", "cpuPercent", *process.CPUPercent)
		}
	}
}

// compact archives discarded raw segments into durable minute aggregates.
// A failed compaction prevents deletion; retention loss is never silently accepted.
func (s *resourceStore) compact(run, path string) error {
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer file.Close()
	type minuteBucket struct {
		record ResourceRecord
		values map[string]*ResourceAggregate
	}
	buckets := map[string]*minuteBucket{}
	hash := sha256.New()
	scanner := bufio.NewScanner(io.TeeReader(file, hash))
	scanner.Buffer(make([]byte, 64<<10), 1<<20)
	for scanner.Scan() {
		var record ResourceRecord
		if err = json.Unmarshal(scanner.Bytes(), &record); err != nil {
			return err
		}
		at := record.At.Truncate(time.Minute)
		key := at.Format(time.RFC3339) + "/" + record.NodeID + "/" + record.Container
		bucket := buckets[key]
		if bucket == nil {
			bucket = &minuteBucket{ResourceRecord{At: at, NodeID: record.NodeID, Container: record.Container, Type: "aggregate", Complete: true}, map[string]*ResourceAggregate{}}
			buckets[key] = bucket
		}
		if !record.Complete {
			bucket.record.Complete = false
			bucket.record.Error = "incomplete source coverage in this minute"
		}
		recordMetrics(record, func(process, name, component, source, metric string, value float64) {
			id := process + "/" + source + "/" + metric
			aggregate := bucket.values[id]
			if aggregate == nil {
				aggregate = &ResourceAggregate{ProcessID: process, Name: name, Component: component, Source: source, Metric: metric, Min: value, Max: value, PeakAt: record.At}
				bucket.values[id] = aggregate
			}
			if value < aggregate.Min {
				aggregate.Min = value
			}
			if value > aggregate.Max {
				aggregate.Max = value
				aggregate.PeakAt = record.At
			}
			weight := record.IntervalSeconds
			if weight <= 0 {
				weight = 1
			}
			aggregate.Mean = (aggregate.Mean*aggregate.Weight + value*weight) / (aggregate.Weight + weight)
			aggregate.Weight += weight
			aggregate.Samples++
			aggregate.Seconds += record.IntervalSeconds
			aggregate.Last = value
		})
		if len(buckets) > 4000 {
			return errors.New("resource compaction bucket limit exceeded")
		}
	}
	if err = scanner.Err(); err != nil {
		return err
	}
	checkpoint := "raw:" + hex.EncodeToString(hash.Sum(nil))
	keys := make([]string, 0, len(buckets))
	for key := range buckets {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	// Replace atomically with existing aggregate bytes + this segment, so a failed
	// write cannot leave half a compaction to duplicate after a retry.
	destination := filepath.Join(s.root, "runs", run, "resource-minutes.ndjson")
	previous, err := os.Open(destination)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if previous != nil {
		defer previous.Close()
		info, err := previous.Stat()
		if err != nil {
			return err
		}
		if info.Size() >= 256<<20 {
			return errors.New("minute aggregate retention reached 256 MiB; raw segment retained")
		}
		scan := bufio.NewScanner(previous)
		scan.Buffer(make([]byte, 64<<10), 1<<20)
		for scan.Scan() {
			var header struct {
				Type  string `json:"type"`
				Error string `json:"error"`
			}
			if json.Unmarshal(scan.Bytes(), &header) != nil {
				return errors.New("minute aggregate file is corrupt")
			}
			if header.Type == "checkpoint" && header.Error == checkpoint {
				return nil
			}
		}
		if err := scan.Err(); err != nil {
			return err
		}
		if _, err := previous.Seek(0, 0); err != nil {
			return err
		}
	}
	temp, err := os.CreateTemp(filepath.Dir(destination), ".resource-minutes-*")
	if err != nil {
		return err
	}
	defer os.Remove(temp.Name())
	defer temp.Close()
	if previous != nil {
		if _, err = io.Copy(temp, previous); err != nil {
			return err
		}
	}
	encoder := json.NewEncoder(temp)
	if err = encoder.Encode(ResourceRecord{Type: "checkpoint", Error: checkpoint}); err != nil {
		return err
	}
	for _, key := range keys {
		bucket := buckets[key]
		ids := make([]string, 0, len(bucket.values))
		for id := range bucket.values {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			bucket.record.Aggregates = append(bucket.record.Aggregates, *bucket.values[id])
		}
		if err = encoder.Encode(bucket.record); err != nil {
			return err
		}
	}
	info, err := temp.Stat()
	if err != nil {
		return err
	}
	if info.Size() > 256<<20 {
		return fmt.Errorf("minute aggregate retention reached 256 MiB; raw segment retained")
	}
	if err = temp.Sync(); err != nil {
		return err
	}
	if err = temp.Close(); err != nil {
		return err
	}
	if err = os.Rename(temp.Name(), destination); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(destination))
	if err != nil {
		return err
	}
	err = dir.Sync()
	dir.Close()
	if err != nil {
		return err
	}
	s.mu.Lock()
	status := s.status[run]
	status.AggregateBytes = info.Size()
	s.status[run] = status
	s.dirty[run] = true
	s.mu.Unlock()
	return nil
}
