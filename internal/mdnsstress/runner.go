package mdnsstress

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net"
	"sort"
	"sync"
	"time"

	"github.com/EFR-GmbH/eebus-arena/internal/arena"
)

type Sender interface {
	WriteToUDP([]byte, *net.UDPAddr) (int, error)
	SetWriteDeadline(time.Time) error
}

// Endpoint is one configured multicast family, including its interface scope.
type Endpoint struct {
	Sender Sender
	Target *net.UDPAddr
	Family string
}
type familyCounters struct{ Attempted, Sent, SendErrors, Received uint64 }

// Runner owns one active workload. Stop joins its worker before returning.
// Capture memory is bounded to 32 pairing packets of at most 9000 bytes each.
type Runner struct {
	mu                                                             sync.Mutex
	control                                                        sync.Mutex
	cancel                                                         context.CancelFunc
	done                                                           chan struct{}
	active                                                         bool
	profile                                                        arena.StressProfile
	started, ended                                                 time.Time
	attempted, sent, sendErrors, skipped, received, captureDropped uint64
	captures                                                       map[string][]byte
	emit                                                           func(string, map[string]any)
	endpoints                                                      []Endpoint
	family                                                         string
	byFamily                                                       map[string]*familyCounters
}

func New(sender Sender, emit func(string, map[string]any)) *Runner {
	return NewEndpoints([]Endpoint{{Sender: sender, Target: &net.UDPAddr{IP: net.IPv4(224, 0, 0, 251), Port: 5353}, Family: "ipv4"}}, "ipv4", emit)
}

// NewEndpoints distributes a total requested rate evenly over selected families.
// Endpoint configuration is immutable for the runner's lifetime.
func NewEndpoints(endpoints []Endpoint, family string, emit func(string, map[string]any)) *Runner {
	counts := map[string]*familyCounters{}
	for _, endpoint := range endpoints {
		counts[endpoint.Family] = &familyCounters{}
	}
	return &Runner{endpoints: append([]Endpoint(nil), endpoints...), family: family, byFamily: counts, emit: emit, captures: map[string][]byte{}}
}
func (r *Runner) Capture(packet []byte) {
	r.CaptureFamily(packet, "ipv4")
}

