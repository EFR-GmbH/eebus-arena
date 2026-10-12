package arena

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/EFR-GmbH/eebus-arena/internal/resourceprobe"
)

// ResourceRecord keeps container totals separate from attributed process samples.
type ResourceRecord struct {
	At              time.Time               `json:"at"`
	NodeID          string                  `json:"nodeId"`
	Container       string                  `json:"container,omitempty"`
	Incarnation     string                  `json:"incarnation,omitempty"`
	Type            string                  `json:"type"`
	IntervalSeconds float64                 `json:"intervalSeconds,omitempty"`
	Metrics         map[string]any          `json:"metrics,omitempty"`
	Processes       []resourceprobe.Process `json:"processes,omitempty"`
	Complete        bool                    `json:"complete"`
	Error           string                  `json:"error,omitempty"`
	Scope           string                  `json:"scope,omitempty"`
	SourceAt        *time.Time              `json:"sourceAt,omitempty"`
	Aggregates      []ResourceAggregate     `json:"aggregates,omitempty"`
}

// ResourceAggregate preserves a minute's measured extrema and interval weighting.
type ResourceAggregate struct {
	ProcessID string    `json:"processId,omitempty"`
	Name      string    `json:"name"`
	Component string    `json:"component"`
	Source    string    `json:"source"`
	Metric    string    `json:"metric"`
	Min       float64   `json:"min"`
	Max       float64   `json:"max"`
	Mean      float64   `json:"mean"`
	Last      float64   `json:"last"`
	Samples   int       `json:"samples"`
	Seconds   float64   `json:"seconds"`
	Weight    float64   `json:"weight"`
	PeakAt    time.Time `json:"peakAt"`
}

// ResourceStatus describes retained coverage and writer health.
type ResourceStatus struct {
	QueueDrops     uint64    `json:"queueDrops"`
	WriteErrors    uint64    `json:"writeErrors"`
	RetentionDrops uint64    `json:"retentionDrops"`
	Rotations      uint64    `json:"rotations"`
	LastError      string    `json:"lastError,omitempty"`
	FirstAt        time.Time `json:"firstAt,omitempty"`
	LastAt         time.Time `json:"lastAt,omitempty"`
	AggregateBytes int64     `json:"aggregateBytes"`
}

type resourceWrite struct {
	run     string
	data    []byte
	barrier chan struct{}
	stop    bool
	release string
	durable bool
}
type resourceStore struct {
	root         string
	queue        chan resourceWrite
	done         chan struct{}
	mu           sync.Mutex
	fileMu       sync.Mutex
	status       map[string]ResourceStatus
	dirty        map[string]bool
	closed       bool
	segmentBytes int64
}

func newResourceStore(root string) *resourceStore {
	s := &resourceStore{root: root, queue: make(chan resourceWrite, 4096), done: make(chan struct{}), status: map[string]ResourceStatus{}, dirty: map[string]bool{}, segmentBytes: 32 << 20}
	entries, err := os.ReadDir(filepath.Join(root, "runs"))
	if err == nil {
		for _, entry := range entries {
			if safeID.MatchString(entry.Name()) {
				var status ResourceStatus
				if blob, err := os.ReadFile(filepath.Join(root, "runs", entry.Name(), "resource-meta.json")); err == nil {
					if json.Unmarshal(blob, &status) == nil {
						s.status[entry.Name()] = status
					}
				}
			}
		}
	}
	go s.loop()
	return s
}

// enqueue freezes sample data before handing it to the writer.
func (s *resourceStore) enqueue(run string, record ResourceRecord) {
	data, err := json.Marshal(record)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.dirty[run] = true
	if err != nil {
		status := s.status[run]
		status.WriteErrors++
		status.LastError = err.Error()
		s.status[run] = status
		return
	}
	select {
	case s.queue <- resourceWrite{run: run, data: append(data, '\n')}:
		status := s.status[run]
		if status.FirstAt.IsZero() || record.At.Before(status.FirstAt) {
			status.FirstAt = record.At
		}
		if record.At.After(status.LastAt) {
			status.LastAt = record.At
		}
		s.status[run] = status
	default:
		status := s.status[run]
		status.QueueDrops++
		s.status[run] = status
	}
}

