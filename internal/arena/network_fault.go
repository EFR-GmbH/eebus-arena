package arena

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// NetworkFault selects a directional peer flow; times are always real infrastructure time.
type NetworkFault struct {
	Family          string  `json:"family,omitempty"`
	From            string  `json:"from"`
	To              string  `json:"to"`
	Traffic         string  `json:"traffic"`
	DelayMillis     int     `json:"delayMillis,omitempty"`
	JitterMillis    int     `json:"jitterMillis,omitempty"`
	LossPercent     float64 `json:"lossPercent,omitempty"`
	RateKbit        int     `json:"rateKbit,omitempty"`
	Partition       bool    `json:"partition,omitempty"`
	DurationSeconds int     `json:"durationSeconds"`
}
type FaultLease struct {
	ID        string       `json:"id"`
	Run       string       `json:"run"`
	Node      string       `json:"node"`
	Container string       `json:"container"`
	Helper    string       `json:"helper"`
	Profile   NetworkFault `json:"profile"`
	Image     string       `json:"image,omitempty"`
	Peer      string       `json:"peer,omitempty"`
	Ingress   bool         `json:"ingress,omitempty"`
	Started   time.Time    `json:"started"`
	State     string       `json:"state"`
	cancel    context.CancelFunc
	done      chan struct{}
}

func (f NetworkFault) Validate() error {
	if !Contains([]string{"", "ipv4", "ipv6"}, f.Family) {
		return errors.New("fault family must be ipv4 or ipv6")
	}
	if f.From == f.To || !safeID.MatchString(f.From) || !safeID.MatchString(f.To) || !Contains([]string{"unicast", "mdns"}, f.Traffic) {
		return errors.New("fault requires distinct node IDs and unicast or mdns traffic")
	}
	if f.DelayMillis < 0 || f.DelayMillis > 10000 || f.JitterMillis < 0 || f.JitterMillis > f.DelayMillis || math.IsNaN(f.LossPercent) || math.IsInf(f.LossPercent, 0) || f.LossPercent < 0 || f.LossPercent > 100 || f.RateKbit < 0 || f.RateKbit > 1000000 || f.DurationSeconds < 1 || f.DurationSeconds > 600 {
		return errors.New("invalid fault bounds (maximum lifetime 600 real seconds)")
	}
	if !f.Partition && f.DelayMillis == 0 && f.LossPercent == 0 && f.RateKbit == 0 {
		return errors.New("fault needs an impairment")
	}
	return nil
}

func (c *Controller) faultImage() string {
	if value := os.Getenv("ARENA_NETFAULT_IMAGE"); immutableImageID.MatchString(value) {
		return value
	}
	var helper struct {
		Image string `json:"image"`
	}
	if blob, err := os.ReadFile(c.data + "/netfault.json"); err == nil && json.Unmarshal(blob, &helper) == nil && immutableImageID.MatchString(helper.Image) {
		return helper.Image
	}
	return ""
}
func (c *Controller) nodeIP(ctx context.Context, run, node, family string) (string, string, error) {
	c.mu.Lock()
	_, n, err := c.nodeLocked(run, node)
	if err != nil {
		c.mu.Unlock()
		return "", "", err
	}
	container := n.Container
	external := n.External
	c.mu.Unlock()
	if external != nil {
		address := external.Address
		if family == "ipv6" {
			address = external.IPv6Address
		}
		if address == "" {
			return "", "", errors.New("physical faults need the explicit protocol address for the selected family; an agent endpoint may use another address")
		}
		return "", address, nil
	}
	if container == "" || c.docker == nil {
		return "", "", errors.New("node has no owned container")
	}
	if err = c.docker.Owned(ctx, container, run, node); err != nil {
		return "", "", err
	}
	var inspect struct {
		NetworkSettings struct {
			Networks map[string]struct{ IPAddress, GlobalIPv6Address string }
		}
	}
	if err = c.docker.JSON(ctx, "GET", "/containers/"+container+"/json", nil, &inspect); err != nil {
		return "", "", err
	}
	if len(inspect.NetworkSettings.Networks) != 1 {
		return "", "", errors.New("fault requires one unambiguous arena network")
	}
	for _, network := range inspect.NetworkSettings.Networks {
		address := network.IPAddress
		if family == "ipv6" {
			address = network.GlobalIPv6Address
		}
		if ip := net.ParseIP(address); ip != nil && (family == "ipv6") == (ip.To4() == nil) && !ip.IsUnspecified() && !ip.IsMulticast() {
			return container, address, nil
		}
	}
	return "", "", fmt.Errorf("node %s address unavailable", family)
}

