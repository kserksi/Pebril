//go:build linux

package resources

import (
	"bufio"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

// Memory counts full resident mappings, including repeated shared pages. The
// limit is a visible cgroup ceiling, not a reservation or cgroup charge total.
type Memory struct {
	Used  uint64 `json:"used_bytes"`
	Limit uint64 `json:"limit_bytes"`
	Total uint64 `json:"system_total_bytes"`
}

func MemoryLimit() (uint64, uint64, error) { return memoryLimit("/proc") }

func memoryLimit(proc string) (uint64, uint64, error) {
	data, err := os.ReadFile(filepath.Join(proc, "meminfo"))
	if err != nil {
		return 0, 0, err
	}
	total, err := memoryKB(data, "MemTotal")
	if err != nil || total == 0 {
		return 0, 0, errors.New("Invalid system MemTotal")
	}
	membership, err := os.ReadFile(filepath.Join(proc, "self/cgroup"))
	if err != nil {
		return 0, 0, err
	}
	mounts, err := os.ReadFile(filepath.Join(proc, "self/mountinfo"))
	if err != nil {
		return 0, 0, err
	}
	if strings.TrimSpace(string(membership)) == "" {
		return 0, 0, errors.New("Missing cgroup membership")
	}
	limit := total
	seen := make(map[int]bool)
	for _, line := range strings.Split(strings.TrimSpace(string(membership)), "\n") {
		if line == "" {
			continue
		}
		fields := strings.SplitN(line, ":", 3)
		if len(fields) != 3 || !strings.HasPrefix(fields[2], "/") || filepath.Clean(fields[2]) != fields[2] {
			return 0, 0, errors.New("Invalid cgroup membership")
		}
		hierarchy, parseErr := strconv.ParseUint(fields[0], 10, 64)
		if parseErr != nil || hierarchy == 0 && fields[1] != "" || hierarchy > 0 && fields[1] == "" {
			return 0, 0, errors.New("Invalid cgroup hierarchy")
		}
		version := 0
		if fields[0] == "0" && fields[1] == "" {
			version = 2
		} else if contains(fields[1], "memory") {
			version = 1
		}
		if version == 0 {
			continue
		}
		if seen[version] {
			return 0, 0, errors.New("Duplicate memory cgroup membership")
		}
		seen[version] = true
		root, mount, err := cgroupMount(mounts, version, fields[2])
		if err != nil {
			return 0, 0, err
		}
		if root != "/" {
			return 0, 0, errors.New("Memory cgroup ancestors are outside the visible mount")
		}
		current := filepath.Join(mount, strings.TrimPrefix(fields[2], "/"))
		for {
			if version == 2 {
				value, e := os.ReadFile(filepath.Join(current, "memory.max"))
				if errors.Is(e, os.ErrNotExist) {
					// The actual hierarchy root has no memory.max. A disabled controller
					// also has none; require readable controller evidence before accepting it.
					controllers, readErr := os.ReadFile(filepath.Join(current, "cgroup.controllers"))
					if readErr != nil {
						return 0, 0, readErr
					}
					if current != mount && containsWords(string(controllers), "memory") {
						return 0, 0, errors.New("Missing memory.max for an available memory controller")
					}
					if current != mount {
						enabled, readErr := os.ReadFile(filepath.Join(filepath.Dir(current), "cgroup.subtree_control"))
						if readErr != nil {
							return 0, 0, readErr
						}
						if containsWords(string(enabled), "memory") {
							return 0, 0, errors.New("Missing enabled memory controller limit")
						}
					}
				} else if e != nil {
					return 0, 0, e
				} else if err = applyLimit(&limit, string(value), true); err != nil {
					return 0, 0, err
				}
			} else {
				value, e := os.ReadFile(filepath.Join(current, "memory.limit_in_bytes"))
				if e != nil {
					return 0, 0, e
				}
				enabled := true
				if current != filepath.Join(mount, strings.TrimPrefix(fields[2], "/")) {
					hierarchy, e := os.ReadFile(filepath.Join(current, "memory.use_hierarchy"))
					if e != nil {
						return 0, 0, e
					}
					switch strings.TrimSpace(string(hierarchy)) {
					case "0":
						enabled = false
					case "1":
					default:
						return 0, 0, errors.New("Invalid memory.use_hierarchy")
					}
				}
				if enabled {
					if err = applyLimit(&limit, string(value), false); err != nil {
						return 0, 0, err
					}
				}
				if current == filepath.Join(mount, strings.TrimPrefix(fields[2], "/")) {
					stat, e := os.ReadFile(filepath.Join(current, "memory.stat"))
					if e != nil {
						return 0, 0, e
					}
					found := false
					for _, line := range strings.Split(string(stat), "\n") {
						f := strings.Fields(line)
						if len(f) > 0 && f[0] == "hierarchical_memory_limit" {
							if len(f) != 2 || found {
								return 0, 0, errors.New("Invalid hierarchical memory limit")
							}
							found = true
							if err = applyLimit(&limit, f[1], false); err != nil {
								return 0, 0, err
							}
						}
					}
					if !found {
						return 0, 0, errors.New("Missing hierarchical memory limit")
					}
				}
			}
			if current == mount {
				break
			}
			current = filepath.Dir(current)
		}
	}
	return limit, total, nil
}

func applyLimit(limit *uint64, text string, allowMax bool) error {
	text = strings.TrimSpace(text)
	if allowMax && text == "max" {
		return nil
	}
	n, err := strconv.ParseUint(text, 10, 64)
	if err != nil || n == 0 {
		return errors.New("Invalid memory controller limit")
	}
	if n < *limit {
		*limit = n
	}
	return nil
}
func contains(list, item string) bool {
	for _, v := range strings.Split(list, ",") {
		if v == item {
			return true
		}
	}
	return false
}
func containsWords(list, item string) bool {
	for _, v := range strings.Fields(list) {
		if v == item {
			return true
		}
	}
	return false
}
func mountPath(value string) (string, error) {
	var result strings.Builder
	for i := 0; i < len(value); i++ {
		if value[i] != '\\' {
			result.WriteByte(value[i])
			continue
		}
		if i+3 >= len(value) {
			return "", errors.New("Invalid mountinfo escape")
		}
		n, err := strconv.ParseUint(value[i+1:i+4], 8, 8)
		if err != nil {
			return "", err
		}
		result.WriteByte(byte(n))
		i += 3
	}
	path := result.String()
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return "", errors.New("Invalid cgroup mount path")
	}
	return path, nil
}
func cgroupMount(data []byte, version int, membership string) (string, string, error) {
	var selectedRoot, selectedMount string
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		separator := -1
		for i, v := range fields {
			if v == "-" {
				separator = i
				break
			}
		}
		if separator < 6 || len(fields) < separator+4 {
			return "", "", errors.New("Invalid mountinfo")
		}
		if !(version == 2 && fields[separator+1] == "cgroup2" || version == 1 && fields[separator+1] == "cgroup" && contains(fields[separator+3], "memory")) {
			continue
		}
		root, err := mountPath(fields[3])
		if err != nil {
			return "", "", err
		}
		mount, err := mountPath(fields[4])
		if err != nil {
			return "", "", err
		}
		if root != "/" && membership != root && !strings.HasPrefix(membership, root+"/") {
			continue
		}
		if selectedMount == "" || len(root) < len(selectedRoot) {
			selectedRoot, selectedMount = root, mount
		}
	}
	if selectedMount == "" {
		return "", "", errors.New("Memory cgroup mount is not visible")
	}
	return selectedRoot, selectedMount, nil
}

