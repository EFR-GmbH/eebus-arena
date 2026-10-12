// Package labagent exposes a single managed native stub on a dedicated lab device.
package labagent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"sync"
	"syscall"
	"time"
)

var safeID = regexp.MustCompile(`^[a-z][a-z0-9-]{0,63}$`)

// Session contains only native configuration and identities, never executable paths.
type Session struct {
	Run         string         `json:"run"`
	Node        string         `json:"node"`
	Config      map[string]any `json:"config"`
	Certificate string         `json:"certificate"`
	PrivateKey  string         `json:"privateKey"`
}
type Envelope struct {
	Cursor uint64          `json:"cursor"`
	Event  json.RawMessage `json:"event"`
}
type Batch struct {
	Events  []Envelope `json:"events"`
	Gap     bool       `json:"gap"`
	Cursor  uint64     `json:"cursor"`
	Running bool       `json:"running"`
	Error   string     `json:"error,omitempty"`
}

// Agent permits one explicitly owned job and keeps bounded replayable observations.
type Agent struct {
	Root, Native, Supervisor, Token string
	mu                              sync.Mutex
	session                         *Session
	command                         *exec.Cmd
	stdin                           io.WriteCloser
	done                            chan struct{}
	cursor                          uint64
	events                          []Envelope
	err                             string
	operations                      sync.Mutex
}

func response(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func input(w http.ResponseWriter, r *http.Request, value any) error {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return err
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return errors.New("one JSON value required")
	}
	return nil
}
func (a *Agent) owned(r *http.Request) bool {
	return a.session != nil && a.session.Run == r.URL.Query().Get("run") && a.session.Node == r.URL.Query().Get("node")
}

