package moshiclient

import (
	"math"
	"testing"
)

func TestEMAHalfLife(t *testing.T) {
	// attack_time is defined as the time to reach 50% of a step change
	// when rising. Starting at 0, stepping toward 1 for exactly
	// attackTime seconds should land at ~0.5.
	e := NewEMA(1.0, 1.0, 0.0)
	got := e.Update(1.0, 1.0)
	if math.Abs(got-0.5) > 1e-9 {
		t.Errorf("after one half-life, value = %v, want ~0.5", got)
	}
}

func TestEMAAttackVsReleaseAsymmetry(t *testing.T) {
	// A fast attack (short half-life) should converge toward a rising
	// target much faster than a slow release converges toward a falling
	// one, over the same dt.
	fastAttack := NewEMA(0.01, 10.0, 0.0)
	fastAttack.Update(0.08, 1.0)
	if fastAttack.Value < 0.9 {
		t.Errorf("fast attack after 0.08s (8 half-lives) = %v, want close to 1", fastAttack.Value)
	}

	slowRelease := NewEMA(0.01, 10.0, 1.0)
	slowRelease.Update(0.08, 0.0)
	if slowRelease.Value < 0.9 {
		t.Errorf("slow release after 0.08s (far less than one half-life) = %v, want still close to 1", slowRelease.Value)
	}
}

func TestEMAConvergesToTarget(t *testing.T) {
	e := NewEMA(0.01, 0.01, 1.0)
	var v float64
	for i := 0; i < 100; i++ {
		v = e.Update(FrameTimeSec, 0.0)
	}
	if v > 1e-6 {
		t.Errorf("after 100 updates toward 0, value = %v, want ~0", v)
	}
}

func TestEMAUpdatePanicsOnNonPositiveDt(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic on dt <= 0")
		}
	}()
	NewEMA(1, 1, 0).Update(0, 1)
}
