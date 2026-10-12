package arena

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// ImportEvidence opens a historical archive without installing images, identities
// or commands. Its nodes can be inspected but never started or controlled.
func (c *Controller) ImportEvidence(ctx context.Context, input io.Reader) (string, error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return "", errors.New("controller is closed")
	}
	c.workers.Add(1)
	c.mu.Unlock()
	defer c.workers.Done()
	gz, err := gzip.NewReader(input)
	if err != nil {
		return "", err
	}
	defer gz.Close()
	uncompressed := &io.LimitedReader{R: gz, N: (512 << 20) + 1}
	archive := tar.NewReader(uncompressed)
	runID := "run-" + id()
	directory := c.path(runID, "")
	if err = os.MkdirAll(directory, 0700); err != nil {
		return "", err
	}
	keep := false
	defer func() {
		if !keep {
			_ = os.RemoveAll(directory)
		}
	}()
	allowed := map[string]bool{"run.json": true, "events.ndjson": true, "events.ndjson.1": true, "events.ndjson.2": true, "resources.ndjson": true, "resources.ndjson.1": true, "resources.ndjson.2": true, "resource-minutes.ndjson": true, "resource-meta.json": true}
	seen := map[string]bool{}
	var total int64
	for count := 0; ; count++ {
		if err = ctx.Err(); err != nil {
			return "", err
		}
		header, nextErr := archive.Next()
		if nextErr == io.EOF {
			break
		}
		if nextErr != nil {
			return "", nextErr
		}
		if count >= 512 || header.Size < 0 || header.Size > 256<<20 || total+header.Size > 512<<20 {
			return "", errors.New("evidence archive exceeds storage bounds")
		}
		total += header.Size
		if header.Typeflag != tar.TypeReg || filepath.IsAbs(header.Name) || filepath.Clean(header.Name) != header.Name || strings.Contains(header.Name, "\\") || strings.HasPrefix(header.Name, "..") {
			return "", errors.New("evidence must contain regular relative files")
		}
		parts := strings.Split(header.Name, "/")
		historical := len(parts) == 3 && parts[0] == "revisions" && safeID.MatchString(parts[1]) && strings.HasPrefix(parts[1], "edit-") && parts[2] == "run.json"
		if !allowed[header.Name] && !historical {
			continue
		}
		if seen[header.Name] {
			return "", errors.New("duplicate evidence segment")
		}
		seen[header.Name] = true
		if header.Name == "run.json" || header.Name == "resource-meta.json" || historical {
			if header.Size > 8<<20 {
				return "", errors.New("evidence metadata exceeds 8 MiB")
			}
		}
		if err = os.MkdirAll(filepath.Dir(filepath.Join(directory, header.Name)), 0700); err != nil {
			return "", err
		}
		file, createErr := os.OpenFile(filepath.Join(directory, header.Name), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if createErr != nil {
			return "", createErr
		}
		_, copyErr := io.CopyN(file, archive, header.Size)
		closeErr := file.Close()
		if err = errors.Join(copyErr, closeErr); err != nil {
			return "", err
		}
	}
	// A tar end marker can precede the gzip checksum. Drain with a hard bound
	// so corrupt trailers and appended compressed bombs cannot be accepted.
	if _, err = io.Copy(io.Discard, uncompressed); err != nil {
		return "", err
	}
	if uncompressed.N == 0 {
		return "", errors.New("decompressed evidence exceeds 512 MiB")
	}
	if !seen["run.json"] {
		return "", errors.New("evidence archive has no run snapshot")
	}
	var run Run
	blob, err := os.ReadFile(filepath.Join(directory, "run.json"))
	if err != nil {
		return "", err
	}
	if err = json.Unmarshal(blob, &run); err != nil {
		return "", err
	}
	if err = sanitizeEvidenceSnapshot(&run); err != nil {
		return "", err
	}
	original := run.ID
	run.ID = runID
	run.Status = "archived"
	run.Error = ""
	run.Runtime = map[string]any{"evidenceOriginalRun": original}
	if err = writeJSON(filepath.Join(directory, "run.json"), run); err != nil {
		return "", err
	}
	for name := range seen {
		if !strings.HasPrefix(name, "revisions/") {
			continue
		}
		var historical Run
		blob, err = os.ReadFile(filepath.Join(directory, name))
		if err != nil {
			return "", err
		}
		if err = json.Unmarshal(blob, &historical); err != nil {
			return "", err
		}
		if err = sanitizeEvidenceSnapshot(&historical); err != nil {
			return "", err
		}
		historical.Status, historical.Runtime = "archived", map[string]any{"evidenceOriginalRun": historical.ID}
		if err = writeJSON(filepath.Join(directory, name), historical); err != nil {
			return "", err
		}
	}
	if c.resources != nil {
		var status ResourceStatus
		if blob, err = os.ReadFile(filepath.Join(directory, "resource-meta.json")); err == nil {
			if err = json.Unmarshal(blob, &status); err != nil {
				return "", fmt.Errorf("resource metadata: %w", err)
			}
			c.resources.mu.Lock()
			c.resources.status[runID] = status
			c.resources.mu.Unlock()
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return "", errors.New("controller is closed")
	}
	c.runs[runID] = &run
	keep = true
	c.emitLocked(Event{RunID: runID, Type: "evidence.imported", Data: map[string]any{"originalRun": original, "readOnly": true}})
	return runID, nil
}

func sanitizeEvidenceSnapshot(run *Run) error {
	if len(run.Nodes) == 0 || len(run.Nodes) > 201 || run.Created.IsZero() {
		return errors.New("invalid evidence run snapshot")
	}
	nodes := map[string]bool{}
	for _, node := range run.Nodes {
		if node == nil || !safeID.MatchString(node.ID) || nodes[node.ID] {
			return errors.New("invalid or duplicate evidence node")
		}
		nodes[node.ID] = true
		node.Container = ""
		if node.External != nil {
			node.External.Token, node.External.Endpoint, node.External.TLSFingerprint = "", "", ""
		}
		node.Lifecycle = "archived"
	}
	for _, group := range run.Definition.Groups {
		if external := group.External; external != nil {
			external.Token, external.Endpoint, external.TLSFingerprint = "", "", ""
		}
	}
	return nil
}
