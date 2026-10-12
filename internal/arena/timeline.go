package arena

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"time"
)

// TimelineWindow reads retained protocol evidence with its real receipt clock.
type TimelineWindow struct {
	Events    []Event `json:"events"`
	Truncated bool    `json:"truncated"`
	Rotated   bool    `json:"rotated"`
	Matched   int     `json:"matched"`
}

// Timeline pins journal files before releasing the lock, so rotation cannot mix segments.
func (c *Controller) Timeline(ctx context.Context, run, node, prefix string, from, to time.Time, limit int) (TimelineWindow, error) {
	result := TimelineWindow{Events: []Event{}}
	if limit < 1 || limit > 2000 {
		return result, errors.New("limit must be 1–2000")
	}
	if !from.IsZero() && !to.IsZero() && !to.After(from) {
		return result, errors.New("invalid timeline interval")
	}
	type segment struct {
		file *os.File
		size int64
	}
	var files []segment
	defer func() {
		for _, f := range files {
			f.file.Close()
		}
	}()
	c.mu.Lock()
	if c.runs[run] == nil {
		c.mu.Unlock()
		return result, errors.New("run not found")
	}
	for _, name := range []string{"events.ndjson.2", "events.ndjson.1", "events.ndjson"} {
		file, err := os.Open(c.path(run, name))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			c.mu.Unlock()
			return result, err
		}
		info, err := file.Stat()
		if err != nil {
			file.Close()
			c.mu.Unlock()
			return result, err
		}
		files = append(files, segment{file, info.Size()})
	}
	c.mu.Unlock()
	next := 0
	for _, file := range files {
		scanner := bufio.NewScanner(io.LimitReader(file.file, file.size))
		scanner.Buffer(make([]byte, 64<<10), 2<<20)
		for scanner.Scan() {
			if err := ctx.Err(); err != nil {
				return result, err
			}
			var event Event
			if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
				return result, errors.New("malformed retained timeline record")
			}
			if event.Type == "evidence.rotated" {
				result.Rotated = true
			}
			at := event.ReceivedAt
			if at.IsZero() {
				at = event.At
				event.ReceivedAt = at
			}
			if event.SourceAt != nil && event.SourceAt.IsZero() {
				event.SourceAt = nil
			}
			if node != "" && event.NodeID != node || !strings.HasPrefix(event.Type, prefix) || !from.IsZero() && at.Before(from) || !to.IsZero() && at.After(to) {
				continue
			}
			if event.Type == "metrics" || event.Type == "snapshot" {
				continue
			}
			result.Matched++
			if len(result.Events) < limit {
				result.Events = append(result.Events, event)
			} else {
				result.Events[next] = event
				next = (next + 1) % limit
				result.Truncated = true
			}
		}
		if err := scanner.Err(); err != nil {
			return result, err
		}
	}
	if result.Truncated && next != 0 {
		result.Events = append(append([]Event{}, result.Events[next:]...), result.Events[:next]...)
	}
	return result, nil
}