// ApplyNetworkFault uses a separate helper with NET_ADMIN only in the owned namespace.
func (c *Controller) ApplyNetworkFault(ctx context.Context, run string, profile NetworkFault) (FaultLease, error) {
	if err := profile.Validate(); err != nil {
		return FaultLease{}, err
	}
	image := c.faultImage()
	if image == "" {
		return FaultLease{}, errors.New("network faults unsupported: install the pinned netfault helper")
	}
	enforced := profile.From
	ingress := false
	if profile.Traffic == "mdns" {
		enforced = profile.To
		ingress = true
	}
	unlock, err := c.mutation(ctx, run, enforced)
	if err != nil {
		return FaultLease{}, err
	}
	var unlockOnce sync.Once
	release := func() { unlockOnce.Do(unlock) }
	defer release()
	c.mu.Lock()
	r, n, err := c.nodeLocked(run, enforced)
	if err != nil || c.closed || !reconcilable(r.Status) || n.External != nil {
		c.mu.Unlock()
		return FaultLease{}, errors.New("fault enforcer must be an active owned container")
	}
	mode := r.Definition.networkMode()
	if profile.Family == "" {
		profile.Family = "ipv4"
		if mode == "ipv6" {
			profile.Family = "ipv6"
		}
	}
	if mode != "dual-stack" && profile.Family != mode {
		c.mu.Unlock()
		return FaultLease{}, errors.New("fault family is not enabled in this arena")
	}
	if c.faults == nil {
		c.faults = map[string]*FaultLease{}
	}
	if len(c.faults) >= 128 {
		c.mu.Unlock()
		return FaultLease{}, errors.New("at most 128 simultaneous faults")
	}
	for _, lease := range c.faults {
		if lease.Run == run && lease.Node == enforced {
			c.mu.Unlock()
			return FaultLease{}, errors.New("node already has a network fault; restore it first")
		}
	}
	c.workers.Add(1)
	c.mu.Unlock()
	defer c.workers.Done()
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(c.ctx, cancel)
	defer stop()
	defer cancel()
	fromContainer, fromIP, err := c.nodeIP(ctx, run, profile.From, profile.Family)
	if err != nil {
		return FaultLease{}, err
	}
	toContainer, toIP, err := c.nodeIP(ctx, run, profile.To, profile.Family)
	if err != nil {
		return FaultLease{}, err
	}
	if profile.Traffic == "mdns" && profile.Family == "ipv6" {
		if fromContainer == "" {
			return FaultLease{}, errors.New("IPv6 mDNS faults require a container sender so its link-local source can be inspected")
		}
		fromIP, err = c.ipv6MDNSSource(ctx, fromContainer, image, run, profile.From)
		if err != nil {
			return FaultLease{}, err
		}
	}
	container, peer := fromContainer, toIP
	if ingress {
		container, peer = toContainer, fromIP
	}
	if container == "" {
		return FaultLease{}, errors.New("physical network interfaces cannot be impaired by the container helper")
	}
	identifier := "fault-" + id()
	lease := &FaultLease{ID: identifier, Run: run, Node: enforced, Container: container, Profile: profile, Image: image, Peer: peer, Ingress: ingress, Started: time.Now().UTC(), State: "starting", cancel: cancel, done: make(chan struct{})}
	c.mu.Lock()
	// Namespace inspection can overlap faults on other nodes. Reserve under
	// the same lock as the final bound check, not only the earlier preflight.
	if len(c.faults) >= 128 {
		c.mu.Unlock()
		return FaultLease{}, errors.New("at most 128 simultaneous faults")
	}
	c.faults[identifier] = lease
	c.mu.Unlock()
	keep := false
	defer func() {
		if !keep {
			c.mu.Lock()
			delete(c.faults, identifier)
			c.persistFaultsLocked(run)
			c.mu.Unlock()
		}
		close(lease.done)
	}()
	loss := profile.LossPercent
	if profile.Partition {
		loss = 100
	}
	args := []string{"--lease", identifier, "--peer", peer, "--delay", strconv.Itoa(profile.DelayMillis), "--jitter", strconv.Itoa(profile.JitterMillis), "--loss", strconv.FormatFloat(loss, 'f', 3, 64), "--rate", strconv.Itoa(profile.RateKbit), "--duration", strconv.Itoa(profile.DurationSeconds)}
	if ingress {
		args = append(args, "--ingress")
	}
	if profile.Traffic == "mdns" {
		args = append(args, "--mdns")
	}
	config := faultHelperConfig(*lease, args)
	var created struct {
		ID string `json:"Id"`
	}
	if err = c.docker.JSON(ctx, "POST", "/containers/create?name=eebus-"+identifier, config, &created); err != nil {
		return FaultLease{}, err
	}
	c.mu.Lock()
	lease.Helper = created.ID
	c.persistFaultsLocked(run)
	c.mu.Unlock()
	cleanupCreated := func() {
		cleanup, cancelCleanup := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancelCleanup()
		cleanupErr := c.helperOwned(cleanup, *lease)
		if cleanupErr == nil {
			cleanupErr = c.docker.JSON(cleanup, "POST", "/containers/"+created.ID+"/stop?t=10", nil, nil)
		}
		keep = true
		c.mu.Lock()
		lease.State = "cleanup_unverified"
		c.persistFaultsLocked(run)
		c.mu.Unlock()
		// Release this startup mutation before recovery takes the same gate.
		// The retained lease still prevents any replacement fault.
		release()
		if cleanupErr == nil {
			cleanupErr = c.recoverFault(cleanup, identifier)
		}
		if cleanupErr != nil {
			c.Emit(Event{RunID: run, NodeID: enforced, Type: "fault.cleanup_unverified", Data: map[string]any{"id": identifier, "error": cleanupErr.Error()}})
		}
	}
	if err = c.docker.JSON(ctx, "POST", "/containers/"+created.ID+"/start", nil, nil); err != nil {
		cleanupCreated()
		return FaultLease{}, err
	}
	c.mu.Lock()
	r, n, err = c.nodeLocked(run, enforced)
	if err != nil || c.closed || !reconcilable(r.Status) || n.Container != container || ctx.Err() != nil {
		c.mu.Unlock()
		cleanupCreated()
		return FaultLease{}, errors.New("fault namespace changed during startup")
	}
	keep = true
	c.persistFaultsLocked(run)
	c.workers.Add(1)
	snapshot := *lease
	c.mu.Unlock()
	c.Emit(Event{RunID: run, NodeID: enforced, Type: "fault.requested", Data: map[string]any{"id": identifier, "profile": profile, "namespace": container, "image": image}})
	go func() { defer c.workers.Done(); c.watchFault(identifier) }()
	return snapshot, nil
}
func (c *Controller) persistFaultsLocked(run string) {
	if r := c.runs[run]; r != nil {
		leases := []FaultLease{}
		for _, lease := range c.faults {
			if lease.Run == run {
				leases = append(leases, *lease)
			}
		}
		r.Runtime["faults"] = leases
		c.saveLocked(r)
	}
}
func (c *Controller) helperOwned(ctx context.Context, lease FaultLease) error {
	var info struct {
		Config struct{ Labels map[string]string }
	}
	if err := c.docker.JSON(ctx, "GET", "/containers/"+lease.Helper+"/json", nil, &info); err != nil {
		return err
	}
	if info.Config.Labels["org.eebus-arena.fault"] != lease.ID || info.Config.Labels["org.eebus-arena.helper.run"] != lease.Run || info.Config.Labels["org.eebus-arena.helper.node"] != lease.Node {
		return errors.New("fault helper ownership mismatch")
	}
	return nil
}
func (c *Controller) watchFault(identifier string) {
	c.mu.Lock()
	currentLease := c.faults[identifier]
	if currentLease == nil {
		c.mu.Unlock()
		return
	}
	lease := *currentLease
	c.mu.Unlock()
	ctx, cancel := context.WithTimeout(c.ctx, time.Duration(lease.Profile.DurationSeconds+30)*time.Second)
	defer cancel()
	err := c.docker.Logs(ctx, lease.Helper, time.Time{}, func(_ byte, line string) {
		parts := strings.SplitN(line, " ", 2)
		if len(parts) != 2 {
			return
		}
		var data map[string]any
		if json.Unmarshal([]byte(parts[1]), &data) != nil {
			return
		}
		kind, _ := data["type"].(string)
		if kind != "fault.applied" && kind != "fault.restored" && kind != "fault.cleanup_failed" && kind != "fault.statistics" {
			return
		}
		data["id"] = identifier
		c.Emit(Event{RunID: lease.Run, NodeID: lease.Node, Type: kind, Data: data})
		c.mu.Lock()
		if current := c.faults[identifier]; current != nil && kind != "fault.statistics" {
			current.State = strings.TrimPrefix(kind, "fault.")
			c.persistFaultsLocked(lease.Run)
		}
		c.mu.Unlock()
	})
	if errors.Is(err, io.EOF) {
		err = nil
	}
	if ctx.Err() != nil {
		return
	}
	var wait struct {
		StatusCode int
		Error      *struct{ Message string }
	}
	if waitErr := c.docker.JSON(ctx, "POST", "/containers/"+lease.Helper+"/wait?condition=not-running", nil, &wait); waitErr != nil {
		err = waitErr
	}
	c.mu.Lock()
	current := c.faults[identifier]
	restored := current != nil && current.State == "restored"
	c.mu.Unlock()
	if err != nil || wait.StatusCode != 0 || !restored {
		c.mu.Lock()
		if current := c.faults[identifier]; current != nil {
			current.State = "cleanup_unverified"
			c.persistFaultsLocked(lease.Run)
		}
		c.mu.Unlock()
		c.Emit(Event{RunID: lease.Run, NodeID: lease.Node, Type: "fault.cleanup_unverified", Data: map[string]any{"id": identifier, "exitCode": wait.StatusCode, "error": fmt.Sprint(err)}})
		recovery, stop := context.WithTimeout(c.ctx, 20*time.Second)
		defer stop()
		if recoverErr := c.recoverFault(recovery, identifier); recoverErr != nil {
			c.Emit(Event{RunID: lease.Run, NodeID: lease.Node, Type: "fault.recovery_failed", Data: map[string]any{"id": identifier, "error": recoverErr.Error()}})
		}
		return
	}
	cleanup, cancelCleanup := context.WithTimeout(c.ctx, 10*time.Second)
	defer cancelCleanup()
	if err = c.helperOwned(cleanup, lease); err == nil {
		err = c.docker.JSON(cleanup, "DELETE", "/containers/"+lease.Helper, nil, nil)
	}
	if err == nil {
		c.mu.Lock()
		delete(c.faults, identifier)
		c.persistFaultsLocked(lease.Run)
		c.mu.Unlock()
	}
}

