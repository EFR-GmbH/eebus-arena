package arena

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"time"

	"github.com/EFR-GmbH/eebus-arena/internal/labagent"
)

// ExternalNode describes a managed physical stub, distinct from a container.
type ExternalNode struct {
	Label          string `json:"label"`
	Endpoint       string `json:"endpoint"`
	TLSFingerprint string `json:"tlsFingerprint"`
	Token          string `json:"token"`
	Interface      string `json:"interface"`
	Address        string `json:"address,omitempty"`
	IPv6Address    string `json:"ipv6Address,omitempty"`
}

// LabNetwork connects owned container nodes directly to a configurable lab interface.
type LabNetwork struct {
	Mode       string `json:"mode,omitempty"`
	Interface  string `json:"interface,omitempty"`
	Subnet     string `json:"subnet,omitempty"`
	IPRange    string `json:"ipRange,omitempty"`
	IPv6Subnet string `json:"ipv6Subnet,omitempty"`
	IPv6Range  string `json:"ipv6Range,omitempty"`
}

var interfaceName = regexp.MustCompile(`^[a-zA-Z0-9_.-]{1,15}$`)

func (d Definition) validatePhysical() error {
	hasExternal := false
	hasContainer := false
	seen := map[string]bool{}
	for _, group := range d.Groups {
		if group.External == nil {
			hasContainer = true
			continue
		}
		hasExternal = true
		external := group.External
		if group.Count != 1 || group.Role == "STRESSER" || d.TimeScale != 1 {
			return errors.New("physical stubs require one EG or CS at 1×")
		}
		if external.Label == "" || len(external.Label) > 120 || !interfaceName.MatchString(external.Interface) {
			return errors.New("physical stub needs a label and explicit device interface")
		}
		if external.Address != "" && net.ParseIP(external.Address).To4() == nil {
			return errors.New("physical protocol address must be a literal IPv4 address")
		}
		if external.IPv6Address != "" {
			ip := net.ParseIP(external.IPv6Address)
			if ip == nil || ip.To4() != nil || ip.IsUnspecified() || ip.IsMulticast() || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
				return errors.New("physical IPv6 protocol address must be a global or ULA literal")
			}
		}
		endpoint, err := url.Parse(external.Endpoint)
		if err != nil || endpoint.Scheme != "https" || endpoint.Host == "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" || endpoint.Path != "" {
			return errors.New("external endpoint must be an HTTPS origin")
		}
		if seen[endpoint.Host] {
			return errors.New("one physical lease per endpoint")
		}
		seen[endpoint.Host] = true
		for _, value := range []string{external.TLSFingerprint, external.Token} {
			decoded, err := hex.DecodeString(value)
			if err != nil || len(decoded) != 32 {
				return errors.New("external TLS fingerprint and token must be 32-byte hex values")
			}
		}
		limits := d.resourcesFor(group.ID+"-001", group.ID)
		for _, value := range []*int{limits.CPUMillis, limits.MemoryMiB, limits.PIDsLimit} {
			if value != nil && *value != 0 {
				return errors.New("container resource limits cannot constrain a physical device")
			}
		}
	}
	if hasExternal && (d.Network == nil || d.Network.Interface == "") {
		return errors.New("physical arenas require an explicit lab network")
	}
	if hasExternal && !hasContainer {
		return errors.New("physical arenas need at least one container peer")
	}
	return d.validateNetwork()
}

func externalClient(external *ExternalNode) *http.Client {
	return &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("external redirects are forbidden") }, Transport: &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS13, InsecureSkipVerify: true, // Verified by the explicitly pinned certificate below.
		VerifyConnection: func(state tls.ConnectionState) error {
			if len(state.PeerCertificates) != 1 {
				return errors.New("expected one pinned agent certificate")
			}
			cert := state.PeerCertificates[0]
			hash := sha256.Sum256(cert.Raw)
			if hex.EncodeToString(hash[:]) != external.TLSFingerprint {
				return errors.New("agent TLS fingerprint mismatch")
			}
			now := time.Now()
			if now.Before(cert.NotBefore) || now.After(cert.NotAfter) {
				return errors.New("agent certificate expired or not yet valid")
			}
			return nil
		}}, MaxIdleConnsPerHost: 2, ResponseHeaderTimeout: 10 * time.Second}}
}
func externalRequest(ctx context.Context, external *ExternalNode, method, path, run, node string, value any, result any) error {
	var body io.Reader
	if value != nil {
		blob, err := json.Marshal(value)
		if err != nil {
			return err
		}
		body = bytes.NewReader(blob)
	}
	query := url.Values{"run": {run}, "node": {node}}
	request, err := http.NewRequestWithContext(ctx, method, external.Endpoint+path+"?"+query.Encode(), body)
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+external.Token)
	request.Header.Set("Content-Type", "application/json")
	client := externalClient(external)
	defer client.CloseIdleConnections()
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		var value struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&value)
		return fmt.Errorf("physical agent: %s (%d)", value.Error, response.StatusCode)
	}
	if result != nil {
		return json.NewDecoder(io.LimitReader(response.Body, 4<<20)).Decode(result)
	}
	return nil
}

