package moshiclient

import "math"

// EMA is a two-pole exponential moving average that smooths differently
// for rising ("attack") vs falling ("release") values — ported directly
// from Unmute's unmute/stt/exponential_moving_average.py, which Kyutai
// uses to smooth the raw per-frame pause-prediction score from moshi-server's
// STT before thresholding it. AttackTime/ReleaseTime are the half-life in
// seconds: how long it takes the estimate to reach 50% of a step change in
// that direction.
type EMA struct {
	AttackTime  float64
	ReleaseTime float64
	Value       float64
}

// NewEMA constructs an EMA with the given attack/release half-lives and
// initial value.
func NewEMA(attackTime, releaseTime, initialValue float64) *EMA {
	return &EMA{AttackTime: attackTime, ReleaseTime: releaseTime, Value: initialValue}
}

// Update advances the estimate by dt seconds toward newValue and returns
// the new value. dt must be positive; newValue must be non-negative —
// mirrors the Python implementation's assertions (panics on violation,
// since both are programmer errors, not runtime conditions callers should
// need to handle).
func (e *EMA) Update(dt, newValue float64) float64 {
	if dt <= 0 {
		panic("moshiclient: EMA.Update: dt must be positive")
	}
	if newValue < 0 {
		panic("moshiclient: EMA.Update: newValue must be non-negative")
	}
	var alpha float64
	if newValue > e.Value {
		alpha = 1 - math.Exp(-dt/e.AttackTime*math.Ln2)
	} else {
		alpha = 1 - math.Exp(-dt/e.ReleaseTime*math.Ln2)
	}
	e.Value = (1-alpha)*e.Value + alpha*newValue
	return e.Value
}
