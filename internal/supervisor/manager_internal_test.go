package supervisor

import (
	"testing"
	"time"
)

// TestRestartBudgetWindowed exercises the windowed crash-loop cap (decision 4a)
// directly: attempts inside the window count toward the budget, and a healthy
// run past the window prunes old attempts so the budget refreshes.
func TestRestartBudgetWindowed(t *testing.T) {
	b := restartBudget{max: 2, window: 60 * time.Second}
	base := time.Unix(1_700_000_000, 0).UTC()

	if !b.allow(base) {
		t.Fatal("1st restart within budget should be allowed")
	}
	if !b.allow(base.Add(time.Second)) {
		t.Fatal("2nd restart within budget should be allowed")
	}
	if b.allow(base.Add(2 * time.Second)) {
		t.Fatal("3rd restart within window should be denied (budget exhausted)")
	}

	// A crash long after the window: the two earlier attempts are pruned, so a
	// fresh budget is available again.
	if !b.allow(base.Add(2 * time.Minute)) {
		t.Fatal("restart past the window should be allowed after pruning")
	}
}

// TestRestartBudgetZeroMax denies every restart, modelling restart=always with
// max_restarts=0 (warranted, but never within budget).
func TestRestartBudgetZeroMax(t *testing.T) {
	b := restartBudget{max: 0, window: time.Minute}
	if b.allow(time.Unix(0, 0)) {
		t.Fatal("max=0 must deny all restarts")
	}
}
