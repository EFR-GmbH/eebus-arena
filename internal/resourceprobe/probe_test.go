package resourceprobe

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func statFixture(pid int, name string, ticks, start uint64, rss int) string {
	fields := make([]string, 22)
	for i := range fields {
		fields[i] = "0"
	}
	fields[0] = "S"
	fields[11] = fmt.Sprint(ticks)
	fields[19] = fmt.Sprint(start)
	fields[21] = fmt.Sprint(rss)
	return fmt.Sprintf("%d (%s) %s", pid, name, strings.Join(fields, " "))
}

func TestStatParsing(t *testing.T) {
	for _, test := range []struct {
		name, text string
		valid      bool
	}{
		{"spaces and parentheses", statFixture(27, "avahi (worker)", 90, 42, 8), true},
		{"truncated", "27 (worker) S 0", false},
		{"negative RSS", statFixture(27, "worker", 90, 42, -1), false},
		{"missing name", "27 S 0", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			process, err := parseStat(test.text, 4096)
			if (err == nil) != test.valid {
				t.Fatalf("valid=%v, err=%v", test.valid, err)
			}
			if test.valid && (process.ID != "27:42" || process.RSSBytes != 32768 || process.Ticks != 90) {
				t.Fatalf("wrong counters: %+v", process)
			}
		})
	}
}

func TestCollectCPUAndPIDReuse(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "27")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	write := func(ticks, start uint64) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, "stat"), []byte(statFixture(27, "avahi-daemon", ticks, start, 8)), 0600); err != nil {
			t.Fatal(err)
		}
	}
	p := &Probe{Root: root, TicksPerSecond: 100, MaxProcesses: 16, previous: map[string]Process{}}
	write(100, 42)
	sample, err := p.Collect()
	if err != nil || len(sample.Processes) != 1 || sample.Processes[0].CPUPercent != nil {
		t.Fatalf("first counter must have no invented CPU: %+v %v", sample, err)
	}
	p.at = time.Now().Add(-time.Second)
	write(190, 42)
	sample, err = p.Collect()
	if err != nil || sample.Processes[0].CPUPercent == nil || *sample.Processes[0].CPUPercent < 85 || *sample.Processes[0].CPUPercent > 95 {
		t.Fatalf("expected approximately 90 percent of one CPU: %+v %v", sample, err)
	}
	p.at = time.Now().Add(-time.Second)
	write(500, 43)
	sample, err = p.Collect()
	if err != nil || sample.Processes[0].CPUPercent != nil {
		t.Fatal("PID reuse inherited the old CPU counter")
	}
}

func TestCgroupCountersAndIncompleteProcesses(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "3"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "3", "stat"), []byte("corrupt"), 0600); err != nil {
		t.Fatal(err)
	}
	cgroup := t.TempDir()
	for name, text := range map[string]string{"memory.current": "4096", "memory.max": "8192", "cpu.max": "50000 100000", "cpu.stat": "usage_usec 900000\nthrottled_usec 300000\nnr_throttled 4\n", "memory.events": "oom 1\noom_kill 1\n", "memory.stat": "inactive_file 1024\n"} {
		if err := os.WriteFile(filepath.Join(cgroup, name), []byte(text), 0600); err != nil {
			t.Fatal(err)
		}
	}
	p := &Probe{Root: root, CgroupRoot: cgroup, TicksPerSecond: 100, MaxProcesses: 16, previous: map[string]Process{}}
	sample, err := p.Collect()
	if err != nil {
		t.Fatal(err)
	}
	if sample.Complete || sample.Errors != 1 || sample.Cgroup["workingSetBytes"] != float64(3072) || sample.Cgroup["cpuMillis"] != float64(500) || sample.Cgroup["oomKills"] != float64(1) {
		t.Fatalf("lost pressure or coverage: %+v", sample)
	}
}
