//go:build linux

package resources

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func testMonitor(cap int) (*Monitor, *Memory, *error) {
	memory := &Memory{Used: 50, Limit: 100, Total: 100}
	var failure error
	m := &Monitor{
		configured: func() int { return cap },
		readMemory: func() (Memory, error) { return *memory, failure },
		readDisk:   func(string) (diskUsage, error) { return diskUsage{"test", 1, 99}, nil },
		state:      State{Concurrency: cap}, active: make(map[string]Warning),
	}
	return m, memory, &failure
}

func TestPressureThresholdsAndLatchedCooldown(t *testing.T) {
	m, memory, _ := testMonitor(5)
	steps := []struct {
		used uint64
		want int
		cool bool
	}{
		{79, 5, false}, {80, 4, false}, {94, 3, false},
		{95, 3, true}, {99, 3, true}, {80, 3, true},
		{40, 3, true}, {39, 4, false}, {39, 5, false}, {39, 5, false},
	}
	for _, step := range steps {
		memory.Used = step.used
		if err := m.sample(false); err != nil {
			t.Fatal(err)
		}
		state := m.State()
		if state.Concurrency != step.want || state.Cooldown != step.cool {
			t.Fatalf("usage %d: got concurrency %d, cooldown %v; want %d, %v", step.used, state.Concurrency, state.Cooldown, step.want, step.cool)
		}
	}
	memory.Used = 80
	for i := 0; i < 10; i++ {
		_ = m.sample(false)
	}
	if got := m.State().Concurrency; got != 1 {
		t.Fatalf("pressure target must remain positive: %d", got)
	}
}

func TestManualConcurrencyAndRestrictedCap(t *testing.T) {
	m, memory, _ := testMonitor(5)
	var configured atomic.Int64
	configured.Store(5)
	m.configured = func() int { return int(configured.Load()) }
	memory.Used = 95
	_ = m.sample(false)
	configured.Store(2)
	if got := m.State(); got.Concurrency != 2 || !got.Cooldown {
		t.Fatalf("manual reduction during cooldown: %+v", got)
	}
	configured.Store(9)
	if got := m.State().Concurrency; got != 2 {
		t.Fatalf("manual increase bypassed pressure target: %d", got)
	}
	memory.Used = 40
	_ = m.sample(false)
	if got := m.State(); got.Concurrency != 2 || !got.Cooldown {
		t.Fatalf("40%% must not recover: %+v", got)
	}
	memory.Used = 39
	_ = m.sample(false)
	if got := m.State(); got.Concurrency != 3 || got.Cooldown {
		t.Fatalf("recovery must add one slot: %+v", got)
	}
	m.state.Restricted = true
	_ = m.sample(false)
	if got := m.State(); got.Concurrency != 1 || !hasWarning(got, "restricted-start") {
		t.Fatalf("restricted target exceeded: %+v", got)
	}
	configured.Store(0)
	m.state.Restricted = false
	if got := m.State().Concurrency; got != 1 {
		t.Fatalf("nonpositive config yielded invalid target: %d", got)
	}
}

func TestSampleFaultRequiresFreshRecovery(t *testing.T) {
	m, memory, failure := testMonitor(4)
	_ = m.sample(false)
	*failure = errors.New("permission denied /private/config/password")
	_ = m.sample(false)
	state := m.State()
	if !state.Cooldown || state.Memory != nil || !hasWarning(state, "memory-monitor") {
		t.Fatalf("fault retained healthy state: %+v", state)
	}
	for _, warning := range state.Warnings {
		if strings.Contains(warning.Message, "/private") {
			t.Fatal("raw sampling error exposed")
		}
	}
	*failure = nil
	memory.Used = 50
	_ = m.sample(false)
	if state = m.State(); !state.Cooldown || !hasWarning(state, "memory-monitor") || state.Memory != nil {
		t.Fatalf("complete but high reading cleared latched fault: %+v", state)
	}
	memory.Used = 39
	_ = m.sample(false)
	if state = m.State(); state.Cooldown || hasWarning(state, "memory-monitor") || state.Memory == nil {
		t.Fatalf("complete low reading failed recovery: %+v", state)
	}
	memory.Limit = 0
	_ = m.sample(false)
	if state = m.State(); !state.Cooldown || state.Memory != nil {
		t.Fatalf("zero denominator accepted: %+v", state)
	}
	if err := m.sample(true); err == nil {
		t.Fatal("startup accepted invalid sample")
	}
}

