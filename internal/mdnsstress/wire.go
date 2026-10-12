// Package mdnsstress generates bounded DNS-SD workloads independently of the tested stacks.
package mdnsstress

import (
	"encoding/binary"
	"fmt"
	"math/rand/v2"
	"net"
	"strings"

	"github.com/EFR-GmbH/eebus-arena/internal/arena"
)

func name(s string) []byte {
	out := []byte{}
	for _, part := range strings.Split(strings.TrimSuffix(s, "."), ".") {
		out = append(out, byte(len(part)))
		out = append(out, part...)
	}
	return append(out, 0)
}
func u16(b []byte, v uint16) []byte { return binary.BigEndian.AppendUint16(b, v) }
func rr(owner string, typ uint16, ttl uint32, data []byte) []byte {
	b := name(owner)
	b = u16(b, typ)
	class := uint16(0x8001)
	if typ == 12 {
		class = 1
	}
	b = u16(b, class)
	b = binary.BigEndian.AppendUint32(b, ttl)
	b = u16(b, uint16(len(data)))
	return append(b, data...)
}
func txt(fields []string) []byte {
	b := []byte{}
	for _, f := range fields {
		b = append(b, byte(len(f)))
		b = append(b, f...)
	}
	return b
}

// Packet produces one DNS packet; selection is deterministic for a profile/seed/index.
func Packet(p arena.StressProfile, index uint64) []byte {
	random := rand.New(rand.NewPCG(uint64(p.Seed), index))
	service := p.Service + ".local."
	slot := int(index % uint64(p.Services))
	generation := uint64(0)
	churning := p.Pattern == "churn" && random.IntN(100) < p.Churn
	if churning {
		generation = 1 + index/uint64(p.Services)
	}
	if p.Pattern == "duplicate" {
		slot /= 2
	}
	instance := fmt.Sprintf("arena-noise-%d-%d.%s", slot, generation, service)
	host := fmt.Sprintf("arena-noise-%d.local.", slot)
	header := make([]byte, 12)
	if p.Pattern == "query" {
		binary.BigEndian.PutUint16(header[4:6], 1)
		b := append(header, name(service)...)
		b = u16(b, 12)
		return u16(b, 1)
	}
	binary.BigEndian.PutUint16(header[2:4], 0x8400)
	binary.BigEndian.PutUint16(header[6:8], 4)
	if p.Family == "dual-stack" {
		binary.BigEndian.PutUint16(header[6:8], 5)
	}
	ttl := uint32(p.TTL)
	if p.Pattern == "goodbye" || (churning && index%2 == 0) {
		ttl = 0
	}
	ski := fmt.Sprintf("%040x", slot+1)
	fields := []string{"txtvers=1", "id=arena-noise-" + fmt.Sprint(slot), "ski=" + ski, "register=false", "path=/ship/"}
	if p.Service == "_shippairing._tcp" {
		forID := p.ForID
		if forID == "" {
			forID = "arena-noise-target"
		}
		forPar := p.ForPar
		if forPar == "" {
			forPar = strings.Repeat("0", 64)
		}
		fields = []string{"txtvers=1", "parType=fpSha256", "forId=" + forID, "forPar=" + forPar, "trustId=arena-noise-" + fmt.Sprint(slot), "trustPar=" + fmt.Sprintf("%064X", slot+1), "trustCurve=secp256r1", "type=addCu", "trustNonce=" + fmt.Sprintf("%032X", index+1), "alg=hmacSha256", "digest=" + strings.Repeat("0", 64)}
	}
	if p.Pattern == "invalid" {
		switch index % 4 {
		case 0:
			fields = fields[1:]
		case 1:
			fields[0] = "txtvers=999"
		case 2:
			fields = append(fields, "txtvers=2")
		case 3:
			fields[1] = "id="
		}
	}
	if p.Service == "_http._tcp" {
		fields = []string{"path=/"}
	}
	b := append(header, rr(service, 12, ttl, name(instance))...)
	port := uint16(4712)
	if p.Pattern == "stale" {
		port = 9
	}
	if p.Pattern == "duplicate" {
		port += uint16(index % 2)
	}
	srv := u16(u16(u16(nil, 0), 0), port)
	srv = append(srv, name(host)...)
	b = append(b, rr(instance, 33, ttl, srv)...)
	b = append(b, rr(instance, 16, ttl, txt(fields))...)
	if p.Family != "ipv6" {
		b = append(b, rr(host, 1, ttl, []byte{192, 0, 2, byte(1 + slot%253)})...)
	}
	if p.Family == "ipv6" || p.Family == "dual-stack" {
		ip := net.ParseIP(fmt.Sprintf("2001:db8::%x", slot+1)).To16()
		b = append(b, rr(host, 28, ttl, ip)...)
	}
	return b
}

// IsPairingPacket recognizes DNS names, including compression, without accepting raw injection input.
func IsPairingPacket(b []byte) bool {
	if len(b) < 12 || b[2]&0x80 == 0 {
		return false
	}
	pos := 12
	counts := []int{int(binary.BigEndian.Uint16(b[4:6])), int(binary.BigEndian.Uint16(b[6:8])) + int(binary.BigEndian.Uint16(b[8:10])) + int(binary.BigEndian.Uint16(b[10:12]))}
	if counts[0]+counts[1] > 1024 {
		return false
	}
	for section, count := range counts {
		for i := 0; i < count; i++ {
			n, next, ok := readName(b, pos)
			if !ok {
				return false
			}
			pos = next
			if section == 0 {
				pos += 4
			} else {
				if pos+10 > len(b) {
					return false
				}
				length := int(binary.BigEndian.Uint16(b[pos+8 : pos+10]))
				if pos+10+length > len(b) {
					return false
				}
				if binary.BigEndian.Uint16(b[pos:pos+2]) == 16 && strings.HasSuffix(strings.ToLower(n), "._shippairing._tcp.local") {
					return true
				}
				pos += 10 + length
			}
			if pos > len(b) {
				return false
			}
		}
	}
	return false
}
func readName(b []byte, pos int) (string, int, bool) {
	labels := []string{}
	next := -1
	for hops := 0; hops < 128; hops++ {
		if pos >= len(b) {
			return "", 0, false
		}
		size := int(b[pos])
		pos++
		if size == 0 {
			if next < 0 {
				next = pos
			}
			return strings.Join(labels, "."), next, true
		}
		if size&0xc0 == 0xc0 {
			if pos >= len(b) {
				return "", 0, false
			}
			if next < 0 {
				next = pos + 1
			}
			pos = (size&0x3f)<<8 | int(b[pos])
			continue
		}
		if size > 63 || pos+size > len(b) {
			return "", 0, false
		}
		labels = append(labels, string(b[pos:pos+size]))
		pos += size
	}
	return "", 0, false
}
