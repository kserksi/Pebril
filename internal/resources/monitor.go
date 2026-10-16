//go:build linux

package resources

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/bits"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"syscall"
	"time"
)

type Warning struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type State struct {
	Concurrency int       `json:"concurrency"`
	Cooldown    bool      `json:"cooldown"`
	Restricted  bool      `json:"restricted"`
	Warnings    []Warning `json:"warnings"`
	Memory      *Memory   `json:"memory,omitempty"`
}

// Monitor gates new work; cooldown never cancels work already in progress.
type Monitor struct {
	mu         sync.Mutex
	state      State
	configured func() int
	paths      []string
	readMemory func() (Memory, error)
	readDisk   func(string) (diskUsage, error)
	diskLast   map[int]diskUsage
	fault      bool
	active     map[string]Warning
	cancel     context.CancelFunc
	done       chan struct{}
}

func NewMonitor(ctx context.Context, configured func() int, restricted bool, paths []string) (*Monitor, error) {
	if configured == nil {
		return nil, errors.New("resource concurrency source is required")
	}
	m := &Monitor{
		configured: configured, paths: append([]string(nil), paths...),
		readMemory: ReadMemory, readDisk: readDiskUsage,
		state: State{Restricted: restricted}, active: make(map[string]Warning),
		done: make(chan struct{}),
	}
	m.state.Concurrency = m.cap()
	if err := m.sample(true); err != nil {
		return nil, err
	}
	ctx, m.cancel = context.WithCancel(ctx)
	go func() {
		defer close(m.done)
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				_ = m.sample(false)
			}
		}
	}()
	return m, nil
}

func (m *Monitor) Close() {
	m.cancel()
	<-m.done
}

func (m *Monitor) cap() int {
	if m.state.Restricted {
		return 1
	}
	return max(1, m.configured())
}

func (m *Monitor) State() State {
	m.mu.Lock()
	defer m.mu.Unlock()
	// Persist manual reductions, so a later increase still recovers one slot per sample.
	m.state.Concurrency = min(m.state.Concurrency, m.cap())
	state := m.state
	state.Warnings = append([]Warning{}, m.state.Warnings...)
	if state.Memory != nil {
		memory := *state.Memory
		state.Memory = &memory
	}
	return state
}

func (m *Monitor) sample(startup bool) error {
	memory, err := m.readMemory()
	if err == nil && (memory.Limit == 0 || memory.Total == 0 || memory.Limit > memory.Total) {
		err = errors.New("invalid memory ceiling")
	}
	if startup && err != nil {
		return errors.New("initial resource memory sample is unavailable or incomplete")
	}
	warnings := m.diskWarnings()
	m.mu.Lock()
	defer m.mu.Unlock()
	cap := m.cap()
	m.state.Concurrency = min(m.state.Concurrency, cap)
	if err != nil {
		m.fault = true
		m.state.Memory = nil
		m.state.Cooldown = true
	} else {
		if !atLeast(memory.Used, memory.Limit, 40) {
			m.fault = false
			m.state.Cooldown = false
			if m.state.Concurrency < cap {
				m.state.Concurrency++
			}
		} else if m.state.Cooldown || atLeast(memory.Used, memory.Limit, 95) {
			m.state.Cooldown = true
		} else if atLeast(memory.Used, memory.Limit, 80) {
			m.state.Concurrency = max(1, m.state.Concurrency-1)
		}
		if !m.fault {
			m.state.Memory = &memory
		}
	}
	if m.state.Restricted {
		warnings = append(warnings, Warning{"restricted-start", "Restricted startup is active: concurrency is one and AVIF conversion is disabled."})
	}
	if m.fault {
		warnings = append(warnings, Warning{"memory-monitor", "Memory sampling is incomplete. New uploads and conversions are paused until a complete sample is below 40%."})
	} else if m.state.Cooldown {
		warnings = append(warnings, Warning{"memory-cooldown", "Memory cooldown is active. New uploads and conversions are paused until usage is below 40%."})
	} else if atLeast(memory.Used, memory.Limit, 80) {
		warnings = append(warnings, Warning{"memory-pressure", "Memory usage is at least 80%; conversion concurrency is being reduced."})
	}
	m.updateWarnings(warnings)
	return nil
}

// Compare without overflowing byte counters or rounding a threshold boundary.
func atLeast(n, d uint64, percent uint64) bool {
	nh, nl := bits.Mul64(n, 100)
	dh, dl := bits.Mul64(d, percent)
	return nh > dh || nh == dh && nl >= dl
}