func (c *Controller) startPhysical(ctx context.Context, run *Run) error {
	for _, node := range run.Nodes {
		if node.External == nil {
			continue
		}
		var config map[string]any
		blob, err := os.ReadFile(c.path(run.ID, "identities/"+node.ID+"/config.json"))
		if err != nil {
			return err
		}
		if err = json.Unmarshal(blob, &config); err != nil {
			return err
		}
		config["interfaces"] = []string{node.External.Interface}
		cert, err := os.ReadFile(c.path(run.ID, "identities/"+node.ID+"/cert.pem"))
		if err != nil {
			return err
		}
		key, err := os.ReadFile(c.path(run.ID, "identities/"+node.ID+"/key.pem"))
		if err != nil {
			return err
		}
		session := labagent.Session{Run: run.ID, Node: node.ID, Config: config, Certificate: string(cert), PrivateKey: string(key)}
		if err = externalRequest(ctx, node.External, "POST", "/session", run.ID, node.ID, session, nil); err != nil {
			return fmt.Errorf("%s: %w", node.ID, err)
		}
		c.mu.Lock()
		node.Lifecycle = "running"
		c.startPhysicalWatcherLocked(run.ID, node)
		c.mu.Unlock()
	}
	return nil
}
func (c *Controller) startPhysicalWatcherLocked(run string, node *Node) {
	key := run + "/" + node.ID
	if c.closed || c.watchers[key] != nil {
		return
	}
	ctx, cancel := context.WithCancel(c.ctx)
	c.watchers[key] = cancel
	c.workers.Add(1)
	external := *node.External
	nodeID := node.ID
	go func() { defer c.workers.Done(); c.watchPhysical(ctx, run, nodeID, &external) }()
}
func (c *Controller) watchPhysical(ctx context.Context, run, node string, external *ExternalNode) {
	after := uint64(0)
	timer := time.NewTicker(500 * time.Millisecond)
	defer timer.Stop()
	client := externalClient(external)
	defer client.CloseIdleConnections()
	lastError := ""
	for {
		var batch labagent.Batch
		// The cursor is an agent transport cursor, independent of native sequence numbers.
		query := url.Values{"run": {run}, "node": {node}, "after": {fmt.Sprint(after)}}
		request, err := http.NewRequestWithContext(ctx, "GET", external.Endpoint+"/events?"+query.Encode(), nil)
		if err == nil {
			request.Header.Set("Authorization", "Bearer "+external.Token)
			response, callErr := client.Do(request)
			if callErr != nil {
				err = callErr
			} else {
				if response.StatusCode != 200 {
					err = fmt.Errorf("physical stream HTTP %d", response.StatusCode)
				} else {
					err = json.NewDecoder(io.LimitReader(response.Body, 4<<20)).Decode(&batch)
				}
				response.Body.Close()
			}
		}
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			if err.Error() != lastError {
				c.Emit(Event{RunID: run, NodeID: node, Type: "external.unavailable", Data: map[string]any{"error": err.Error()}})
				lastError = err.Error()
				c.mu.Lock()
				if _, n, lookupErr := c.nodeLocked(run, node); lookupErr == nil {
					n.Lifecycle = "unavailable"
					c.dirty[run] = true
				}
				c.mu.Unlock()
			}
		} else {
			if ctx.Err() != nil {
				return
			}
			if batch.Gap && after != 0 {
				c.Emit(Event{RunID: run, NodeID: node, Type: "telemetry.gap", Data: map[string]any{"reason": "physical agent transport retention"}})
			}
			for _, envelope := range batch.Events {
				var event Event
				if json.Unmarshal(envelope.Event, &event) != nil || event.NodeID != node {
					continue
				}
				event.RunID = run
				sourceAt := event.At
				event.SourceAt = &sourceAt
				event.At = time.Now().UTC()
				if event.Type == "resource.process" {
					event.Data["deviceAt"] = event.SourceAt
					event.Data["at"] = event.At
					event.Data["clockOffsetSeconds"] = event.At.Sub(*event.SourceAt).Seconds()
				}
				if event.Incarnation == "" {
					c.Emit(event)
				} else {
					c.acceptFrom(event, "")
				}
			}
			after = batch.Cursor
			lastError = ""
			c.mu.Lock()
			if _, n, err := c.nodeLocked(run, node); err == nil {
				state := "exited"
				if batch.Running {
					state = "running"
				}
				if n.Lifecycle != state {
					n.Lifecycle = state
					c.emitLocked(Event{RunID: run, NodeID: node, Type: "external.state", Data: map[string]any{"state": state, "error": batch.Error}})
				}
			}
			c.mu.Unlock()
		}
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
	}
}

func (c *Controller) physicalAction(ctx context.Context, run, node, action string, external *ExternalNode) error {
	if !Contains([]string{"start", "stop", "kill", "restart"}, action) {
		return errors.New("unsupported physical lifecycle action")
	}
	c.mu.Lock()
	_, n, err := c.nodeLocked(run, node)
	if err != nil {
		c.mu.Unlock()
		return err
	}
	c.stopWatcherLocked(run, node)
	c.resetNodeLocked(run, n, time.Now().UTC())
	c.mu.Unlock()
	c.Emit(Event{RunID: run, NodeID: node, Type: "action.lifecycle", Data: map[string]any{"action": action, "scope": "managed physical stub"}})
	err = externalRequest(ctx, external, "POST", "/action", run, node, map[string]string{"action": action}, nil)
	c.mu.Lock()
	if _, n, lookupErr := c.nodeLocked(run, node); lookupErr == nil {
		c.startPhysicalWatcherLocked(run, n)
	}
	c.mu.Unlock()
	return err
}

func (c *Controller) stopPhysical(ctx context.Context, run *Run) error {
	var failures []error
	for _, node := range run.Nodes {
		if node.External != nil {
			if err := externalRequest(ctx, node.External, "POST", "/action", run.ID, node.ID, map[string]string{"action": "release"}, nil); err != nil {
				failures = append(failures, fmt.Errorf("release %s: %w", node.ID, err))
			}
		}
	}
	return errors.Join(failures...)
}
