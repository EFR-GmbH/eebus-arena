// Package resourceprobe measures Linux processes using real elapsed time.
package resourceprobe

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Process identifies a process across PID reuse and contains interval CPU and RSS.
type Process struct {
	ID         string   `json:"id"`
	PID        int      `json:"pid"`
	Name       string   `json:"name"`
	Component  string   `json:"component"`
	StartTicks uint64   `json:"startTicks"`
	CPUPercent *float64 `json:"cpuPercent,omitempty"`
	RSSBytes   uint64   `json:"rssBytes"`
	Ticks      uint64   `json:"-"`
}

// Sample records collection coverage; CPU values average the stated real interval.
type Sample struct {
	Scope              string         `json:"scope,omitempty"`
	DeviceAt           *time.Time     `json:"deviceAt,omitempty"`
	ClockOffsetSeconds *float64       `json:"clockOffsetSeconds,omitempty"`
	At                 time.Time      `json:"at"`
	IntervalSeconds    float64        `json:"intervalSeconds"`
	CollectionMillis   float64        `json:"collectionMillis"`
	Complete           bool           `json:"complete"`
	Errors             int            `json:"errors"`
	Truncated          bool           `json:"truncated"`
	Processes          []Process      `json:"processes"`
	Cgroup             map[string]any `json:"cgroup,omitempty"`
}

// Probe maintains CPU counter baselines for the processes in one PID namespace.
type Probe struct {
	Root           string
	TicksPerSecond uint64
	MaxProcesses   int
	CgroupRoot     string
	Device         bool
	previous       map[string]Process
	at             time.Time
}

// New obtains the kernel's clock-tick frequency rather than assuming 100 Hz.
func New() (*Probe, error) {
	blob, err := os.ReadFile("/proc/self/auxv")
	if err != nil {
		return nil, err
	}
	width := strconv.IntSize / 8
	read := func(value []byte) uint64 {
		if width == 8 {
			return binary.NativeEndian.Uint64(value)
		}
		return uint64(binary.NativeEndian.Uint32(value))
	}
	for i := 0; i+2*width <= len(blob); i += 2 * width {
		if read(blob[i:i+width]) == 17 {
			freq := read(blob[i+width : i+2*width])
			if freq == 0 {
				break
			}
			return &Probe{Root: "/proc", CgroupRoot: "/sys/fs/cgroup", TicksPerSecond: freq, MaxProcesses: 256, previous: map[string]Process{}}, nil
		}
	}
	return nil, errors.New("kernel AT_CLKTCK is unavailable")
}

// Collect reads at most MaxProcesses and reports incomplete coverage explicitly.
func (p *Probe) Collect() (Sample, error) {
	started := time.Now()
	s := Sample{At: started.UTC(), Complete: true, Processes: []Process{}}
	if p.TicksPerSecond == 0 || p.MaxProcesses < 1 {
		return s, errors.New("invalid probe configuration")
	}
	entries, err := os.ReadDir(p.Root)
	if err != nil {
		return s, err
	}
	if !p.at.IsZero() {
		s.IntervalSeconds = started.Sub(p.at).Seconds()
	}
	current := map[string]Process{}
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid <= 0 {
			continue
		}
		if len(s.Processes) >= p.MaxProcesses {
			s.Complete = false
			s.Truncated = true
			break
		}
		blob, err := os.ReadFile(filepath.Join(p.Root, entry.Name(), "stat"))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			s.Errors++
			s.Complete = false
			continue
		}
		process, err := parseStat(string(blob), os.Getpagesize())
		if err != nil {
			s.Errors++
			s.Complete = false
			continue
		}
		if executable, err := os.Readlink(filepath.Join(p.Root, entry.Name(), "exe")); err == nil {
			process.Name = filepath.Base(strings.TrimSuffix(executable, " (deleted)"))
		}
		process.Component = component(process.Name)
		if old, ok := p.previous[process.ID]; ok && s.IntervalSeconds > 0 && process.Ticks >= old.Ticks {
			value := float64(process.Ticks-old.Ticks) / float64(p.TicksPerSecond) / s.IntervalSeconds * 100
			process.CPUPercent = &value
		}
		current[process.ID] = process
		s.Processes = append(s.Processes, process)
	}
	p.previous = current
	p.at = started
	s.Cgroup = p.cgroup()
	if p.Device {
		s.Scope = "device"
		s.Cgroup = p.deviceMetrics()
	}
	s.CollectionMillis = time.Since(started).Seconds() * 1000
	return s, nil
}

