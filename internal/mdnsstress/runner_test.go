package mdnsstress

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/EFR-GmbH/eebus-arena/internal/arena"
)

func profile() arena.StressProfile {
	return arena.StressProfile{Version: 1, Seed: 42, Services: 100, Rate: 100, Burst: 100, Churn: 20, TTL: 120, DurationSeconds: 1, Pattern: "advertise", Service: "_shippairing._tcp"}
}
func TestPacketsAreDeterministicAndWellFramed(t *testing.T) {
	for _, pattern := range []string{"advertise", "query", "churn", "goodbye", "duplicate", "invalid", "stale"} {
		t.Run(pattern, func(t *testing.T) {
			p := profile()
			p.Pattern = pattern
			b := Packet(p, 33)
			if !bytes.Equal(b, Packet(p, 33)) {
				t.Fatal("nondeterministic packet")
			}
			pos := 12
			count := int(binary.BigEndian.Uint16(b[6:8]))
			if pattern == "query" {
				count = 1
			}
			for i := 0; i < count; i++ {
				_, next, ok := readName(b, pos)
				if !ok {
					t.Fatal("invalid owner")
				}
				pos = next
				if pattern == "query" {
					pos += 4
					continue
				}
				if pos+10 > len(b) {
					t.Fatal("truncated RR")
				}
				size := int(binary.BigEndian.Uint16(b[pos+8 : pos+10]))
				if pattern == "goodbye" && binary.BigEndian.Uint32(b[pos+4:pos+8]) != 0 {
					t.Fatal("goodbye TTL")
				}
				pos += 10 + size
			}
			if pos != len(b) {
				t.Fatal("record lengths do not cover packet")
			}
		})
	}
}
func TestCaptureBoundsAndCompressionLoop(t *testing.T) {
	if IsPairingPacket([]byte{1, 2, 3}) {
		t.Fatal("short packet")
	}
	b := make([]byte, 18)
	binary.BigEndian.PutUint16(b[4:6], 1)
	b[12] = 0xc0
	b[13] = 12
	if IsPairingPacket(b) {
		t.Fatal("compression loop accepted")
	}
	r := New(&fakeSender{}, func(string, map[string]any) {})
	p := profile()
	for i := 0; i < 100; i++ {
		r.Capture(Packet(p, uint64(i)))
	}
	s := r.Snapshot()
	if len(s["captureIds"].([]string)) != 32 || s["captureDropped"].(uint64) != 68 {
		t.Fatal("unbounded capture", s)
	}
}

type fakeSender struct {
	mu     sync.Mutex
	writes int
	fail   bool
}

func (s *fakeSender) SetWriteDeadline(time.Time) error { return nil }
func (s *fakeSender) WriteToUDP(b []byte, _ *net.UDPAddr) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.writes++
	if s.fail {
		return 0, errors.New("socket failure")
	}
	return len(b), nil
}
func TestStopJoinsWorkerAndErrorsAreCounted(t *testing.T) {
	s := &fakeSender{fail: true}
	r := New(s, func(string, map[string]any) {})
	if err := r.Start(context.Background(), profile()); err != nil {
		t.Fatal(err)
	}
	if err := r.Start(context.Background(), profile()); err == nil {
		t.Fatal("concurrent run accepted")
	}
	time.Sleep(50 * time.Millisecond)
	r.Stop()
	before := r.Snapshot()
	time.Sleep(20 * time.Millisecond)
	after := r.Snapshot()
	if before["attempted"] != after["attempted"] || after["active"] != false || after["sendErrors"].(uint64) == 0 {
		t.Fatal("worker not joined or failure hidden", after)
	}
}
func TestReplayRequiresObservedPacket(t *testing.T) {
	r := New(&fakeSender{}, func(string, map[string]any) {})
	p := profile()
	p.Pattern = "replay"
	p.ReplayID = "missing"
	if r.Start(context.Background(), p) == nil {
		t.Fatal("unobserved replay accepted")
	}
	r.Capture(Packet(profile(), 0))
	p.ReplayID = r.Snapshot()["captureIds"].([]string)[0]
	if err := r.Start(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	r.Stop()
}

func TestAddressFamiliesInAnnouncements(t *testing.T) {
	for _, family := range []string{"ipv4", "ipv6", "dual-stack"} {
		p := profile()
		p.Family = family
		b := Packet(p, 1)
		pos := 12
		types := map[uint16]int{}
		for i := 0; i < int(binary.BigEndian.Uint16(b[6:8])); i++ {
			_, next, ok := readName(b, pos)
			if !ok {
				t.Fatal("bad owner")
			}
			pos = next
			kind := binary.BigEndian.Uint16(b[pos : pos+2])
			types[kind]++
			size := int(binary.BigEndian.Uint16(b[pos+8 : pos+10]))
			if kind == 1 && size != 4 || kind == 28 && size != 16 {
				t.Fatal("wrong address RDATA")
			}
			pos += 10 + size
		}
		if (family != "ipv6" && types[1] != 1) || (family == "ipv6" && types[1] != 0) || (family != "ipv4" && types[28] != 1) || (family == "ipv4" && types[28] != 0) || pos != len(b) {
			t.Fatal(family, types)
		}
	}
}
func TestDualStackRateIsSharedAndErrorsIndependent(t *testing.T) {
	v4, v6 := &fakeSender{}, &fakeSender{fail: true}
	r := NewEndpoints([]Endpoint{{Sender: v4, Family: "ipv4"}, {Sender: v6, Family: "ipv6"}}, "dual-stack", func(string, map[string]any) {})
	p := profile()
	p.Rate = 40
	p.Burst = 1
	if err := r.Start(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1100 * time.Millisecond)
	r.Stop()
	s := r.Snapshot()
	families := s["families"].(map[string]any)
	four := families["ipv4"].(map[string]any)
	six := families["ipv6"].(map[string]any)
	attempts := four["attempted"].(uint64) + six["attempted"].(uint64)
	if attempts < 30 || attempts > 50 || four["sent"].(uint64) == 0 || six["sendErrors"].(uint64) == 0 || six["sent"].(uint64) != 0 {
		t.Fatal(s)
	}
	only := New(v4, func(string, map[string]any) {})
	p.Family = "dual-stack"
	if only.Start(context.Background(), p) == nil {
		only.Stop()
		t.Fatal("accepted missing IPv6 transport")
	}
}
