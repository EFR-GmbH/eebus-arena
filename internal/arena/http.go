package arena

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

func jsonResponse(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(value)
}
func decode(w http.ResponseWriter, r *http.Request, value any) error {
	r.Body = http.MaxBytesReader(w, r.Body, 8<<20)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return fmt.Errorf("request must contain one JSON value")
	}
	return nil
}
func (c *Controller) Handler(ui fs.FS) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/state", func(w http.ResponseWriter, r *http.Request) { jsonResponse(w, 200, c.Snapshot()) })
	mux.HandleFunc("POST /api/v1/evidence", func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, 512<<20)
		ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
		defer cancel()
		run, err := c.ImportEvidence(ctx, r.Body)
		if err != nil {
			jsonResponse(w, 400, map[string]any{"error": err.Error()})
			return
		}
		jsonResponse(w, 201, map[string]any{"id": run, "readOnly": true})
	})
	mux.HandleFunc("POST /api/v1/runs/{run}/faults", func(w http.ResponseWriter, r *http.Request) {
		var profile NetworkFault
		if err := decode(w, r, &profile); err != nil {
			respondAction(w, err)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
		defer cancel()
		lease, err := c.ApplyNetworkFault(ctx, r.PathValue("run"), profile)
		if err != nil {
			respondAction(w, err)
			return
		}
		jsonResponse(w, 201, lease)
	})
	mux.HandleFunc("DELETE /api/v1/runs/{run}/faults/{fault}", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
		defer cancel()
		respondAction(w, c.RestoreNetworkFault(ctx, r.PathValue("run"), r.PathValue("fault")))
	})
	mux.HandleFunc("GET /api/v1/runs/{run}/timeline", func(w http.ResponseWriter, r *http.Request) {
		var from, to time.Time
		var err error
		for key, target := range map[string]*time.Time{"from": &from, "to": &to} {
			if value := r.URL.Query().Get(key); value != "" {
				*target, err = time.Parse(time.RFC3339Nano, value)
				if err != nil {
					jsonResponse(w, 400, map[string]any{"error": key + " must be RFC3339"})
					return
				}
			}
		}
		limit := 500
		if value := r.URL.Query().Get("limit"); value != "" {
			limit, err = strconv.Atoi(value)
			if err != nil {
				jsonResponse(w, 400, map[string]any{"error": "invalid limit"})
				return
			}
		}
		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		defer cancel()
		window, err := c.Timeline(ctx, r.PathValue("run"), r.URL.Query().Get("node"), r.URL.Query().Get("prefix"), from, to, limit)
		if err != nil {
			jsonResponse(w, 400, map[string]any{"error": err.Error()})
			return
		}
		jsonResponse(w, 200, window)
	})
	mux.HandleFunc("GET /api/v1/runs/{run}/resources", func(w http.ResponseWriter, r *http.Request) {
		var from, to time.Time
		var err error
		if text := r.URL.Query().Get("from"); text != "" {
			from, err = time.Parse(time.RFC3339Nano, text)
			if err != nil {
				jsonResponse(w, 400, map[string]any{"error": "from must be RFC3339"})
				return
			}
		}
		if text := r.URL.Query().Get("to"); text != "" {
			to, err = time.Parse(time.RFC3339Nano, text)
			if err != nil {
				jsonResponse(w, 400, map[string]any{"error": "to must be RFC3339"})
				return
			}
		}
		if text := r.URL.Query().Get("windowSeconds"); text != "" {
			seconds, parseErr := strconv.Atoi(text)
			if parseErr != nil || seconds < 1 || seconds > 31536000 || !from.IsZero() {
				jsonResponse(w, 400, map[string]any{"error": "windowSeconds must be 1–31536000, without from"})
				return
			}
			if to.IsZero() {
				to = time.Now().UTC()
				c.mu.Lock()
				if run := c.runs[r.PathValue("run")]; run != nil && Contains([]string{"created", "stopped", "archived"}, run.Status) {
					c.resources.mu.Lock()
					last := c.resources.status[run.ID].LastAt
					c.resources.mu.Unlock()
					if !last.IsZero() {
						to = last.Add(time.Second)
					}
				}
				c.mu.Unlock()
			}
			from = to.Add(-time.Duration(seconds) * time.Second)
		}
		points := 400
		if text := r.URL.Query().Get("points"); text != "" {
			points, err = strconv.Atoi(text)
			if err != nil {
				jsonResponse(w, 400, map[string]any{"error": "points must be an integer"})
				return
			}
		}
		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		defer cancel()
		history, err := c.ResourceHistory(ctx, r.PathValue("run"), r.URL.Query().Get("node"), from, to, points)
		if err != nil {
			jsonResponse(w, 400, map[string]any{"error": err.Error()})
			return
		}
		jsonResponse(w, 200, history)
	})
	mux.HandleFunc("POST /api/v1/variants/reload", func(w http.ResponseWriter, r *http.Request) {
		if err := c.LoadVariants(); err != nil {
			jsonResponse(w, 400, map[string]any{"error": err.Error()})
			return
		}
		jsonResponse(w, 200, c.Snapshot())
	})
	mux.HandleFunc("POST /api/v1/runs", func(w http.ResponseWriter, r *http.Request) {
		var bundle Bundle
		if err := decode(w, r, &bundle); err != nil {
			jsonResponse(w, 400, map[string]any{"error": err.Error()})
			return
		}
		id, err := c.Create(bundle)
		if err != nil {
			jsonResponse(w, 400, map[string]any{"error": err.Error()})
			return
		}
		jsonResponse(w, 201, map[string]any{"id": id})
	})
	mux.HandleFunc("PUT /api/v1/runs/{run}", func(w http.ResponseWriter, r *http.Request) {
		var input EditRequest
		if err := decode(w, r, &input); err != nil {
			jsonResponse(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		if err := c.Update(ctx, r.PathValue("run"), input); err != nil {
			status := http.StatusBadRequest
			if errors.Is(err, ErrRunNotFound) {
				status = http.StatusNotFound
			} else if errors.Is(err, ErrEditActive) || errors.Is(err, ErrEditConflict) {
				status = http.StatusConflict
			}
			jsonResponse(w, status, map[string]any{"error": err.Error()})
			return
		}
		jsonResponse(w, http.StatusOK, map[string]any{"id": r.PathValue("run")})
	})
	mux.HandleFunc("POST /api/v1/runs/{run}/start", func(w http.ResponseWriter, r *http.Request) {
		err := c.Start(r.PathValue("run"))
		respondAction(w, err)
	})
	mux.HandleFunc("POST /api/v1/runs/{run}/stop", func(w http.ResponseWriter, r *http.Request) { err := c.Down(r.PathValue("run")); respondAction(w, err) })
	mux.HandleFunc("DELETE /api/v1/runs/{run}", func(w http.ResponseWriter, r *http.Request) {
		respondDrop(w, c.Drop(r.PathValue("run")))
	})
	mux.HandleFunc("POST /api/v1/runs/{run}/restore", func(w http.ResponseWriter, r *http.Request) {
		respondDrop(w, c.Restore(r.PathValue("run")))
	})
	mux.HandleFunc("POST /api/v1/runs/{run}/test", func(w http.ResponseWriter, r *http.Request) { err := c.Test(r.PathValue("run")); respondAction(w, err) })
	mux.HandleFunc("POST /api/v1/runs/{run}/nodes/{node}/actions", func(w http.ResponseWriter, r *http.Request) {
		var input struct {
			Action string `json:"action"`
		}
		if err := decode(w, r, &input); err != nil {
			respondAction(w, err)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		respondAction(w, c.Action(ctx, r.PathValue("run"), r.PathValue("node"), input.Action))
	})
	mux.HandleFunc("POST /api/v1/runs/{run}/nodes/{node}/pairing/reset", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		respondAction(w, c.ResetPairing(ctx, r.PathValue("run"), r.PathValue("node")))
	})
	mux.HandleFunc("POST /api/v1/runs/{run}/nodes/{node}/commands", func(w http.ResponseWriter, r *http.Request) {
		var input Command
		if err := decode(w, r, &input); err != nil {
			respondAction(w, err)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
		defer cancel()
		event, err := c.Command(ctx, r.PathValue("run"), r.PathValue("node"), input)
		if err != nil {
			respondAction(w, err)
			return
		}
		jsonResponse(w, 200, event)
	})
	mux.HandleFunc("GET /api/v1/runs/{run}/export", func(w http.ResponseWriter, r *http.Request) {
		runID := r.PathValue("run")
		c.mu.Lock()
		known := c.runs[runID] != nil
		c.mu.Unlock()
		if !known {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Disposition", `attachment; filename="arena-`+runID+`.json"`)
		w.Header().Set("Content-Type", "application/json")
		http.ServeFile(w, r, c.path(runID, "bundle.json"))
	})
	mux.HandleFunc("GET /api/v1/runs/{run}/evidence", func(w http.ResponseWriter, r *http.Request) {
		runID := r.PathValue("run")
		type evidenceFile struct {
			file *os.File
			name string
			info os.FileInfo
		}
		var files []evidenceFile
		if c.resources != nil {
			ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
			err := c.resources.flush(ctx)
			cancel()
			if err != nil {
				jsonResponse(w, 503, map[string]any{"error": err.Error()})
				return
			}
		}
		// Wait for compaction outside the controller lock, then pin files while
		// both rotation and Drop are excluded.
		if c.resources != nil {
			c.resources.fileMu.Lock()
		}
		c.mu.Lock()
		known := c.runs[runID] != nil
		if known {
			// Pin all segments together. Rotation and Drop cannot change their
			// identity while the archive captures this consistent set of files.
			names := []string{"bundle.json", "run.json", "compose.json", "events.ndjson.2", "events.ndjson.1", "events.ndjson", "resources.ndjson.2", "resources.ndjson.1", "resources.ndjson", "resource-minutes.ndjson", "resource-meta.json"}
			// Historical snapshots bind prior assertions and telemetry to the
			// setup that produced them.
			entries, _ := os.ReadDir(c.path(runID, "revisions"))
			for _, entry := range entries {
				if entry.IsDir() && safeID.MatchString(entry.Name()) && strings.HasPrefix(entry.Name(), "edit-") {
					names = append(names, filepath.Join("revisions", entry.Name(), "run.json"))
				}
			}
			if Contains([]string{"created", "stopped"}, c.runs[runID].Status) {
				for _, n := range c.runs[runID].Nodes {
					names = append(names, filepath.Join("state", n.ID, "pairing.json"))
				}
			}
			for _, name := range names {
				f, err := os.Open(c.path(runID, name))
				if err != nil {
					continue
				}
				info, err := f.Stat()
				if err != nil {
					f.Close()
					continue
				}
				files = append(files, evidenceFile{f, name, info})
			}
		}
		if c.resources != nil {
			c.resources.fileMu.Unlock()
		}
		c.mu.Unlock()
		defer func() {
			for _, f := range files {
				f.file.Close()
			}
		}()
		if !known {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/gzip")
		w.Header().Set("Content-Disposition", `attachment; filename="evidence-`+runID+`.tar.gz"`)
		gz := gzip.NewWriter(w)
		defer gz.Close()
		archive := tar.NewWriter(gz)
		defer archive.Close()
		for _, f := range files {
			header := &tar.Header{Name: f.name, Mode: 0600, Size: f.info.Size(), ModTime: f.info.ModTime()}
			if err := archive.WriteHeader(header); err != nil {
				return
			}
			if _, err := io.CopyN(archive, f.file, f.info.Size()); err != nil {
				return
			}
		}
	})
	mux.HandleFunc("GET /api/v1/events", func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "stream unavailable", 500)
			return
		}
		cursor := r.Header.Get("Last-Event-ID")
		if cursor == "" {
			cursor = r.URL.Query().Get("after")
		}
		epoch, value, hasEpoch := strings.Cut(cursor, ":")
		if !hasEpoch {
			value, epoch = cursor, ""
		}
		after, _ := strconv.ParseUint(value, 10, 64)
		if !hasEpoch && after > 0 {
			epoch = "legacy"
		}
		ch, history := c.subscribe(epoch, after)
		defer c.Unsubscribe(ch)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("X-Accel-Buffering", "no")
		send := func(e Event) bool {
			b, _ := json.Marshal(e)
			_, err := fmt.Fprintf(w, "id: %s:%d\ndata: %s\n\n", e.Epoch, e.Index, b)
			flusher.Flush()
			return err == nil
		}
		for _, e := range history {
			if !send(e) {
				return
			}
		}
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-r.Context().Done():
				return
			case e, ok := <-ch:
				if !ok || !send(e) {
					return
				}
			case <-ticker.C:
				fmt.Fprint(w, ": keepalive\n\n")
				flusher.Flush()
			}
		}
	})
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		c.mu.Lock()
		var body strings.Builder
		for _, run := range c.runs {
			for _, node := range run.Nodes {
				for stage, metric := range node.PairingMetrics {
					for i, count := range metric.Buckets {
						fmt.Fprintf(&body, "arena_pairing_stage_seconds_bucket{run=%q,node=%q,stack=%q,role=%q,stage=%q,le=%q} %d\n", run.ID, node.ID, node.Stack, node.Role, stage, pairingBucketLabel(i), count)
					}
					fmt.Fprintf(&body, "arena_pairing_stage_seconds_count{run=%q,node=%q,stack=%q,role=%q,stage=%q} %d\n", run.ID, node.ID, node.Stack, node.Role, stage, metric.Count)
					fmt.Fprintf(&body, "arena_pairing_stage_seconds_sum{run=%q,node=%q,stack=%q,role=%q,stage=%q} %g\n", run.ID, node.ID, node.Stack, node.Role, stage, metric.Sum)
				}
				for event, count := range node.Counters {
					fmt.Fprintf(&body, "arena_events_total{run=%q,node=%q,stack=%q,event=%q} %d\n", run.ID, node.ID, node.Stack, event, count)
				}
				available := Contains([]string{"running", "paused"}, node.Lifecycle) && sampleFresh(node.Metrics)
				flag := 0
				if available {
					flag = 1
				}
				fmt.Fprintf(&body, "arena_node_metrics_available{run=%q,node=%q,stack=%q} %d\n", run.ID, node.ID, node.Stack, flag)
				if !available {
					continue
				}
				for name, key := range map[string]string{"cpu_percent": "cpuPercent", "memory_bytes": "memoryBytes", "working_set_bytes": "workingSetBytes", "network_receive_bytes_total": "rxBytes", "network_transmit_bytes_total": "txBytes", "block_read_bytes_total": "blockReadBytes", "block_write_bytes_total": "blockWriteBytes"} {
					if node.Metrics[key] == nil {
						continue
					}
					fmt.Fprintf(&body, "arena_node_%s{run=%q,node=%q,stack=%q} %g\n", name, run.ID, node.ID, node.Stack, num(node.Metrics[key]))
				}
			}
		}
		if sampleFresh(c.controlMetrics) && c.controlMetrics["complete"] == true {
			fmt.Fprintf(&body, "arena_control_memory_bytes %g\n", num(c.controlMetrics["memoryBytes"]))
		}
		c.mu.Unlock()
		io.WriteString(w, body.String())
	})
	files := http.FileServer(http.FS(ui))
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") {
			http.NotFound(w, r)
			return
		}
		name := strings.TrimPrefix(filepath.Clean(r.URL.Path), "/")
		if name == "." || name == "" {
			name = "index.html"
		}
		if _, err := fs.Stat(ui, name); err != nil || name == "index.html" {
			data, err := fs.ReadFile(ui, "index.html")
			if err != nil {
				http.Error(w, "UI build unavailable", 503)
				return
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			http.ServeContent(w, r, "index.html", time.Time{}, bytes.NewReader(data))
			return
		}
		files.ServeHTTP(w, r)
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Content-Security-Policy", "frame-ancestors 'none'")
		host := r.Host
		if value, _, err := net.SplitHostPort(host); err == nil {
			host = value
		}
		allowed := append([]string{"localhost", "127.0.0.1", "::1", "arena"}, strings.Split(os.Getenv("ARENA_ALLOWED_HOSTS"), ",")...)
		if host == "" || !Contains(allowed, host) {
			http.Error(w, "host is not allowed", 403)
			return
		}
		if r.Method != "GET" && r.Method != "HEAD" {
			origin := r.Header.Get("Origin")
			if origin != "" && origin != "http://"+r.Host && origin != "https://"+r.Host {
				http.Error(w, "cross-origin actions are disabled", 403)
				return
			}
			expected := "application/json"
			if r.Method == "POST" && r.URL.Path == "/api/v1/evidence" {
				expected = "application/gzip"
			}
			mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
			if err != nil || mediaType != expected {
				http.Error(w, "action requires "+expected, 415)
				return
			}
		}
		mux.ServeHTTP(w, r)
	})
}
func respondAction(w http.ResponseWriter, err error) {
	if err != nil {
		jsonResponse(w, 400, map[string]any{"error": err.Error()})
		return
	}
	jsonResponse(w, 202, map[string]any{"ok": true})
}

func respondDrop(w http.ResponseWriter, err error) {
	if err != nil {
		status := http.StatusConflict
		if errors.Is(err, ErrRunNotFound) {
			status = http.StatusNotFound
		}
		jsonResponse(w, status, map[string]any{"error": err.Error()})
		return
	}
	jsonResponse(w, http.StatusOK, map[string]any{"ok": true})
}