func (p *Probe) cgroup() map[string]any {
	values := map[string]any{}
	if p.CgroupRoot == "" {
		return values
	}
	read := func(path, key string, scale float64) {
		blob, err := os.ReadFile(filepath.Join(p.CgroupRoot, path))
		if err != nil {
			return
		}
		value, err := strconv.ParseUint(strings.TrimSpace(string(blob)), 10, 64)
		if err == nil {
			values[key] = float64(value) * scale
		}
	}
	read("memory.current", "memoryBytes", 1)
	read("memory.max", "memoryLimitBytes", 1)
	read("pids.current", "pids", 1)
	read("pids.max", "pidsLimit", 1)
	if blob, err := os.ReadFile(filepath.Join(p.Root, "self/status")); err == nil {
		for _, line := range strings.Split(string(blob), "\n") {
			if strings.HasPrefix(line, "Cpus_allowed_list:") {
				if count := cpuCount(strings.TrimSpace(strings.TrimPrefix(line, "Cpus_allowed_list:"))); count > 0 {
					values["cpuAffinityMillis"] = float64(count * 1000)
				}
			}
		}
	}
	if blob, err := os.ReadFile(filepath.Join(p.CgroupRoot, "cpu.max")); err == nil {
		parts := strings.Fields(string(blob))
		if len(parts) == 2 && parts[0] != "max" {
			quota, e1 := strconv.ParseFloat(parts[0], 64)
			period, e2 := strconv.ParseFloat(parts[1], 64)
			if e1 == nil && e2 == nil && period > 0 {
				values["cpuMillis"] = quota / period * 1000
			}
		}
	}
	for _, spec := range []struct {
		file  string
		keys  map[string]string
		scale map[string]float64
	}{
		{"cpu.stat", map[string]string{"usage_usec": "cpuUsageSeconds", "throttled_usec": "cpuThrottledSeconds", "nr_throttled": "cpuThrottledPeriods", "nr_periods": "cpuPeriods"}, map[string]float64{"usage_usec": 0.000001, "throttled_usec": 0.000001}},
		{"memory.events", map[string]string{"oom": "oomEvents", "oom_kill": "oomKills", "max": "memoryLimitEvents"}, nil},
		{"memory.stat", map[string]string{"file": "cacheBytes", "inactive_file": "inactiveFileBytes", "anon": "anonymousBytes"}, nil},
	} {
		blob, err := os.ReadFile(filepath.Join(p.CgroupRoot, spec.file))
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(blob), "\n") {
			parts := strings.Fields(line)
			if len(parts) != 2 {
				continue
			}
			key, ok := spec.keys[parts[0]]
			if !ok {
				continue
			}
			value, err := strconv.ParseFloat(parts[1], 64)
			if err != nil {
				continue
			}
			scale := spec.scale[parts[0]]
			if scale == 0 {
				scale = 1
			}
			values[key] = value * scale
		}
	}
	for _, kind := range []string{"cpu", "memory", "io"} {
		if blob, err := os.ReadFile(filepath.Join(p.CgroupRoot, kind+".pressure")); err == nil {
			for _, line := range strings.Split(string(blob), "\n") {
				fields := strings.Fields(line)
				if len(fields) == 0 {
					continue
				}
				for _, field := range fields[1:] {
					key, text, ok := strings.Cut(field, "=")
					if ok && key == "avg10" {
						if value, err := strconv.ParseFloat(text, 64); err == nil {
							values[kind+"Pressure_"+fields[0]+"_avg10"] = value
						}
					}
				}
			}
		}
	}
	if memory, ok := values["memoryBytes"].(float64); ok {
		if inactive, ok := values["inactiveFileBytes"].(float64); ok {
			working := memory - inactive
			if working < 0 {
				working = memory
			}
			values["workingSetBytes"] = working
		}
	}
	return values
}