// RestoreNetworkFault signals only the matching helper, whose handler restores its rules.
func (c *Controller) RestoreNetworkFault(ctx context.Context, run, identifier string) error {
	c.mu.Lock()
	lease := c.faults[identifier]
	if lease == nil || lease.Run != run {
		c.mu.Unlock()
		if lease == nil {
			return nil
		}
		return errors.New("fault belongs to another arena")
	}
	snapshot := *lease
	c.mu.Unlock()
	if snapshot.State == "starting" && snapshot.done != nil {
		if snapshot.cancel != nil {
			snapshot.cancel()
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-snapshot.done:
		}
		c.mu.Lock()
		lease = c.faults[identifier]
		if lease == nil {
			c.mu.Unlock()
			return nil
		}
		snapshot = *lease
		c.mu.Unlock()
	}
	if Contains([]string{"cleanup_failed", "cleanup_unverified"}, snapshot.State) {
		return c.recoverFault(ctx, identifier)
	}
	if err := c.helperOwned(ctx, snapshot); err != nil {
		c.mu.Lock()
		gone := c.faults[identifier] == nil
		c.mu.Unlock()
		if gone {
			return nil
		}
		if errors.Is(err, ErrDockerNotFound) {
			return c.recoverFault(ctx, identifier)
		}
		return err
	}
	if err := c.docker.JSON(ctx, "POST", "/containers/"+snapshot.Helper+"/stop?t=10", nil, nil); err != nil {
		return err
	}
	// Stop only requests restoration. Wait for the helper's evidence and removal.
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		c.mu.Lock()
		current := c.faults[identifier]
		gone := current == nil
		failed := current != nil && Contains([]string{"cleanup_failed", "cleanup_unverified"}, current.State)
		c.mu.Unlock()
		if gone {
			return nil
		}
		if failed {
			return c.recoverFault(ctx, identifier)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func faultHelperConfig(lease FaultLease, args []string) map[string]any {
	return map[string]any{"Image": lease.Image, "Cmd": args, "Labels": map[string]string{"org.eebus-arena.helper.run": lease.Run, "org.eebus-arena.helper.node": lease.Node, "org.eebus-arena.fault": lease.ID}, "HostConfig": map[string]any{"NetworkMode": "container:" + lease.Container, "CapDrop": []string{"ALL"}, "CapAdd": []string{"NET_ADMIN"}, "ReadonlyRootfs": true, "SecurityOpt": []string{"no-new-privileges:true"}, "Memory": 64 << 20, "MemorySwap": 64 << 20, "NanoCpus": 250000000, "PidsLimit": 64, "LogConfig": map[string]any{"Type": "json-file", "Config": map[string]string{"max-size": "1m", "max-file": "1"}}}}
}

// A killed helper cannot run a signal handler. Recover using a second bounded
// helper in the same ownership-verified namespace and require native cleanup
// evidence before removing the persisted lease.
func (c *Controller) recoverFault(ctx context.Context, identifier string) error {
	c.mu.Lock()
	lease := c.faults[identifier]
	if lease == nil {
		c.mu.Unlock()
		return nil
	}
	snapshot := *lease
	c.mu.Unlock()
	release, err := c.mutation(ctx, snapshot.Run, snapshot.Node)
	if err != nil {
		return err
	}
	defer release()
	c.mu.Lock()
	lease = c.faults[identifier]
	if lease == nil {
		c.mu.Unlock()
		return nil
	}
	snapshot = *lease
	c.mu.Unlock()
	if !immutableImageID.MatchString(snapshot.Image) {
		return errors.New("fault has no pinned recovery helper image")
	}
	if err = c.docker.Owned(ctx, snapshot.Container, snapshot.Run, snapshot.Node); err != nil {
		if !errors.Is(err, ErrDockerNotFound) {
			return err
		}
		// A helper can retain the namespace after its node was removed. Verify
		// ownership and remove that final reference before retiring the lease.
		if helperErr := c.helperOwned(ctx, snapshot); helperErr == nil {
			if err = c.docker.JSON(ctx, "POST", "/containers/"+snapshot.Helper+"/stop?t=10", nil, nil); err != nil && !errors.Is(err, ErrDockerNotFound) {
				return err
			}
			if err = c.docker.JSON(ctx, "DELETE", "/containers/"+snapshot.Helper, nil, nil); err != nil && !errors.Is(err, ErrDockerNotFound) {
				return err
			}
		} else if !errors.Is(helperErr, ErrDockerNotFound) {
			return helperErr
		}
		c.mu.Lock()
		delete(c.faults, identifier)
		c.persistFaultsLocked(snapshot.Run)
		c.emitLocked(Event{RunID: snapshot.Run, NodeID: snapshot.Node, Type: "fault.restored", Data: map[string]any{"id": identifier, "reason": "owned node and helper removed; namespace retired"}})
		c.mu.Unlock()
		return nil
	}
	missingHelper := false
	if err = c.helperOwned(ctx, snapshot); err != nil {
		if !errors.Is(err, ErrDockerNotFound) {
			return err
		}
		missingHelper = true
	}
	var original struct{ State struct{ Running bool } }
	if !missingHelper {
		if err = c.docker.JSON(ctx, "GET", "/containers/"+snapshot.Helper+"/json", nil, &original); err != nil {
			return err
		}
	}
	if original.State.Running {
		return errors.New("cannot recover rules while the original helper is running")
	}
	args := []string{"--lease", snapshot.ID, "--restore-only"}
	if snapshot.Ingress {
		args = append(args, "--ingress")
	}
	var created struct {
		ID string `json:"Id"`
	}
	if err = c.docker.JSON(ctx, "POST", "/containers/create", faultHelperConfig(snapshot, args), &created); err != nil {
		return err
	}
	cleaner := snapshot
	cleaner.Helper = created.ID
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if c.helperOwned(cleanup, cleaner) == nil {
			_ = c.docker.JSON(cleanup, "POST", "/containers/"+cleaner.Helper+"/stop?t=5", nil, nil)
			_ = c.docker.JSON(cleanup, "DELETE", "/containers/"+cleaner.Helper, nil, nil)
		}
	}()
	if err = c.docker.JSON(ctx, "POST", "/containers/"+cleaner.Helper+"/start", nil, nil); err != nil {
		return err
	}
	restored := false
	err = c.docker.Logs(ctx, cleaner.Helper, time.Time{}, func(_ byte, line string) {
		_, data, ok := strings.Cut(line, " ")
		if !ok {
			return
		}
		var result map[string]any
		if json.Unmarshal([]byte(data), &result) != nil {
			return
		}
		if result["type"] == "fault.restored" {
			restored = true
			result["id"] = identifier
			c.Emit(Event{RunID: snapshot.Run, NodeID: snapshot.Node, Type: "fault.restored", Data: result})
		}
	})
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	var wait struct{ StatusCode int }
	if err = c.docker.JSON(ctx, "POST", "/containers/"+cleaner.Helper+"/wait?condition=not-running", nil, &wait); err != nil {
		return err
	}
	if wait.StatusCode != 0 || !restored {
		return errors.New("recovery helper did not verify restoration; lease retained")
	}
	if !missingHelper {
		if err = c.helperOwned(ctx, snapshot); err != nil && !errors.Is(err, ErrDockerNotFound) {
			return err
		}
		if err == nil {
			if err = c.docker.JSON(ctx, "DELETE", "/containers/"+snapshot.Helper, nil, nil); err != nil && !errors.Is(err, ErrDockerNotFound) {
				return err
			}
		}
	}
	c.mu.Lock()
	delete(c.faults, identifier)
	c.persistFaultsLocked(snapshot.Run)
	c.mu.Unlock()
	return nil
}
func (c *Controller) restoreRunFaults(ctx context.Context, run string) error {
	c.mu.Lock()
	ids := []string{}
	for id, lease := range c.faults {
		if lease.Run == run {
			ids = append(ids, id)
		}
	}
	c.mu.Unlock()
	var failures []error
	for _, id := range ids {
		if err := c.RestoreNetworkFault(ctx, run, id); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

func (c *Controller) restoreFaultWatchers() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.faults == nil {
		c.faults = map[string]*FaultLease{}
	}
	for _, run := range c.runs {
		blob, err := json.Marshal(run.Runtime["faults"])
		if err != nil {
			continue
		}
		var leases []FaultLease
		if json.Unmarshal(blob, &leases) != nil {
			continue
		}
		for _, lease := range leases {
			if lease.Run != run.ID || !safeID.MatchString(lease.ID) || lease.Helper == "" {
				continue
			}
			if _, _, err := c.nodeLocked(run.ID, lease.Node); err != nil {
				continue
			}
			copy := lease
			c.faults[lease.ID] = &copy
			c.workers.Add(1)
			go func(id string) {
				defer c.workers.Done()
				ctx, cancel := context.WithTimeout(c.ctx, 10*time.Second)
				defer cancel()
				if err := c.helperOwned(ctx, copy); err != nil {
					c.Emit(Event{RunID: copy.Run, NodeID: copy.Node, Type: "fault.cleanup_unverified", Data: map[string]any{"id": id, "error": err.Error()}})
					return
				}
				c.watchFault(id)
			}(lease.ID)
		}
	}
}

// IPv6 multicast uses a link-local source, distinct from Docker's ULA used by
// unicast SHIP. Inspect the actual namespace with no capabilities or writable
// root filesystem; never guess an EUI-64 address from its MAC.
func (c *Controller) ipv6MDNSSource(ctx context.Context, container, image, run, node string) (string, error) {
	if err := c.docker.Owned(ctx, container, run, node); err != nil {
		return "", err
	}
	config := addressInspectionConfig(container, image, run, node)
	var created struct {
		ID string `json:"Id"`
	}
	if err := c.docker.JSON(ctx, "POST", "/containers/create", config, &created); err != nil {
		return "", err
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = c.docker.JSON(cleanup, "DELETE", "/containers/"+created.ID+"?force=true", nil, nil)
	}()
	if err := c.docker.JSON(ctx, "POST", "/containers/"+created.ID+"/start", nil, nil); err != nil {
		return "", err
	}
	var addresses []string
	err := c.docker.Logs(ctx, created.ID, time.Time{}, func(stream byte, line string) {
		_, body, ok := strings.Cut(line, " ")
		if !ok || stream != 1 {
			return
		}
		var result struct {
			Addresses []string `json:"addresses"`
		}
		if json.Unmarshal([]byte(body), &result) == nil {
			addresses = result.Addresses
		}
	})
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	return ipv6LinkLocal(addresses)
}

func addressInspectionConfig(container, image, run, node string) map[string]any {
	return map[string]any{"Image": image, "Cmd": []string{"--addresses"}, "Labels": map[string]string{"org.eebus-arena.helper.run": run, "org.eebus-arena.helper.node": node, "org.eebus-arena.address-inspection": "true"}, "HostConfig": map[string]any{"NetworkMode": "container:" + container, "CapDrop": []string{"ALL"}, "ReadonlyRootfs": true, "SecurityOpt": []string{"no-new-privileges:true"}, "Memory": 32 << 20, "MemorySwap": 32 << 20, "NanoCpus": 250000000, "PidsLimit": 16, "LogConfig": map[string]any{"Type": "json-file", "Config": map[string]string{"max-size": "1m", "max-file": "1"}}}}
}

// Inspection helpers are short-lived and have no lease. Recover only helpers
// marked for inspection and belonging to a node in this controller's data.
func (c *Controller) cleanupAddressInspections(ctx context.Context) error {
	filters, err := json.Marshal(map[string][]string{"label": {"org.eebus-arena.address-inspection=true"}})
	if err != nil {
		return err
	}
	var containers []struct {
		ID     string `json:"Id"`
		Labels map[string]string
	}
	if err := c.docker.JSON(ctx, "GET", "/containers/json?all=true&filters="+url.QueryEscape(string(filters)), nil, &containers); err != nil {
		return err
	}
	for _, container := range containers {
		run, node := container.Labels["org.eebus-arena.helper.run"], container.Labels["org.eebus-arena.helper.node"]
		c.mu.Lock()
		_, _, err := c.nodeLocked(run, node)
		c.mu.Unlock()
		if err != nil || container.Labels["org.eebus-arena.address-inspection"] != "true" {
			continue
		}
		var inspected struct {
			Config     struct{ Labels map[string]string }
			HostConfig struct{ NetworkMode string }
		}
		if err := c.docker.JSON(ctx, "GET", "/containers/"+container.ID+"/json", nil, &inspected); err != nil {
			return err
		}
		labels := inspected.Config.Labels
		if labels["org.eebus-arena.helper.run"] != run || labels["org.eebus-arena.helper.node"] != node || labels["org.eebus-arena.address-inspection"] != "true" || !strings.HasPrefix(inspected.HostConfig.NetworkMode, "container:") {
			return errors.New("inspection helper ownership changed")
		}
		if err := c.docker.JSON(ctx, "DELETE", "/containers/"+container.ID+"?force=true", nil, nil); err != nil {
			return err
		}
	}
	return nil
}
func ipv6LinkLocal(addresses []string) (string, error) {
	var source string
	for _, address := range addresses {
		ip, _, err := net.ParseCIDR(address)
		if err != nil || ip.To4() != nil || !ip.IsLinkLocalUnicast() {
			continue
		}
		if source != "" && source != ip.String() {
			return "", errors.New("ambiguous IPv6 mDNS source on eth0")
		}
		source = ip.String()
	}
	if source == "" {
		return "", errors.New("no IPv6 link-local mDNS source found on eth0; rebuild the network helper")
	}
	return source, nil
}
