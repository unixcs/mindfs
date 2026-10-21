package agent

import (
	"testing"
	"time"
)

// A recovery probe must get the same window as the initial one: a cold CLI
// start fetches its remote model catalogue before session/new answers, and the
// old 30s recovery window cut that off, reporting a healthy agent as
// unavailable ("context deadline exceeded").
func TestRecoveryProbeSessionTimeoutMatchesInitial(t *testing.T) {
	initial, ok := probeSessionTimeoutForPhase(probePhaseInitial)
	if !ok {
		t.Fatal("initial phase has no session timeout")
	}
	recovery, ok := probeSessionTimeoutForPhase(probePhaseRecovery)
	if !ok {
		t.Fatal("recovery phase has no session timeout")
	}
	if recovery != initial {
		t.Fatalf("recovery timeout = %v, want %v", recovery, initial)
	}
	if recovery < 45*time.Second {
		t.Fatalf("recovery timeout %v is below the cold-start budget", recovery)
	}
}