func TestWarningTransitionsAndSnapshotIsolation(t *testing.T) {
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	defer slog.SetDefault(previous)
	m, memory, _ := testMonitor(4)
	memory.Used = 80
	_ = m.sample(true)
	_ = m.sample(false)
	if n := strings.Count(logs.String(), "resource_warning=memory-pressure"); n != 1 {
		t.Fatalf("unchanged warning logged %d times: %s", n, logs.String())
	}
	state := m.State()
	state.Warnings[0].Message = "changed"
	state.Memory.Used = 0
	if fresh := m.State(); fresh.Memory.Used != 80 || fresh.Warnings[0].Message == "changed" {
		t.Fatal("caller mutated monitor snapshot")
	}
	memory.Used = 50
	_ = m.sample(false)
	_ = m.sample(false)
	if n := strings.Count(logs.String(), "Resource warning cleared"); n != 1 {
		t.Fatalf("recovery logged %d times: %s", n, logs.String())
	}
}

func TestDiskThresholdAliasesAndErrors(t *testing.T) {
	m, _, _ := testMonitor(2)
	m.paths = []string{"db", "images", "temp"}
	usage := diskUsage{"same-filesystem", 80, 20}
	m.readDisk = func(string) (diskUsage, error) { return usage, nil }
	if warnings := m.diskWarnings(); len(warnings) != 0 {
		t.Fatalf("exact80%% warned: %+v", warnings)
	}
	usage.used, usage.available = 81, 19
	if warnings := m.diskWarnings(); len(warnings) != 1 {
		t.Fatalf("same filesystem aliases not deduplicated: %+v", warnings)
	}
	m.readDisk = func(path string) (diskUsage, error) {
		if path == "temp" {
			return diskUsage{}, errors.New("secret path detail")
		}
		return usage, nil
	}
	warnings := m.diskWarnings()
	if len(warnings) != 2 || warnings[0].Code != "disk-monitor-3" || strings.Contains(warnings[0].Message, "secret") {
		t.Fatalf("disk failure did not produce safe explicit diagnostic: %+v", warnings)
	}
	m.readDisk = func(path string) (diskUsage, error) {
		disk := usage
		if path == "temp" {
			disk.identity = "new-mount"
		}
		return disk, nil
	}
	if warnings := m.diskWarnings(); len(warnings) != 2 {
		t.Fatalf("changed mount not re-evaluated: %+v", warnings)
	}
	for _, invalid := range []diskUsage{{"fs", 0, 0}, {"fs", math.MaxUint64, 1}} {
		m.diskLast = nil
		m.readDisk = func(string) (diskUsage, error) { return invalid, nil }
		if warnings := m.diskWarnings(); len(warnings) != 3 || !strings.HasPrefix(warnings[0].Code, "disk-monitor-") {
			t.Fatalf("invalid filesystem counters accepted: %+v", warnings)
		}
	}
}

