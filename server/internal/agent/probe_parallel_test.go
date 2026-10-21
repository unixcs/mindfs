package agent

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestProbeParallelLimitDefaultAndEnv(t *testing.T) {
	t.Setenv("MINDFS_PROBE_PARALLEL", "")
	if got := probeParallelLimit(); got != defaultProbeParallel {
		t.Fatalf("default limit = %d, want %d", got, defaultProbeParallel)
	}
	t.Setenv("MINDFS_PROBE_PARALLEL", "3")
	if got := probeParallelLimit(); got != 3 {
		t.Fatalf("env limit = %d, want 3", got)
	}
	for _, bad := range []string{"0", "-1", "abc"} {
		t.Setenv("MINDFS_PROBE_PARALLEL", bad)
		if got := probeParallelLimit(); got != defaultProbeParallel {
			t.Fatalf("limit for %q = %d, want default %d", bad, got, defaultProbeParallel)
		}
	}
}

// runDefinitionsConcurrently must never let more than the cap of agents cold
// start at once: on a 4-core box five simultaneous node CLI launches blow every
// probe window, which reads as "all agents unavailable".
func TestRunDefinitionsConcurrentlyCapsParallelism(t *testing.T) {
	defs := []Definition{{Name: "a"}, {Name: "b"}, {Name: "c"}, {Name: "d"}, {Name: "e"}}
	var running, peak int32
	var wg sync.WaitGroup
	wg.Add(len(defs))

	p := NewProber(nil, nil, 0)
	if cap(p.probeSem) != defaultProbeParallel {
		t.Fatalf("probeSem cap = %d, want %d", cap(p.probeSem), defaultProbeParallel)
	}
	p.runDefinitionsConcurrently(defs,
		func(_ int, def Definition) {
			defer wg.Done()
			cur := atomic.AddInt32(&running, 1)
			for {
				old := atomic.LoadInt32(&peak)
				if cur <= old || atomic.CompareAndSwapInt32(&peak, old, cur) {
					break
				}
			}
			time.Sleep(30 * time.Millisecond)
			atomic.AddInt32(&running, -1)
		})

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("definitions did not all run")
	}
	if got := atomic.LoadInt32(&peak); got > defaultProbeParallel {
		t.Fatalf("peak concurrent probes = %d, want <= %d", got, defaultProbeParallel)
	}
	if got := atomic.LoadInt32(&running); got != 0 {
		t.Fatalf("probes still running after finish: %d", got)
	}
}
