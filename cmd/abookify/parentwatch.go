package main

import (
	"os"
	"strconv"
	"time"
)

// watchParent calls lost once getppid stops answering parent. A child whose
// parent died is re-parented (to init/launchd, or a subreaper), so the value
// changes; polling it is the portable stand-in for Linux's PR_SET_PDEATHSIG.
// Blocks until lost has been called; meant for its own goroutine.
func watchParent(parent int, getppid func() int, every time.Duration, lost func()) {
	for {
		if getppid() != parent {
			lost()
			return
		}
		time.Sleep(every)
	}
}

func envIntOrDefault(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}