func (s *resourceStore) release(ctx context.Context, run string) error {
	barrier := make(chan struct{})
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-s.done:
		return nil
	case s.queue <- resourceWrite{release: run, barrier: barrier}:
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-barrier:
		return nil
	case <-s.done:
		return nil
	}
}
func (s *resourceStore) forget(run string) {
	s.mu.Lock()
	delete(s.status, run)
	delete(s.dirty, run)
	s.mu.Unlock()
}

func (s *resourceStore) flush(ctx context.Context) error {
	return s.barrier(ctx, true)
}

func (s *resourceStore) barrier(ctx context.Context, durable bool) error {
	barrier := make(chan struct{})
	select {
	case <-s.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case s.queue <- resourceWrite{barrier: barrier, durable: durable}:
	}
	select {
	case <-barrier:
		return nil
	case <-s.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *resourceStore) close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		<-s.done
		return
	}
	s.closed = true
	s.mu.Unlock()
	s.queue <- resourceWrite{stop: true}
	<-s.done
}

func (s *resourceStore) loop() {
	defer close(s.done)
	files := map[string]*os.File{}
	retryAt := map[string]time.Time{}
	unsynced := map[string]bool{}
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	persist := func() {
		s.mu.Lock()
		snapshot := map[string]ResourceStatus{}
		for run := range s.dirty {
			snapshot[run] = s.status[run]
			delete(s.dirty, run)
		}
		s.mu.Unlock()
		for run, status := range snapshot {
			if _, err := os.Stat(filepath.Join(s.root, "runs", run)); errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err := writeJSON(filepath.Join(s.root, "runs", run, "resource-meta.json"), status); err != nil {
				s.recordError(run, err)
			}
		}
	}
	for {
		var write resourceWrite
		select {
		case write = <-s.queue:
		case <-ticker.C:
			write = resourceWrite{barrier: make(chan struct{}), durable: true}
		}
		if write.stop || write.barrier != nil {
			if write.stop || write.durable || write.release != "" {
				for run, file := range files {
					if !unsynced[run] {
						continue
					}
					if err := file.Sync(); err != nil {
						s.recordError(run, err)
					} else {
						delete(unsynced, run)
					}
				}
			}
			if write.release != "" {
				if file := files[write.release]; file != nil {
					if err := file.Close(); err != nil {
						s.recordError(write.release, err)
					}
					delete(files, write.release)
					delete(retryAt, write.release)
					delete(unsynced, write.release)
				}
			}
			if write.stop || write.durable || write.release != "" {
				persist()
			}
			if write.barrier != nil {
				close(write.barrier)
			}
			if write.stop {
				for run, file := range files {
					if err := file.Close(); err != nil {
						s.recordError(run, err)
					}
				}
				persist()
				return
			}
			continue
		}
		s.fileMu.Lock()
		path := filepath.Join(s.root, "runs", write.run, "resources.ndjson")
		file := files[write.run]
		var err error
		if file == nil {
			file, err = os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
			if err == nil {
				files[write.run] = file
			}
		}
		if err == nil {
			var info os.FileInfo
			info, err = file.Stat()
			if err == nil && info.Size()+int64(len(write.data)) > s.segmentBytes && !time.Now().Before(retryAt[write.run]) {
				err = file.Close()
				delete(files, write.run)
				if err == nil {
					err = s.compact(write.run, path+".2")
				}
				if err == nil {
					err = os.Remove(path + ".2")
					if errors.Is(err, os.ErrNotExist) {
						err = nil
					}
				}
				if err == nil {
					err = os.Rename(path+".1", path+".2")
					if errors.Is(err, os.ErrNotExist) {
						err = nil
					}
				}
				if err == nil {
					err = os.Rename(path, path+".1")
				}
				if err == nil {
					file, err = os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
					if err == nil {
						files[write.run] = file
					}
				}
				s.mu.Lock()
				status := s.status[write.run]
				if err == nil {
					status.Rotations++
					delete(retryAt, write.run)
				}
				s.status[write.run] = status
				s.dirty[write.run] = true
				s.mu.Unlock()
				if err != nil {
					s.recordError(write.run, err)
					retryAt[write.run] = time.Now().Add(time.Minute)
					// Preserve the old segments and allow one bounded segment of
					// headroom for new readings while compaction is unavailable.
					file, err = os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
					if err == nil {
						files[write.run] = file
					}
				}
			}
			if err == nil {
				info, err = file.Stat()
				if err == nil && info.Size()+int64(len(write.data)) > 2*s.segmentBytes {
					s.mu.Lock()
					status := s.status[write.run]
					status.RetentionDrops++
					status.LastError = "raw retention headroom exhausted; new samples discarded until rotation succeeds"
					s.status[write.run] = status
					s.dirty[write.run] = true
					s.mu.Unlock()
				} else if err == nil {
					_, err = file.Write(write.data)
					unsynced[write.run] = true
				}
			}
		}
		if err != nil {
			s.recordError(write.run, err)
		}
		s.fileMu.Unlock()
	}
}