func (m *Monitor) updateWarnings(warnings []Warning) {
	next := make(map[string]Warning, len(warnings))
	for _, warning := range warnings {
		next[warning.Code] = warning
		if _, exists := m.active[warning.Code]; !exists {
			slog.Warn(warning.Message, "resource_warning", warning.Code)
		}
	}
	for code := range m.active {
		if _, exists := next[code]; !exists {
			slog.Info("Resource warning cleared", "resource_warning", code)
		}
	}
	sort.Slice(warnings, func(i, j int) bool { return warnings[i].Code < warnings[j].Code })
	m.active = next
	m.state.Warnings = warnings
}

type diskUsage struct {
	identity  string
	used      uint64
	available uint64
}

func readDiskUsage(path string) (diskUsage, error) {
	if path == "" {
		return diskUsage{}, errors.New("storage path is empty")
	}
	path, err := filepath.Abs(path)
	if err != nil {
		return diskUsage{}, err
	}
	var info os.FileInfo
	symlinks := 0
	for {
		info, err = os.Stat(path)
		if err == nil || !os.IsNotExist(err) {
			break
		}
		// A dangling directory alias belongs to its target's nearest existing parent.
		if entry, e := os.Lstat(path); e == nil && entry.Mode()&os.ModeSymlink != 0 {
			symlinks++
			if symlinks > 40 {
				return diskUsage{}, errors.New("storage symlink loop")
			}
			target, e := os.Readlink(path)
			if e != nil {
				return diskUsage{}, e
			}
			if !filepath.IsAbs(target) {
				target = filepath.Join(filepath.Dir(path), target)
			}
			path = target
			continue
		}
		parent := filepath.Dir(path)
		if parent == path {
			break
		}
		path = parent
	}
	if err != nil {
		return diskUsage{}, err
	}
	var fs syscall.Statfs_t
	if err := syscall.Statfs(path, &fs); err != nil {
		return diskUsage{}, err
	}
	if fs.Blocks == 0 || fs.Bfree > fs.Blocks || fs.Bavail > fs.Bfree {
		return diskUsage{}, errors.New("invalid filesystem block counters")
	}
	used := fs.Blocks - fs.Bfree
	if used == 0 && fs.Bavail == 0 || used+fs.Bavail < used {
		return diskUsage{}, errors.New("invalid filesystem usage denominator")
	}
	stat := info.Sys().(*syscall.Stat_t)
	return diskUsage{
		identity: fmt.Sprintf("%x-%x-%x-%x", stat.Dev, fs.Fsid.X__val[0], fs.Fsid.X__val[1], fs.Type),
		used:     used, available: fs.Bavail,
	}, nil
}

func (m *Monitor) diskWarnings() []Warning {
	warnings := []Warning{}
	fresh := make(map[string]diskUsage)
	unreadable := make(map[int]bool)
	if m.diskLast == nil {
		m.diskLast = make(map[int]diskUsage)
	}
	for i, path := range m.paths {
		disk, err := m.readDisk(path)
		if err != nil || disk.identity == "" || disk.used+disk.available == 0 || disk.used+disk.available < disk.used {
			unreadable[i] = true
			warnings = append(warnings, Warning{
				fmt.Sprintf("disk-monitor-%d", i+1),
				fmt.Sprintf("Storage usage could not be read for configured storage path %d.", i+1),
			})
			continue
		}
		m.diskLast[i] = disk
		fresh[disk.identity] = disk
	}
	for i := range unreadable {
		// A read failure cannot clear an earlier pressure warning; a fresh alias can.
		if previous, exists := m.diskLast[i]; exists {
			if _, readable := fresh[previous.identity]; !readable {
				fresh[previous.identity] = previous
			}
		}
	}
	for _, disk := range fresh {
		denominator := disk.used + disk.available
		// Strictly greater than 80%, including counters beyond float precision.
		nh, nl := bits.Mul64(disk.used, 100)
		dh, dl := bits.Mul64(denominator, 80)
		if nh > dh || nh == dh && nl > dl {
			warnings = append(warnings, Warning{"disk-pressure-" + disk.identity, "A storage filesystem was observed above 80% usage; a complete reading at or below 80% clears this warning."})
		}
	}
	sort.Slice(warnings, func(i, j int) bool { return warnings[i].Code < warnings[j].Code })
	return warnings
}