// Handler protects every endpoint with a deployment token; use TLS with a pinned certificate.
func (a *Agent) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /session", func(w http.ResponseWriter, r *http.Request) {
		var session Session
		if err := input(w, r, &session); err != nil {
			response(w, 400, map[string]any{"error": err.Error()})
			return
		}
		a.operations.Lock()
		defer a.operations.Unlock()
		a.mu.Lock()
		busy := a.command != nil
		foreign := a.session != nil && (a.session.Run != session.Run || a.session.Node != session.Node)
		// A controller retry may have lost the successful response. Reattach only
		// to exactly the same native identity and configuration.
		current, _ := json.Marshal(a.session)
		requested, _ := json.Marshal(&session)
		same := busy && !foreign && bytes.Equal(current, requested)
		a.mu.Unlock()
		if same {
			response(w, 200, map[string]any{"stub": true, "reattached": true})
			return
		}
		if busy || foreign {
			response(w, 409, map[string]any{"error": "device is leased by a session; stop and release it first"})
			return
		}
		if err := a.start(session); err != nil {
			response(w, 400, map[string]any{"error": err.Error()})
			return
		}
		response(w, 201, map[string]any{"stub": true})
	})
	mux.HandleFunc("GET /events", func(w http.ResponseWriter, r *http.Request) {
		after, err := strconv.ParseUint(r.URL.Query().Get("after"), 10, 64)
		if err != nil {
			response(w, 400, map[string]any{"error": "invalid cursor"})
			return
		}
		a.mu.Lock()
		defer a.mu.Unlock()
		if !a.owned(r) {
			response(w, 409, map[string]any{"error": "session ownership mismatch"})
			return
		}
		batch := Batch{Events: []Envelope{}, Cursor: a.cursor, Running: a.command != nil, Error: a.err}
		if len(a.events) > 0 && after+1 < a.events[0].Cursor {
			batch.Gap = true
		}
		for _, event := range a.events {
			if event.Cursor > after {
				batch.Events = append(batch.Events, event)
			}
		}
		response(w, 200, batch)
	})
	mux.HandleFunc("POST /command", func(w http.ResponseWriter, r *http.Request) {
		var command struct {
			ID      string         `json:"id"`
			Command string         `json:"command"`
			Params  map[string]any `json:"params,omitempty"`
		}
		if err := input(w, r, &command); err != nil {
			response(w, 400, map[string]any{"error": err.Error()})
			return
		}
		a.operations.Lock()
		defer a.operations.Unlock()
		a.mu.Lock()
		owned := a.owned(r)
		stdin := a.stdin
		a.mu.Unlock()
		if !owned || stdin == nil {
			response(w, 409, map[string]any{"error": "owned stub is not running"})
			return
		}
		// The native adapter validates capabilities and command parameters.
		if err := json.NewEncoder(stdin).Encode(command); err != nil {
			response(w, 503, map[string]any{"error": err.Error()})
			return
		}
		response(w, 202, map[string]any{"accepted": true})
	})
	mux.HandleFunc("POST /action", func(w http.ResponseWriter, r *http.Request) {
		var value struct {
			Action string `json:"action"`
		}
		if err := input(w, r, &value); err != nil {
			response(w, 400, map[string]any{"error": err.Error()})
			return
		}
		a.operations.Lock()
		defer a.operations.Unlock()
		a.mu.Lock()
		owned := a.owned(r)
		session := a.session
		command := a.command
		done := a.done
		a.mu.Unlock()
		if !owned {
			if session == nil && value.Action == "release" {
				response(w, 200, map[string]any{"ok": true, "absent": true})
				return
			}
			response(w, 409, map[string]any{"error": "session ownership mismatch"})
			return
		}
		var err error
		switch value.Action {
		case "stop", "release", "restart", "kill":
			if command != nil {
				sig := syscall.SIGTERM
				if value.Action == "kill" {
					sig = syscall.SIGKILL
				}
				err = signalJob(command, sig)
				select {
				case <-done:
				case <-time.After(8 * time.Second):
					_ = signalJob(command, syscall.SIGKILL)
					select {
					case <-done:
					case <-time.After(2 * time.Second):
						err = errors.New("stub termination did not complete")
					case <-r.Context().Done():
						err = r.Context().Err()
					}
				}
			}
			if err == nil && value.Action == "restart" {
				err = a.start(*session)
			}
			if err == nil && value.Action == "release" {
				a.mu.Lock()
				a.session = nil
				a.mu.Unlock()
			}
		case "start":
			if command != nil {
				err = errors.New("stub already running")
			} else {
				err = a.start(*session)
			}
		default:
			err = errors.New("unsupported physical lifecycle action")
		}
		if err != nil {
			response(w, 400, map[string]any{"error": err.Error()})
			return
		}
		response(w, 200, map[string]any{"ok": true})
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		expected := sha256.Sum256([]byte("Bearer " + a.Token))
		actual := sha256.Sum256([]byte(r.Header.Get("Authorization")))
		if a.Token == "" || subtle.ConstantTimeCompare(expected[:], actual[:]) != 1 {
			response(w, 401, map[string]any{"error": "unauthorized"})
			return
		}
		mux.ServeHTTP(w, r)
	})
}

func (a *Agent) start(session Session) error {
	if !safeID.MatchString(session.Run) || !safeID.MatchString(session.Node) || session.Config["nodeId"] != session.Node || session.Config["timeScale"] != float64(1) {
		return errors.New("invalid node identity or physical clock (1× required)")
	}
	if role := session.Config["role"]; role != "EG" && role != "CS" {
		return errors.New("stub role must be EG or CS")
	}
	if interfaces, ok := session.Config["interfaces"].([]any); ok {
		for _, value := range interfaces {
			name, ok := value.(string)
			if !ok {
				return errors.New("invalid interface")
			}
			if _, err := net.InterfaceByName(name); err != nil {
				return err
			}
		}
	} else {
		return errors.New("explicit physical interface required")
	}
	base := filepath.Join(a.Root, session.Run, session.Node)
	if err := os.MkdirAll(filepath.Join(base, "state"), 0700); err != nil {
		return err
	}
	for name, value := range map[string]string{"cert.pem": session.Certificate, "key.pem": session.PrivateKey} {
		if err := os.WriteFile(filepath.Join(base, name), []byte(value), 0600); err != nil {
			return err
		}
	}
	cfg := map[string]any{}
	for key, value := range session.Config {
		cfg[key] = value
	}
	cfg["certificate"] = filepath.Join(base, "cert.pem")
	cfg["privateKey"] = filepath.Join(base, "key.pem")
	cfg["stateDirectory"] = filepath.Join(base, "state")
	cfg["resourceScope"] = "device"
	blob, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	config := filepath.Join(base, "config.json")
	if err = os.WriteFile(config, blob, 0600); err != nil {
		return err
	}
	command := exec.Command(a.Supervisor, a.Native)
	command.Env = append(os.Environ(), "ARENA_CONFIG="+config)
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.WaitDelay = time.Second
	stdin, err := command.StdinPipe()
	if err != nil {
		return err
	}
	command.Stdout = &eventWriter{agent: a, onError: func() { _ = signalJob(command, syscall.SIGKILL) }}
	// Bounded log retention lives in the native event journal, not a growing device file.
	command.Stderr = &logWriter{agent: a}
	a.mu.Lock()
	if err = command.Start(); err != nil {
		a.mu.Unlock()
		stdin.Close()
		return err
	}
	a.events = nil
	a.session = &session
	a.command = command
	a.stdin = stdin
	a.done = make(chan struct{})
	done := a.done
	a.err = ""
	a.mu.Unlock()
	go func() {
		err := command.Wait()
		// Also terminate descendants if the supervisor exits unexpectedly.
		_ = signalJob(command, syscall.SIGKILL)
		stdin.Close()
		a.mu.Lock()
		a.command = nil
		a.stdin = nil
		if err != nil {
			a.err = err.Error()
		}
		close(done)
		a.mu.Unlock()
	}()
	a.publishJSON(map[string]any{"type": "external.stub", "nodeId": session.Node, "at": time.Now().UTC(), "data": map[string]any{"stub": true, "nativeSHA256": fileHash(a.Native), "resourceScope": "whole device", "clockRate": 1}})
	return nil
}

