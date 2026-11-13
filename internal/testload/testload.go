// Package testload keeps wall-clock assertions meaningful on a shared machine.
//
// Several tests in this repo assert an upper time bound (a 150 ms exec timeout
// comes back within 1.5 s; a 100x20 frame renders in under 5 ms). Those bounds
// describe the code, not the machine — and this repo is built on a 10-core Mac
// mini that also runs six parallel `go test` worktrees, where every one of them
// starts failing for reasons unrelated to the change under test.
//
// Scale stretches a bound by the 1-minute load average per CPU (1x when idle,
// up to 5x when the box is oversubscribed), so a genuine regression still fails
// and scheduler starvation does not. Nothing here is linked into the product
// binary: only tests import it.
package testload

import (
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// maxScale caps how far a bound may stretch.
const maxScale = 5

var (
	once  sync.Once
	scale float64 = 1
)

// Scale returns d multiplied by the current load factor, clamped to [1, 5].
func Scale(d time.Duration) time.Duration {
	once.Do(func() {
		load, ok := perCPU()
		if !ok || load <= 0.75 {
			return
		}
		// 0.75–2.0 load per CPU maps linearly to 1x–5x.
		f := 1 + (load-0.75)*3.2
		if f > maxScale {
			f = maxScale
		}
		scale = f
	})
	return time.Duration(float64(d) * scale)
}

// Factor reports the multiplier Scale applies (1 when the machine is idle).
func Factor() float64 {
	Scale(time.Nanosecond) // ensure the factor is computed
	return scale
}

// perCPU returns the 1-minute load average divided by the number of CPUs.
func perCPU() (float64, bool) {
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
