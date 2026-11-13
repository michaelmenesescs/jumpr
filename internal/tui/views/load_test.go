package views

import (
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// busyMachine reports the machine's 1-minute load average per CPU and whether
// it could be read, and skips the caller when the box is already oversubscribed.
//
// The view tests that measure wall-clock render budgets are meaningful only on
// a machine that is not running six `go test` worktrees beside them: the same
// 100x20 frame that takes 0.3 ms alone takes 6 ms at load 40, which says
// nothing about the code. Timing assertions still run — only the timing check
// is skipped, and the skip says why.
func skipIfBusy(t *testing.T, what string) {
	t.Helper()
	if load, ok := loadPerCPU(); ok && load > 0.75 {
		t.Skipf("load average %.2f per CPU: skipping the %s budget (the behaviour above still ran)", load, what)
	}
}

// loadPerCPU returns the 1-minute load average divided by the number of CPUs.
func loadPerCPU() (float64, bool) {
	var fields []string
	if raw, err := os.ReadFile("/proc/loadavg"); err == nil {
		fields = strings.Fields(string(raw))
	} else if out, err := exec.Command("sysctl", "-n", "vm.loadavg").Output(); err == nil {
		fields = strings.FieldsFunc(string(out), func(r rune) bool {
			return r == ' ' || r == '{' || r == '}' || r == '\n' || r == '\t'
		})
	}
	if len(fields) == 0 {
		return 0, false
	}
	load, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return 0, false
	}
	return load / float64(runtime.NumCPU()), true
}
