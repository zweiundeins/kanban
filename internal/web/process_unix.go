//go:build unix

package web

import (
	"runtime"
	"syscall"
)

// process is what the health check says about this process: CPU time
// spent and the peak resident memory.
func process() map[string]float64 {
	var ru syscall.Rusage
	if syscall.Getrusage(syscall.RUSAGE_SELF, &ru) != nil {
		return nil
	}
	peak := float64(ru.Maxrss) * 1024 // kilobytes on Linux
	if runtime.GOOS == "darwin" {
		peak = float64(ru.Maxrss)
	}
	return map[string]float64{
		"cpu_s":       float64(ru.Utime.Nano()+ru.Stime.Nano()) / 1e9,
		"peak_rss_mb": peak / 1e6,
		"gomaxprocs":  float64(runtime.GOMAXPROCS(0)),
	}
}