// memoryKB parses a single required procfs statistic, checking its unit and overflow.
func memoryKB(data []byte, key string) (uint64, error) {
	found := false
	var result uint64
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	for scanner.Scan() {
		f := strings.Fields(scanner.Text())
		if len(f) == 0 || f[0] != key+":" {
			continue
		}
		if found || len(f) != 3 || f[2] != "kB" {
			return 0, fmt.Errorf("Invalid %s statistic", key)
		}
		n, err := strconv.ParseUint(f[1], 10, 64)
		if err != nil || n > math.MaxUint64/1024 {
			return 0, fmt.Errorf("Invalid %s value", key)
		}
		result = n * 1024
		found = true
	}
	if err := scanner.Err(); err != nil {
		return 0, err
	}
	if !found {
		return 0, fmt.Errorf("Missing %s statistic", key)
	}
	return result, nil
}

type processIdentity struct {
	pid   int
	start uint64
}
type processInfo struct {
	identity processIdentity
	state    string
	group    int
	parent   int
}

var processState = struct {
	sync.Mutex
	known  map[processIdentity]bool
	groups map[processIdentity]bool
}{known: make(map[processIdentity]bool), groups: make(map[processIdentity]bool)}

// Run registers the controlled process group while preserving exec.Cmd exit and
// cancellation behavior. Starting and sampling share a lock to close launch races.
func Run(command *exec.Cmd) error {
	processState.Lock()
	err := command.Start()
	if err != nil {
		processState.Unlock()
		return err
	}
	info, identityErr := readProcess("/proc", command.Process.Pid)
	if identityErr == nil {
		processState.known[info.identity] = true
		if info.group == info.identity.pid {
			processState.groups[info.identity] = true
		}
	}
	processState.Unlock()
	waitErr := command.Wait()
	if identityErr == nil {
		processState.Lock()
		delete(processState.known, info.identity)
		processState.Unlock()
	}
	// A child may finish before stat is readable. Its normal Wait result remains
	// authoritative; a live unreadable worker is caught by tree/group sampling.
	if waitErr != nil {
		return waitErr
	}
	if identityErr != nil && !errors.Is(identityErr, os.ErrNotExist) {
		return identityErr
	}
	return nil
}

