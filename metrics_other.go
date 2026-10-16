//go:build !linux && !darwin

package main

import (
	"runtime"
	"time"
)

// På andre platforme måles serveren ikke; dashboardet skriver, at målingerne ikke er tilgængelige.

func numCPU() int                                            { return runtime.NumCPU() }
func readCPU() (cpuTicks, bool)                              { return cpuTicks{}, false }
func readMem() (used, total uint64, ok bool)                 { return 0, 0, false }
func diskUsage(path string) (used, total uint64, ok bool)    { return 0, 0, false }
func readLoad() (load [3]float64, up time.Duration, ok bool) { return load, 0, false }
