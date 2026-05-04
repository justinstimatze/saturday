# Saturday — Router Backtest

Saturday's second validation gate. Tests whether a small LLM, given a brief
utterance and N candidate session states (one target + distractors), can pick
the right session.

Stronger oracle than the expander: ground truth is the project the corpus
sample actually came from. No fuzzy "would Claude do the right thing" judging.

## What it does

1. Loads `corpus.example.json` (or any frozen corpus from the expander harness).
2. For each sample, builds a candidate set: the sample's own state + `N-1`
   distractor states drawn from other projects (deterministic per-sample seed).
3. Strips the `project` and `session_id` fields so the router has to use real
   signals (cwd, last user turn, last assistant text, last tool use, last tool
   result tail, modified files) rather than label-matching.
4. Calls Haiku as router → `{target_index, confidence, rationale}` via tool-use.
5. Grades correctness against the known target index. Writes `results.csv`.

## Run

```bash
go run main.go --corpus ../corpus.example.json --candidates 4
go run main.go --corpus ../corpus.json --candidates 6 --seed 7
```

Re-runs free — same content-hash caching as the expander harness in `.cache/`.

## Pass criteria

- accuracy ≥ **80%** on `corpus.example.json` with 4 candidates

## Baseline

`claude-haiku-4-5`, 30 samples, 4 candidates, seed=42: **28/30 = 93%**.

The two misses:
- *"add tests for that"* — distractor was a parser-test-fail context that
  read more like "add tests" than the actual target. Confidence 0.75.
- *"what do you mean it's missing, it's at internal/parser/parse.go"* —
  picked a parser-touching distractor instead of ground truth. Confidence 0.95.

The 0.95-on-wrong is the dangerous mode for silent routing. **A spoken
mediator gate at the routing step catches it**: mayor proposes the target
out loud, the user has ~1s to interrupt, and silent inject only fires
on no-interrupt. With the gate, 93% raw routing accuracy is comfortably
shippable.

## Reading `results.csv`

```
column -t -s, < results.csv | less -S
```

Columns: `idx, utterance, ground_truth, picked_project, target_pos,
picked_idx, correct, confidence, distractors, rationale`.

## What this does NOT test

- Disambiguation when two sessions are equally plausible (no clarifier path
  yet — V0.2 will add one)
- Routing degradation as candidate count grows past ~6 (real workload is
  3–4 active sessions, but worth probing later)
- Whisper transcription accuracy on user's vocabulary — separate test
- Live socket-fed routing against `watcher` — integration test, not this one