func TestDiskRealMissingParentAndSymlink(t *testing.T) {
	dir := t.TempDir()
	alias := filepath.Join(dir, "alias")
	if err := os.Symlink(dir, alias); err != nil {
		t.Fatal(err)
	}
	existing, err := readDiskUsage(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{alias, filepath.Join(dir, "missing", "nested")} {
		got, err := readDiskUsage(path)
		if err != nil || got.identity != existing.identity {
			t.Fatalf("alias/missing parent %s: got %+v, %v", path, got, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "missing")); !os.IsNotExist(err) {
		t.Fatal("disk collector created a directory")
	}
}

func TestCounterComparisonDoesNotOverflow(t *testing.T) {
	if !atLeast(math.MaxUint64, math.MaxUint64, 95) || atLeast(math.MaxUint64/3, math.MaxUint64, 40) {
		t.Fatal("byte comparison overflowed")
	}
}

func TestStateConcurrentReadsAndSampling(t *testing.T) {
	m, _, _ := testMonitor(4)
	var configured atomic.Int64
	configured.Store(4)
	m.configured = func() int { return int(configured.Load()) }
	var wg sync.WaitGroup
	for worker := 0; worker < 4; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				configured.Store(int64(i%5 + 1))
				_ = m.State()
			}
		}()
	}
	for i := 0; i < 100; i++ {
		_ = m.sample(false)
	}
	wg.Wait()
}

func TestNewMonitorContextAndClose(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	m, err := NewMonitor(ctx, func() int { return 2 }, false, []string{t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if m.State().Memory == nil {
		t.Fatal("constructor skipped initial sample")
	}
	cancel()
	m.Close()
	m.Close()
}

func hasWarning(state State, code string) bool {
	for _, warning := range state.Warnings {
		if warning.Code == code {
			return true
		}
	}
	return false
}

func TestDiskWarningRepeatAndFaultRecovery(t *testing.T) {
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	defer slog.SetDefault(previous)
	m, _, _ := testMonitor(2)
	m.paths = []string{"db", "images"}
	used, available := uint64(100), uint64(0)
	var failure error
	m.readDisk = func(string) (diskUsage, error) {
		return diskUsage{"fs", used, available}, failure
	}
	_ = m.sample(true)
	_ = m.sample(false)
	if n := strings.Count(logs.String(), "resource_warning=disk-pressure-fs"); n != 1 {
		t.Fatalf("unchanged full-disk warning logged %d times: %s", n, logs.String())
	}
	failure = errors.New("denied")
	_ = m.sample(false)
	if state := m.State(); !hasWarning(state, "disk-pressure-fs") || !hasWarning(state, "disk-monitor-1") {
		t.Fatalf("read failure cleared known disk pressure: %+v", state)
	}
	failure = nil
	used, available = 80, 20
	_ = m.sample(false)
	_ = m.sample(false)
	if state := m.State(); len(state.Warnings) != 0 {
		t.Fatalf("disk recovery not cleared: %+v", state)
	}
	if n := strings.Count(logs.String(), "Resource warning cleared"); n != 3 {
		t.Fatalf("expected one recovery for each active warning, got %d: %s", n, logs.String())
	}
}

func TestDanglingStorageAliasUsesTargetFilesystem(t *testing.T) {
	target, err := readDiskUsage("/dev/shm")
	if err != nil {
		t.Skip("No readable alternate filesystem")
	}
	dir := t.TempDir()
	alias := filepath.Join(dir, "missing-alias")
	if err := os.Symlink("/dev/shm/pebril-missing-storage-target", alias); err != nil {
		t.Fatal(err)
	}
	got, err := readDiskUsage(filepath.Join(alias, "nested"))
	if err != nil || got.identity != target.identity {
		t.Fatalf("dangling alias was attributed to its containing filesystem: %+v, %v", got, err)
	}
}

func TestMaximumConcurrencyRecoveryDoesNotOverflow(t *testing.T) {
	m, memory, _ := testMonitor(math.MaxInt)
	memory.Used = 39
	if err := m.sample(true); err != nil {
		t.Fatal(err)
	}
	if state := m.State(); state.Concurrency != math.MaxInt || state.Cooldown {
		t.Fatalf("startup sample overflowed maximum concurrency: %+v", state)
	}
	m.state.Concurrency = math.MaxInt - 1
	m.state.Cooldown = true
	for sample := 0; sample < 2; sample++ {
		if err := m.sample(false); err != nil {
			t.Fatal(err)
		}
		if state := m.State(); state.Concurrency != math.MaxInt || state.Cooldown {
			t.Fatalf("recovery sample %d overflowed maximum concurrency: %+v", sample, state)
		}
	}
}