func reconcileGroups(proc string, groups, known map[processIdentity]bool) error {
	if len(groups) == 0 {
		return nil
	}
	for leader := range groups {
		current, err := readProcess(proc, leader.pid)
		if err == nil && current.identity != leader {
			delete(groups, leader)
		} else if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	entries, err := os.ReadDir(proc)
	if err != nil {
		return err
	}
	active := make(map[processIdentity]bool)
	for _, entry := range entries {
		pid, e := strconv.Atoi(entry.Name())
		if e != nil || pid <= 0 {
			continue
		}
		info, e := readProcess(proc, pid)
		if errors.Is(e, os.ErrNotExist) {
			continue
		}
		if e != nil {
			return e
		}
		for leader := range groups {
			if info.group == leader.pid && info.identity.start >= leader.start {
				if pid == leader.pid && info.identity != leader {
					continue
				}
				known[info.identity] = true
				active[leader] = true
			}
		}
	}
	for leader := range groups {
		if !active[leader] {
			delete(groups, leader)
		}
	}
	return nil
}

func ReadMemory() (Memory, error) {
	processState.Lock()
	defer processState.Unlock()
	limit, total, err := MemoryLimit()
	if err != nil {
		return Memory{}, err
	}
	if err := reconcileGroups("/proc", processState.groups, processState.known); err != nil {
		return Memory{}, err
	}
	used, err := readResidentTree("/proc", os.Getpid(), processState.known)
	if err != nil {
		return Memory{}, err
	}
	return Memory{Used: used, Limit: limit, Total: total}, nil
}
func readProcess(proc string, pid int) (processInfo, error) {
	data, err := os.ReadFile(filepath.Join(proc, strconv.Itoa(pid), "stat"))
	if err != nil {
		return processInfo{}, err
	}
	text := string(data)
	end := strings.LastIndex(text, ")")
	if end < 0 {
		return processInfo{}, errors.New("Invalid process stat")
	}
	fields := strings.Fields(text[end+1:])
	if len(fields) < 20 {
		return processInfo{}, errors.New("Incomplete process stat")
	}
	start, err := strconv.ParseUint(fields[19], 10, 64)
	if err != nil {
		return processInfo{}, err
	}
	group, err := strconv.Atoi(fields[2])
	if err != nil || group < 0 {
		return processInfo{}, errors.New("Invalid process group")
	}
	parent, err := strconv.Atoi(fields[1])
	if err != nil || parent < 0 {
		return processInfo{}, errors.New("Invalid process parent")
	}
	return processInfo{processIdentity{pid, start}, fields[0], group, parent}, nil
}
func sameProcess(proc string, id processIdentity) (bool, error) {
	info, err := readProcess(proc, id.pid)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return info.identity == id, nil
}
func processResident(proc string, info processInfo) (uint64, error) {
	if info.state == "Z" || info.state == "X" {
		return 0, nil
	}
	base := filepath.Join(proc, strconv.Itoa(info.identity.pid))
	data, err := os.ReadFile(filepath.Join(base, "smaps_rollup"))
	rollup := true
	if errors.Is(err, os.ErrNotExist) {
		data, err = os.ReadFile(filepath.Join(base, "smaps"))
		rollup = false
	}
	if err != nil {
		return 0, err
	}
	if rollup {
		var total uint64
		for _, key := range []string{"Rss", "Private_Hugetlb", "Shared_Hugetlb"} {
			n, e := memoryKB(data, key)
			if e != nil {
				return 0, e
			}
			if math.MaxUint64-total < n {
				return 0, errors.New("Resident memory overflow")
			}
			total += n
		}
		return total, nil
	}
	var total uint64
	counts := map[string]int{}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		key := strings.TrimSuffix(fields[0], ":")
		if key != "Rss" && key != "Private_Hugetlb" && key != "Shared_Hugetlb" {
			continue
		}
		n, e := memoryKB([]byte(line), key)
		if e != nil {
			return 0, e
		}
		if math.MaxUint64-total < n {
			return 0, errors.New("Resident memory overflow")
		}
		total += n
		counts[key]++
	}
	if counts["Rss"] == 0 || counts["Rss"] != counts["Private_Hugetlb"] || counts["Rss"] != counts["Shared_Hugetlb"] {
		return 0, errors.New("Incomplete process smaps")
	}
	return total, nil
}
func processChildren(proc string, info processInfo) ([]int, error) {
	if info.state == "Z" || info.state == "X" {
		return nil, nil
	}
	tasks := filepath.Join(proc, strconv.Itoa(info.identity.pid), "task")
	entries, err := os.ReadDir(tasks)
	if err != nil {
		return nil, err
	}
	var children []int
	for _, entry := range entries {
		if _, e := strconv.Atoi(entry.Name()); e != nil {
			continue
		}
		data, e := os.ReadFile(filepath.Join(tasks, entry.Name(), "children"))
		if errors.Is(e, os.ErrNotExist) {
			if _, statErr := os.Stat(filepath.Join(tasks, entry.Name())); errors.Is(statErr, os.ErrNotExist) {
				continue
			}
		}
		if e != nil {
			return nil, e
		}
		for _, word := range strings.Fields(string(data)) {
			pid, e := strconv.Atoi(word)
			if e != nil || pid <= 0 {
				return nil, errors.New("Invalid process child identity")
			}
			children = append(children, pid)
		}
	}
	return children, nil
}
func readResidentTree(proc string, pid int, known map[processIdentity]bool) (uint64, error) {
	root, err := readProcess(proc, pid)
	if err != nil {
		return 0, err
	}
	pending := []processIdentity{root.identity}
	for id := range known {
		pending = append(pending, id)
	}
	visited := make(map[processIdentity]bool)
	var total uint64
	for len(pending) > 0 {
		id := pending[0]
		pending = pending[1:]
		if visited[id] {
			continue
		}
		visited[id] = true
		info, e := readProcess(proc, id.pid)
		if errors.Is(e, os.ErrNotExist) {
			delete(known, id)
			continue
		}
		if e != nil {
			return 0, e
		}
		if info.identity != id {
			delete(known, id)
			continue
		}
		known[id] = true
		n, e := processResident(proc, info)
		if e != nil {
			same, checkErr := sameProcess(proc, id)
			if checkErr != nil {
				return 0, checkErr
			}
			if !same {
				delete(known, id)
				continue
			}
			after, checkErr := readProcess(proc, id.pid)
			if checkErr == nil && (after.state == "Z" || after.state == "X") {
				continue
			}
			return 0, fmt.Errorf("Read resident memory for process %d: %w", id.pid, e)
		}
		children, e := processChildren(proc, info)
		if e != nil {
			same, checkErr := sameProcess(proc, id)
			if checkErr != nil {
				return 0, checkErr
			}
			if !same {
				delete(known, id)
				continue
			}
			return 0, e
		}
		same, e := sameProcess(proc, id)
		if e != nil {
			return 0, e
		}
		if !same {
			delete(known, id)
			continue
		}
		if math.MaxUint64-total < n {
			return 0, errors.New("Process tree memory overflow")
		}
		total += n
		for _, child := range children {
			childInfo, e := readProcess(proc, child)
			if errors.Is(e, os.ErrNotExist) {
				continue
			}
			if e != nil {
				return 0, e
			}
			if childInfo.parent != id.pid && !known[childInfo.identity] {
				return 0, errors.New("Process parent changed during resource sample")
			}
			pending = append(pending, childInfo.identity)
		}
	}
	return total, nil
}
