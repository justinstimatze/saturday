package main

import (
	"math"
	"testing"
	"time"
)

func TestSmoothdampStepConvergesMonotonically(t *testing.T) {
	pos := 0.0
	prev := pos
	for i := 0; i < 6; i++ {
		pos = smoothdampStep(pos, 100, 50, 30)
		if pos < prev || pos > 100 {
			t.Fatalf("step %d: pos=%v not monotonically approaching target from below (prev=%v)", i, pos, prev)
		}
		prev = pos
	}
	if pos <= 90 {
		t.Fatalf("after 6 steps expected substantial convergence, got pos=%v", pos)
	}
	if pos == 100 {
		t.Fatalf("smoothdamp is asymptotic — should not exactly reach target in finite steps, got pos=%v", pos)
	}
}

func TestSmoothdampStepTauGuardFallsBackNotShortCircuits(t *testing.T) {
	// A hard "tau<=0 -> return target" shortcut and this guard ("tau<=0 ->
	// tau=1") both jump straight to target when dt is large relative to
	// 1ms, so that case can't distinguish them. A small dt can: with the
	// guard's tau=1 fallback, dt=1ms gives alpha=1-exp(-1)~=0.632, which
	// must land strictly between pos and target, not exactly at target.
	for _, tau := range []float64{0, -5} {
		got := smoothdampStep(0, 100, tau, 1)
		if got <= 0 || got >= 100 {
			t.Fatalf("smoothdampStep(0, 100, %v, 1) = %v, want strictly between 0 and 100 (tau<=0 must fall back to tau=1, not short-circuit to target)", tau, got)
		}
		want := 1 - math.Exp(-1)
		if math.Abs(got-want*100) > 1e-9 {
			t.Fatalf("smoothdampStep(0, 100, %v, 1) = %v, want %v (tau=1 fallback formula)", tau, got, want*100)
		}
	}
}

// gridFixture returns a clean idealized 3x3 grid (rows 0/10/20, columns
// 0/30/60, each pane 30x10) plus a pellicle-style status-strip pair at
// left=90 sharing a column but not a row with anything else — the shapes
// check-plan asked this to cover: a pane with both row- and column-mates,
// and a pane sharing a coordinate with an unrelated pane without actually
// being its sibling (Strip's top=0 matches row 1's top, but its height
// doesn't, so it must NOT be grouped into row 1).
func gridFixture() []paneGeom {
	return []paneGeom{
		{id: "A", left: 0, top: 0, width: 30, height: 10},
		{id: "B", left: 30, top: 0, width: 30, height: 10},
		{id: "C", left: 60, top: 0, width: 30, height: 10},
		{id: "D", left: 0, top: 10, width: 30, height: 10},
		{id: "E", left: 30, top: 10, width: 30, height: 10},
		{id: "F", left: 60, top: 10, width: 30, height: 10},
		{id: "G", left: 0, top: 20, width: 30, height: 10},
		{id: "H", left: 30, top: 20, width: 30, height: 10},
		{id: "I", left: 60, top: 20, width: 30, height: 10},
		{id: "Strip", left: 90, top: 0, width: 20, height: 3},
		{id: "Claude", left: 90, top: 3, width: 20, height: 27},
	}
}

func idsOf(panes []paneGeom) []string {
	ids := make([]string, len(panes))
	for i, p := range panes {
		ids[i] = p.id
	}
	return ids
}

func assertIDs(t *testing.T, label string, got []paneGeom, want ...string) {
	t.Helper()
	gotIDs := idsOf(got)
	if len(gotIDs) != len(want) {
		t.Fatalf("%s: got %v, want %v", label, gotIDs, want)
	}
	seen := map[string]bool{}
	for _, id := range gotIDs {
		seen[id] = true
	}
	for _, w := range want {
		if !seen[w] {
			t.Fatalf("%s: got %v, want %v (missing %q)", label, gotIDs, want, w)
		}
	}
}

func TestSiblingGroupsGridCorner(t *testing.T) {
	panes := gridFixture()
	rowMates, colMates := siblingGroups(panes, "A")
	assertIDs(t, "A row-mates", rowMates, "A", "B", "C")
	assertIDs(t, "A col-mates", colMates, "A", "D", "G")
}

func TestSiblingGroupsGridCenter(t *testing.T) {
	panes := gridFixture()
	rowMates, colMates := siblingGroups(panes, "E")
	assertIDs(t, "E row-mates", rowMates, "D", "E", "F")
	assertIDs(t, "E col-mates", colMates, "B", "E", "H")
}

func TestSiblingGroupsPellicleStripAloneOnRowAxis(t *testing.T) {
	panes := gridFixture()
	rowMates, colMates := siblingGroups(panes, "Strip")
	// Strip shares top=0 with row 1 but not height — must not be pulled
	// into that row's group despite the coordinate collision.
	assertIDs(t, "Strip row-mates", rowMates, "Strip")
	assertIDs(t, "Strip col-mates", colMates, "Strip", "Claude")
}

func TestSiblingGroupsUnknownPane(t *testing.T) {
	rowMates, colMates := siblingGroups(gridFixture(), "nonexistent")
	if rowMates != nil || colMates != nil {
		t.Fatalf("expected nil, nil for an unknown pane id, got %v, %v", rowMates, colMates)
	}
}

