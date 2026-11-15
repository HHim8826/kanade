package main

import (
	"bufio"
	"os"
	"runtime/debug"
	"strconv"
	"strings"
)

// memoryLimit sets the Go runtime's soft memory limit (review T9), unless GOMEMLIMIT already does:
// a quarter of the memory there is, the machine's or, in a container, its cgroup's, at least
// 128 MiB. Near it the garbage collector works harder instead of letting the heap double, which
// leaves room for aria2, FFmpeg and the system on a small server. It says what it set; 0 when it
// set nothing.
func memoryLimit() int64 {
	if os.Getenv("GOMEMLIMIT") != "" {
		return 0
	}
	total := memTotal()
	if total <= 0 {
		return 0
	}
	limit := max(total/4, 128<<20)
	debug.SetMemoryLimit(limit)
	return limit
}

// memTotal is the memory there is, in bytes: /proc/meminfo's MemTotal, or the cgroup's limit when
// lower; 0 when neither can be read (not Linux).
func memTotal() int64 {
	var total int64
	if f, err := os.Open("/proc/meminfo"); err == nil {
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			if f := strings.Fields(sc.Text()); len(f) >= 2 && f[0] == "MemTotal:" {
				if kb, err := strconv.ParseInt(f[1], 10, 64); err == nil {
					total = kb << 10
				}
				break
			}
		}
		f.Close()
	}
	for _, p := range []string{"/sys/fs/cgroup/memory.max", "/sys/fs/cgroup/memory/memory.limit_in_bytes"} {
		if b, err := os.ReadFile(p); err == nil {
			if n, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64); err == nil && n > 0 && (total == 0 || n < total) {
				total = n
			}
		}
	}
	return total
}
