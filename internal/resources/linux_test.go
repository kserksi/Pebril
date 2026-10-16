//go:build linux

package resources

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

func resourceFixture(t *testing.T) string { t.Helper(); root := t.TempDir(); return root }
func resourceWrite(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
}
func limitFixture(t *testing.T, version int) (string, string) {
	t.Helper()
	root := resourceFixture(t)
	proc := filepath.Join(root, "proc")
	mount := filepath.Join(root, "memory space")
	resourceWrite(t, filepath.Join(proc, "meminfo"), "MemTotal: 1000 kB\n")
	membership := "0::/jobs/pebril\n"
	kind := "cgroup2 cgroup2 rw"
	if version == 1 {
		membership = "5:cpu,memory:/jobs/pebril\n"
		kind = "cgroup memory rw,memory"
	}
	resourceWrite(t, filepath.Join(proc, "self/cgroup"), membership)
	resourceWrite(t, filepath.Join(proc, "self/mountinfo"), "1 0 0:1 / "+strings.ReplaceAll(mount, " ", "\\040")+" rw - "+kind+"\n")
	if version == 2 {
		resourceWrite(t, filepath.Join(mount, "cgroup.controllers"), "memory cpu\n")
		resourceWrite(t, filepath.Join(mount, "jobs/memory.max"), "600000\n")
		resourceWrite(t, filepath.Join(mount, "jobs/pebril/memory.max"), "max\n")
	} else {
		for _, dir := range []string{"", "jobs", "jobs/pebril"} {
			resourceWrite(t, filepath.Join(mount, dir, "memory.limit_in_bytes"), "9223372036854771712\n")
			resourceWrite(t, filepath.Join(mount, dir, "memory.use_hierarchy"), "1\n")
		}
		resourceWrite(t, filepath.Join(mount, "jobs/memory.limit_in_bytes"), "500000\n")
		resourceWrite(t, filepath.Join(mount, "jobs/pebril/memory.stat"), "hierarchical_memory_limit 500000\n")
	}
	return proc, mount
}
func TestLinuxMemoryCeilings(t *testing.T) {
	for _, version := range []int{1, 2} {
		t.Run(strconv.Itoa(version), func(t *testing.T) {
			proc, _ := limitFixture(t, version)
			limit, total, err := memoryLimit(proc)
			want := uint64(600000)
			if version == 1 {
				want = 500000
			}
			if err != nil || limit != want || total != 1024000 {
				t.Fatalf("got %d/%d, %v", limit, total, err)
			}
		})
	}
	t.Run("no controller", func(t *testing.T) {
		proc, _ := limitFixture(t, 2)
		resourceWrite(t, filepath.Join(proc, "self/cgroup"), "4:cpu:/\n")
		limit, total, err := memoryLimit(proc)
		if err != nil || limit != total || total != 1024000 {
			t.Fatalf("%d/%d %v", limit, total, err)
		}
	})
	t.Run("disabled v2 controller", func(t *testing.T) {
		proc, mount := limitFixture(t, 2)
		if err := os.Remove(filepath.Join(mount, "jobs/pebril/memory.max")); err != nil {
			t.Fatal(err)
		}
		resourceWrite(t, filepath.Join(mount, "jobs/pebril/cgroup.controllers"), "")
		resourceWrite(t, filepath.Join(mount, "jobs/cgroup.subtree_control"), "")
		limit, _, err := memoryLimit(proc)
		if err != nil || limit != 600000 {
			t.Fatalf("%d %v", limit, err)
		}
	})
	for _, bad := range []string{"0", "-1", "wat", "18446744073709551616", "123 extra"} {
		t.Run("bad limit "+bad, func(t *testing.T) {
			proc, mount := limitFixture(t, 2)
			resourceWrite(t, filepath.Join(mount, "jobs/pebril/memory.max"), bad)
			if _, _, err := memoryLimit(proc); err == nil {
				t.Fatal("accepted malformed limit")
			}
		})
	}
	t.Run("missing enabled limit", func(t *testing.T) {
		proc, mount := limitFixture(t, 2)
		if err := os.Remove(filepath.Join(mount, "jobs/pebril/memory.max")); err != nil {
			t.Fatal(err)
		}
		resourceWrite(t, filepath.Join(mount, "jobs/pebril/cgroup.controllers"), "memory")
		if _, _, err := memoryLimit(proc); err == nil {
			t.Fatal("accepted incomplete ceiling")
		}
	})
	t.Run("hidden ancestor", func(t *testing.T) {
		proc, mount := limitFixture(t, 2)
		resourceWrite(t, filepath.Join(proc, "self/mountinfo"), "1 0 0:1 /jobs "+strings.ReplaceAll(mount, " ", "\\040")+" rw - cgroup2 cgroup2 rw\n")
		if _, _, err := memoryLimit(proc); err == nil {
			t.Fatal("accepted incomplete ancestor visibility")
		}
	})
	t.Run("missing v1 hierarchy", func(t *testing.T) {
		proc, mount := limitFixture(t, 1)
		resourceWrite(t, filepath.Join(mount, "jobs/pebril/memory.stat"), "cache 10\n")
		if _, _, err := memoryLimit(proc); err == nil {
			t.Fatal("accepted absent hierarchy limit")
		}
	})
}
func fixtureProcess(t *testing.T, proc string, pid int, start uint64, children string) {
	t.Helper()
	fields := make([]string, 20)
	for i := range fields {
		fields[i] = "0"
	}
	fields[0] = "S"
	if pid == 11 {
		fields[1] = "10"
	}
	fields[2] = "1"
	fields[19] = strconv.FormatUint(start, 10)
	resourceWrite(t, filepath.Join(proc, strconv.Itoa(pid), "stat"), fmt.Sprintf("%d (a ) name) %s\n", pid, strings.Join(fields, " ")))
	resourceWrite(t, filepath.Join(proc, strconv.Itoa(pid), "smaps_rollup"), "Rss: 10 kB\nPrivate_Hugetlb: 2 kB\nShared_Hugetlb: 3 kB\nAnonHugePages: 900 kB\nPss: 1 kB\n")
	resourceWrite(t, filepath.Join(proc, strconv.Itoa(pid), "task", strconv.Itoa(pid), "children"), children)
}
func TestLinuxResidentTree(t *testing.T) {
	proc := resourceFixture(t)
	fixtureProcess(t, proc, 10, 100, "11 11 99")
	fixtureProcess(t, proc, 11, 101, "")
	fixtureProcess(t, proc, 12, 102, "")
	resourceWrite(t, filepath.Join(proc, "10/task/20/children"), "11")
	known := map[processIdentity]bool{{12, 102}: true, {11, 1}: true}
	n, err := readResidentTree(proc, 10, known)
	if err != nil || n != 45*1024 {
		t.Fatalf("got %d %v", n, err)
	}
	if known[processIdentity{11, 1}] {
		t.Fatal("retained reused PID identity")
	}
	if err := os.Remove(filepath.Join(proc, "11/smaps_rollup")); err != nil {
		t.Fatal(err)
	}
	if _, err := readResidentTree(proc, 10, known); err == nil {
		t.Fatal("silently omitted live child")
	}
	resourceWrite(t, filepath.Join(proc, "11/smaps"), "one mapping\nRss: 5 kB\nPrivate_Hugetlb: 0 kB\nShared_Hugetlb: 1 kB\nanother mapping\nRss: 3 kB\nPrivate_Hugetlb: 0 kB\nShared_Hugetlb: 1 kB\n")
	n, err = readResidentTree(proc, 10, known)
	if err != nil || n != 40*1024 {
		t.Fatalf("smaps fallback %d %v", n, err)
	}
	resourceWrite(t, filepath.Join(proc, "11/smaps"), "Rss: 5 kB\n")
	if _, err := readResidentTree(proc, 10, known); err == nil {
		t.Fatal("accepted partial resident mapping")
	}
}
func TestResourceHelper(t *testing.T) {
	mode := os.Getenv("PEBRIL_RESOURCE_HELPER")
	if mode == "" {
		return
	}
	data := make([]byte, 12*1024*1024)
	for i := 0; i < len(data); i += 4096 {
		data[i] = 1
	}
	if mode == "deny" {
		_, _, err := syscall.RawSyscall6(syscall.SYS_PRCTL, 4, 0, 0, 0, 0, 0)
		if err != 0 {
			os.Exit(3)
		}
	}
	fmt.Println("ready")
	_, _ = bufio.NewReader(os.Stdin).ReadString('\n')
	if data[0] != 1 {
		os.Exit(4)
	}
	os.Exit(0)
}
func TestLinuxLiveOwnChild(t *testing.T) {
	for _, mode := range []string{"hold", "deny"} {
		t.Run(mode, func(t *testing.T) {
			command := exec.Command(os.Args[0], "-test.run=^TestResourceHelper$")
			command.Env = append(os.Environ(), "PEBRIL_RESOURCE_HELPER="+mode)
			command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
			input, err := command.StdinPipe()
			if err != nil {
				t.Fatal(err)
			}
			output, err := command.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- Run(command) }()
			t.Cleanup(func() {
				_ = input.Close()
				if err := <-done; err != nil {
					t.Errorf("helper: %v", err)
				}
			})
			if line, err := bufio.NewReader(output).ReadString('\n'); err != nil || line != "ready\n" {
				t.Fatalf("helper readiness %q %v", line, err)
			}
			sample, err := ReadMemory()
			if mode == "deny" {
				if err == nil {
					t.Fatal("unreadable live child did not fault")
				}
				return
			}
			if err != nil || sample.Used < 12*1024*1024 || sample.Limit == 0 || sample.Total < sample.Limit {
				t.Fatalf("live sample %+v %v", sample, err)
			}
		})
	}
	for _, program := range []string{"true", "false"} {
		command := exec.Command(program)
		command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		err := Run(command)
		if (err == nil) != (program == "true") {
			t.Fatalf("%s exit result %v", program, err)
		}
	}
	processState.Lock()
	err := reconcileGroups("/proc", processState.groups, processState.known)
	remaining := len(processState.groups)
	processState.Unlock()
	if err != nil || remaining != 0 {
		t.Fatalf("stale groups %d %v", remaining, err)
	}
}

func TestLinuxGroupReparentAndReuse(t *testing.T) {
	proc := resourceFixture(t)
	fixtureProcess(t, proc, 11, 101, "")
	file := filepath.Join(proc, "11/stat")
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	resourceWrite(t, file, strings.Replace(string(data), " S 10 1 ", " S 10 10 ", 1))
	groups := map[processIdentity]bool{{10, 100}: true}
	known := make(map[processIdentity]bool)
	if err := reconcileGroups(proc, groups, known); err != nil || !known[processIdentity{11, 101}] {
		t.Fatalf("orphan omitted: %v %v", known, err)
	}
	fixtureProcess(t, proc, 10, 200, "")
	if err := reconcileGroups(proc, groups, known); err != nil || len(groups) != 0 {
		t.Fatalf("reused leader retained: %v %v", groups, err)
	}
}