func TestTileAxisTargetsShareMathAndSkip(t *testing.T) {
	panes := gridFixture()
	rowMates, _ := siblingGroups(panes, "E")
	targets := tileAxisTargets(rowMates, "E", 3.0, func(p paneGeom) int { return p.width })
	// group [D,E,F], each width 30, total=90. denom=3+2=5, target=int(3/5*90)=54,
	// rest=(90-54)/2=18. F is last and != addressed, so F is the one left
	// for tmux to absorb the remainder into — D and E get explicit targets.
	want := map[string]int{"D": 18, "E": 54}
	if len(targets) != len(want) {
		t.Fatalf("tileAxisTargets = %v, want %v", targets, want)
	}
	for k, v := range want {
		if targets[k] != v {
			t.Fatalf("tileAxisTargets[%q] = %v, want %v (full: %v)", k, targets[k], v, targets)
		}
	}
	if _, ok := targets["F"]; ok {
		t.Fatalf("F should be left unresized (remainder pane), got an explicit target: %v", targets)
	}
}

func TestTileAxisTargetsNeverSkipsAddressedPane(t *testing.T) {
	// Addressed pane happens to be last in the group's natural order —
	// must still get an explicit target rather than being the one skipped.
	group := []paneGeom{
		{id: "X", left: 0, top: 0, width: 30, height: 10},
		{id: "Y", left: 30, top: 0, width: 30, height: 10},
	}
	targets := tileAxisTargets(group, "Y", 3.0, func(p paneGeom) int { return p.width })
	if _, ok := targets["Y"]; !ok {
		t.Fatalf("addressed pane Y must always receive an explicit target, got %v", targets)
	}
}

func TestTileAxisTargetsLoneAxisIsNil(t *testing.T) {
	panes := gridFixture()
	rowMates, _ := siblingGroups(panes, "Strip")
	if got := tileAxisTargets(rowMates, "Strip", 3.0, func(p paneGeom) int { return p.width }); got != nil {
		t.Fatalf("a lone pane on an axis should yield nil targets (nothing to tile against), got %v", got)
	}
}

// fakeRun records every resize-pane invocation instead of shelling to tmux.
type fakeRun struct {
	calls [][]string
}

func (f *fakeRun) run(args ...string) error {
	cp := append([]string(nil), args...)
	f.calls = append(f.calls, cp)
	return nil
}

func TestAnimateResizesDurationZeroFastPath(t *testing.T) {
	fake := &fakeRun{}
	orig := tmuxRunFn
	tmuxRunFn = fake.run
	t.Cleanup(func() { tmuxRunFn = orig })

	src := &tmuxSource{tweenDuration: 0}
	start := time.Now()
	if err := src.animateResizes([]paneTween{
		{pane: "%1", axis: "x", cur: 50, target: 80},
		{pane: "%2", axis: "y", cur: 20, target: 10},
	}); err != nil {
		t.Fatalf("animateResizes: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 20*time.Millisecond {
		t.Fatalf("duration<=0 fast path should return immediately with no ticking, took %v", elapsed)
	}
	want := [][]string{
		{"resize-pane", "-t", "%1", "-x", "80"},
		{"resize-pane", "-t", "%2", "-y", "10"},
	}
	if len(fake.calls) != len(want) {
		t.Fatalf("got %v calls, want %v", fake.calls, want)
	}
	for i := range want {
		if len(fake.calls[i]) != len(want[i]) {
			t.Fatalf("call %d = %v, want %v", i, fake.calls[i], want[i])
		}
		for j := range want[i] {
			if fake.calls[i][j] != want[i][j] {
				t.Fatalf("call %d = %v, want %v", i, fake.calls[i], want[i])
			}
		}
	}
}

func TestAnimateResizesTweensAndDedupsThenSnaps(t *testing.T) {
	fake := &fakeRun{}
	orig := tmuxRunFn
	tmuxRunFn = fake.run
	t.Cleanup(func() { tmuxRunFn = orig })

	src := &tmuxSource{tweenDuration: 150 * time.Millisecond, tweenTauMs: 30}
	if err := src.animateResizes([]paneTween{
		{pane: "%1", axis: "x", cur: 0, target: 2},
	}); err != nil {
		t.Fatalf("animateResizes: %v", err)
	}

	// At most one call per distinct integer between cur and target
	// inclusive — proves the dedup (skip-if-rounded-value-unchanged) logic
	// is doing its job regardless of exactly how many ticks the real
	// wall-clock ticker happened to fire.
	const maxDistinctValues = 3 // 0, 1, 2
	if len(fake.calls) == 0 || len(fake.calls) > maxDistinctValues {
		t.Fatalf("got %d resize-pane calls, want between 1 and %d: %v", len(fake.calls), maxDistinctValues, fake.calls)
	}
	last := fake.calls[len(fake.calls)-1]
	if last[len(last)-1] != "2" {
		t.Fatalf("last call = %v, want a final hard snap to target \"2\"", last)
	}
	// Recorded values must be strictly increasing — a repeated value back
	// to back would mean dedup failed to skip a no-op resize-pane call.
	prev := -1
	for _, c := range fake.calls {
		v := c[len(c)-1]
		n := 0
		for _, r := range v {
			n = n*10 + int(r-'0')
		}
		if n <= prev {
			t.Fatalf("calls not strictly increasing: %v", fake.calls)
		}
		prev = n
	}
}