// CaptureFamily records receipt on a known transport independently of send counts.
func (r *Runner) CaptureFamily(packet []byte, family string) {
	r.mu.Lock()
	r.received++
	if counters := r.byFamily[family]; counters != nil {
		counters.Received++
	}
	r.mu.Unlock()
	if len(packet) > 9000 || !IsPairingPacket(packet) {
		return
	}
	hash := sha256.Sum256(packet)
	id := hex.EncodeToString(hash[:])
	r.mu.Lock()
	if _, exists := r.captures[id]; exists {
		r.mu.Unlock()
		return
	}
	if len(r.captures) >= 32 {
		r.captureDropped++
		r.mu.Unlock()
		return
	}
	r.captures[id] = append([]byte(nil), packet...)
	r.mu.Unlock()
	r.emit("mdns.capture", map[string]any{"id": id, "family": family, "bytes": len(packet), "packet": base64.StdEncoding.EncodeToString(packet)})
}
func (r *Runner) Start(ctx context.Context, p arena.StressProfile) error {
	if err := p.Validate(); err != nil {
		return err
	}
	if p.Family == "" {
		p.Family = r.family
	}
	var endpoints []Endpoint
	for _, endpoint := range r.endpoints {
		if p.Family == "dual-stack" || endpoint.Family == p.Family {
			endpoints = append(endpoints, endpoint)
		}
	}
	if len(endpoints) == 0 || p.Family == "dual-stack" && len(endpoints) != 2 {
		return errors.New("requested mDNS family is not enabled in this arena")
	}
	r.control.Lock()
	defer r.control.Unlock()
	r.mu.Lock()
	if r.active {
		r.mu.Unlock()
		return errors.New("workload already active")
	}
	var replay []byte
	if p.Pattern == "replay" {
		replay = r.captures[p.ReplayID]
		if len(replay) == 0 {
			r.mu.Unlock()
			return errors.New("replay requires a captured pairing packet ID")
		}
	}
	runCtx, cancel := context.WithTimeout(ctx, time.Duration(p.DurationSeconds)*time.Second)
	r.cancel = cancel
	r.done = make(chan struct{})
	done := r.done
	r.active = true
	r.profile = p
	r.started = time.Now()
	r.ended = time.Time{}
	r.attempted = 0
	r.sent = 0
	r.sendErrors = 0
	r.skipped = 0
	for _, counters := range r.byFamily {
		counters.Attempted, counters.Sent, counters.SendErrors = 0, 0, 0
	}
	r.mu.Unlock()
	r.emit("mdns.workload", map[string]any{"active": true, "profile": p})
	go func() {
		defer close(done)
		defer cancel()
		defer func() {
			r.mu.Lock()
			r.active = false
			r.ended = time.Now()
			r.mu.Unlock()
			r.emit("mdns.workload", map[string]any{"active": false})
		}()
		tick := time.NewTicker(10 * time.Millisecond)
		defer tick.Stop()
		start := time.Now()
		var scheduled uint64
		for {
			select {
			case <-runCtx.Done():
				return
			case now := <-tick.C:
				due := uint64(now.Sub(start).Seconds() * float64(p.Rate))
				if due <= scheduled {
					continue
				}
				count := due - scheduled
				if count > uint64(p.Burst) {
					r.mu.Lock()
					r.skipped += count - uint64(p.Burst)
					r.mu.Unlock()
					scheduled += count - uint64(p.Burst)
					count = uint64(p.Burst)
				}
				for j := uint64(0); j < count; j++ {
					if runCtx.Err() != nil {
						return
					}
					packet := replay
					if p.Pattern != "replay" {
						packet = Packet(p, scheduled)
					}
					endpoint := endpoints[scheduled%uint64(len(endpoints))]
					scheduled++
					err := endpoint.Sender.SetWriteDeadline(time.Now().Add(100 * time.Millisecond))
					n := 0
					if err == nil {
						n, err = endpoint.Sender.WriteToUDP(packet, endpoint.Target)
					}
					r.mu.Lock()
					r.attempted++
					counters := r.byFamily[endpoint.Family]
					counters.Attempted++
					if err != nil || n != len(packet) {
						r.sendErrors++
						counters.SendErrors++
					} else {
						r.sent++
						counters.Sent++
					}
					r.mu.Unlock()
				}
			}
		}
	}()
	return nil
}
func (r *Runner) Stop() {
	r.control.Lock()
	defer r.control.Unlock()
	r.mu.Lock()
	cancel, done := r.cancel, r.done
	r.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if done != nil {
		<-done
	}
}
func (r *Runner) Snapshot() map[string]any {
	r.mu.Lock()
	defer r.mu.Unlock()
	ids := []string{}
	for id := range r.captures {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	end := r.ended
	if end.IsZero() {
		end = time.Now()
	}
	elapsed := end.Sub(r.started).Seconds()
	rate := 0.0
	if !r.started.IsZero() && elapsed > 0 {
		rate = float64(r.sent) / elapsed
	}
	families := map[string]any{}
	for family, counters := range r.byFamily {
		achieved := 0.0
		if !r.started.IsZero() && elapsed > 0 {
			achieved = float64(counters.Sent) / elapsed
		}
		families[family] = map[string]any{"attempted": counters.Attempted, "sent": counters.Sent, "sendErrors": counters.SendErrors, "received": counters.Received, "achievedRate": achieved}
	}
	return map[string]any{"active": r.active, "profile": r.profile, "attempted": r.attempted, "sent": r.sent, "sendErrors": r.sendErrors, "skipped": r.skipped, "received": r.received, "captureDropped": r.captureDropped, "captureIds": ids, "achievedRate": rate, "families": families}
}