func cpuCount(list string) int {
	seen := map[int]bool{}
	for _, part := range strings.Split(list, ",") {
		first, last, rangeValue := strings.Cut(part, "-")
		begin, err := strconv.Atoi(first)
		if err != nil || begin < 0 || begin > 4095 {
			return 0
		}
		end := begin
		if rangeValue {
			end, err = strconv.Atoi(last)
			if err != nil || end < begin || end > 4095 {
				return 0
			}
		}
		for i := begin; i <= end; i++ {
			seen[i] = true
		}
	}
	return len(seen)
}

func (p *Probe) deviceMetrics() map[string]any {
	values := map[string]any{}
	if blob, err := os.ReadFile(filepath.Join(p.Root, "stat")); err == nil {
		lines := strings.SplitN(string(blob), "\n", 2)
		fields := strings.Fields(lines[0])
		if len(fields) >= 8 && fields[0] == "cpu" {
			busy := float64(0)
			valid := true
			for _, index := range []int{1, 2, 3, 6, 7} {
				value, err := strconv.ParseFloat(fields[index], 64)
				if err != nil {
					valid = false
				}
				busy += value
			}
			if len(fields) > 8 {
				value, err := strconv.ParseFloat(fields[8], 64)
				if err != nil {
					valid = false
				}
				busy += value
			}
			if valid {
				values["cpuUsageSeconds"] = busy / float64(p.TicksPerSecond)
			}
		}
	}
	if blob, err := os.ReadFile(filepath.Join(p.Root, "meminfo")); err == nil {
		memory := map[string]float64{}
		for _, line := range strings.Split(string(blob), "\n") {
			fields := strings.Fields(line)
			if len(fields) != 3 || fields[2] != "kB" {
				continue
			}
			if value, err := strconv.ParseFloat(fields[1], 64); err == nil {
				memory[strings.TrimSuffix(fields[0], ":")] = value * 1024
			}
		}
		total, ok := memory["MemTotal"]
		available, hasAvailable := memory["MemAvailable"]
		if ok && hasAvailable && total >= available {
			values["deviceUsedBytes"] = total - available
			values["deviceTotalBytes"] = total
			values["deviceAvailableBytes"] = available
		}
	}
	return values
}

func parseStat(text string, pageSize int) (Process, error) {
	open, close := strings.IndexByte(text, '('), strings.LastIndexByte(text, ')')
	if open < 1 || close <= open {
		return Process{}, errors.New("invalid process stat")
	}
	pid, err := strconv.Atoi(strings.TrimSpace(text[:open]))
	if err != nil {
		return Process{}, err
	}
	fields := strings.Fields(text[close+1:])
	if len(fields) < 22 {
		return Process{}, errors.New("short process stat")
	}
	user, e1 := strconv.ParseUint(fields[11], 10, 64)
	system, e2 := strconv.ParseUint(fields[12], 10, 64)
	start, e3 := strconv.ParseUint(fields[19], 10, 64)
	rss, e4 := strconv.ParseInt(fields[21], 10, 64)
	if e1 != nil || e2 != nil || e3 != nil || e4 != nil || rss < 0 {
		return Process{}, errors.New("invalid process counters")
	}
	return Process{ID: fmt.Sprintf("%d:%d", pid, start), PID: pid, Name: text[open+1 : close], StartTicks: start, RSSBytes: uint64(rss) * uint64(pageSize), Ticks: user + system}, nil
}

func component(name string) string {
	if name == "arena-agent" {
		return "Lab agent"
	}
	switch name {
	case "avahi-daemon":
		return "Avahi"
	case "dbus-daemon":
		return "D-Bus"
	case "arena-supervisor":
		return "Collector"
	case "arena-node", "java":
		return "Native stack"
	case "docker-init", "tini":
		return "Container init"
	default:
		return "Other"
	}
}