func (s *resourceStore) recordError(run string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	status := s.status[run]
	status.WriteErrors++
	status.LastError = err.Error()
	s.status[run] = status
	s.dirty[run] = true
}

func (s *resourceStore) read(ctx context.Context, run string, consume func(ResourceRecord)) (ResourceStatus, error) {
	if err := s.barrier(ctx, false); err != nil {
		return ResourceStatus{}, err
	}
	s.fileMu.Lock()
	type segment struct {
		file *os.File
		size int64
		name string
	}
	files := []segment{}
	for _, name := range []string{"resource-minutes.ndjson", "resources.ndjson.2", "resources.ndjson.1", "resources.ndjson"} {
		file, err := os.Open(filepath.Join(s.root, "runs", run, name))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			s.fileMu.Unlock()
			for _, entry := range files {
				entry.file.Close()
			}
			return ResourceStatus{}, err
		}
		info, err := file.Stat()
		if err != nil {
			file.Close()
			s.fileMu.Unlock()
			for _, entry := range files {
				entry.file.Close()
			}
			return ResourceStatus{}, err
		}
		files = append(files, segment{file, info.Size(), name})
	}
	s.fileMu.Unlock()
	s.mu.Lock()
	status := s.status[run]
	s.mu.Unlock()
	defer func() {
		for _, file := range files {
			file.file.Close()
		}
	}()
	compacted := map[string]bool{}
	for _, file := range files {
		// Compaction checkpoints can only describe the oldest raw segment.
		if file.name == "resources.ndjson.2" && len(compacted) > 0 {
			hash := sha256.New()
			if _, err := io.Copy(hash, io.LimitReader(file.file, file.size)); err != nil {
				return status, err
			}
			if compacted["raw:"+hex.EncodeToString(hash.Sum(nil))] {
				continue
			}
			if _, err := file.file.Seek(0, 0); err != nil {
				return status, err
			}
		}
		scanner := bufio.NewScanner(io.LimitReader(file.file, file.size))
		scanner.Buffer(make([]byte, 64<<10), 1<<20)
		for scanner.Scan() {
			if err := ctx.Err(); err != nil {
				return status, err
			}
			var record ResourceRecord
			if err := json.Unmarshal(scanner.Bytes(), &record); err != nil {
				return status, fmt.Errorf("resource history is corrupt: %w", err)
			}
			if record.Type == "checkpoint" {
				compacted[record.Error] = true
				continue
			}
			consume(record)
		}
		if err := scanner.Err(); err != nil {
			return status, err
		}
	}
	return status, nil
}