func signalJob(command *exec.Cmd, signal syscall.Signal) error {
	if command.Process == nil {
		return nil
	}
	err := syscall.Kill(-command.Process.Pid, signal)
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return err
}

// os/exec owns the copy goroutine, so WaitDelay bounds inherited output pipes.
type eventWriter struct {
	agent   *Agent
	pending []byte
	onError func()
}

func (w *eventWriter) Write(data []byte) (int, error) {
	size := len(data)
	for len(data) > 0 {
		end := bytes.IndexByte(data, '\n')
		part := data
		if end >= 0 {
			part = data[:end]
		}
		if len(w.pending)+len(part) > 1<<20 {
			w.onError()
			return 0, errors.New("physical stdout frame exceeds 1 MiB")
		}
		w.pending = append(w.pending, part...)
		if end < 0 {
			break
		}
		if json.Valid(w.pending) {
			w.agent.publish(append(json.RawMessage(nil), w.pending...))
		}
		w.pending = nil
		data = data[end+1:]
	}
	return size, nil
}

func fileHash(path string) string {
	file, err := os.Open(path)
	if err != nil {
		return "unavailable"
	}
	defer file.Close()
	h := sha256.New()
	if _, err = io.Copy(h, file); err != nil {
		return "unavailable"
	}
	return hex.EncodeToString(h.Sum(nil))
}
func (a *Agent) publish(event json.RawMessage) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.cursor++
	a.events = append(a.events, Envelope{a.cursor, event})
	if len(a.events) > 512 {
		a.events = a.events[len(a.events)-512:]
	}
}
func (a *Agent) publishJSON(value any) {
	blob, err := json.Marshal(value)
	if err == nil {
		a.publish(blob)
	}
}

type logWriter struct{ agent *Agent }

func (w *logWriter) Write(data []byte) (int, error) {
	size := len(data)
	if len(data) > 4096 {
		data = data[:4096]
	}
	w.agent.mu.Lock()
	node := ""
	if w.agent.session != nil {
		node = w.agent.session.Node
	}
	w.agent.mu.Unlock()
	w.agent.publishJSON(map[string]any{"type": "log", "nodeId": node, "at": time.Now().UTC(), "data": map[string]any{"line": string(data), "source": "physical-stub"}})
	return size, nil
}

// Shutdown joins the managed process without stopping any other firmware service.
func (a *Agent) Shutdown(ctx context.Context) error {
	a.operations.Lock()
	defer a.operations.Unlock()
	a.mu.Lock()
	command, done := a.command, a.done
	a.mu.Unlock()
	if command == nil {
		return nil
	}
	if err := signalJob(command, syscall.SIGTERM); err != nil {
		return fmt.Errorf("stop stub: %w", err)
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		_ = signalJob(command, syscall.SIGKILL)
		return ctx.Err()
	}
}
