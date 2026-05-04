// router_backtest — Saturday's second validation gate.
//
// Tests routing accuracy: given an utterance and N candidate session states
// (target + distractors), can a small LLM pick the right session?
//
// Stronger oracle than the expander: ground truth is the project the sample
// actually came from. Routing prompt + cache plumbing live in saturday/llmcore.
//
// Usage:
//
//	cp ../.env.example .env && $EDITOR .env       # set ANTHROPIC_API_KEY
//	go run main.go --corpus ../corpus.example.json --candidates 4
package main

import (
	"encoding/csv"
	"encoding/json"
	"flag"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"sort"
	"strings"

	llm "saturday/llmcore"
)

type Sample struct {
	Utterance   string    `json:"utterance"`
	State       llm.State `json:"state"`
	SourceJSONL string    `json:"source_jsonl"`
	EventIdx    int       `json:"event_idx"`
}

// --- corpus loading ---

func loadCorpus(path string) ([]Sample, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var samples []Sample
	if err := json.Unmarshal(data, &samples); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return samples, nil
}

// uniqueProjects returns the distinct project names in the corpus.
// Routing across only one project is meaningless, so we use this to assert.
func uniqueProjects(samples []Sample) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range samples {
		p := s.State.Project
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// --- candidate set construction ---

// pickDistractors returns k-1 sample indices != targetIdx whose project
// differs from the target's. If fewer than k-1 are available, returns what
// it can (test still runs, just narrower).
func pickDistractors(samples []Sample, targetIdx, k int, rng *rand.Rand) []int {
	target := samples[targetIdx].State.Project
	pool := make([]int, 0, len(samples))
	for i, s := range samples {
		if i == targetIdx || s.State.Project == target {
			continue
		}
		pool = append(pool, i)
	}
	rng.Shuffle(len(pool), func(i, j int) { pool[i], pool[j] = pool[j], pool[i] })
	want := k - 1
	if want > len(pool) {
		want = len(pool)
	}
	return pool[:want]
}

// buildCandidates returns the candidate states (target + distractors) shuffled,
// plus the target's index in the shuffled slice.
func buildCandidates(samples []Sample, targetIdx, k int, rng *rand.Rand) ([]llm.State, int) {
	indices := append([]int{targetIdx}, pickDistractors(samples, targetIdx, k, rng)...)
	rng.Shuffle(len(indices), func(i, j int) { indices[i], indices[j] = indices[j], indices[i] })
	states := make([]llm.State, len(indices))
	targetPos := 0
	for i, idx := range indices {
		states[i] = samples[idx].State
		if idx == targetIdx {
			targetPos = i
		}
	}
	return states, targetPos
}

// --- helpers ---

func head(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func oneLine(s string) string {
	return strings.NewReplacer("\n", " ", "\r", " ", `"`, `'`).Replace(s)
}

func getStr(m map[string]any, k string) string {
	if v, ok := m[k].(string); ok {
		return v
	}
	return ""
}

func getInt(m map[string]any, k string) (int, bool) {
	switch v := m[k].(type) {
	case float64:
		return int(v), true
	case int:
		return v, true
	}
	return 0, false
}

// --- main ---

func main() {
	scriptDir := func() string {
		exe, err := os.Executable()
		if err == nil {
			return filepath.Dir(exe)
		}
		return "."
	}()
	if _, err := os.Stat(filepath.Join(scriptDir, "main.go")); err != nil {
		if wd, err := os.Getwd(); err == nil {
			scriptDir = wd
		}
	}

	defaultEnv := filepath.Join(scriptDir, "..", ".env")
	defaultOut := filepath.Join(scriptDir, "results.csv")
	defaultCache := filepath.Join(scriptDir, ".cache")
	defaultCorpus := filepath.Join(scriptDir, "..", "corpus.example.json")

	corpusPath := flag.String("corpus", defaultCorpus, "frozen corpus path")
	candidates := flag.Int("candidates", 4, "number of candidate sessions per test (target + N-1 distractors)")
	seed := flag.Int("seed", 42, "RNG seed for distractor selection")
	out := flag.String("out", defaultOut, "output CSV path")
	envPath := flag.String("env", defaultEnv, ".env file with ANTHROPIC_API_KEY")
	cacheDir := flag.String("cache", defaultCache, "directory for cached LLM responses")
	flag.Parse()

	llm.LoadDotEnv(*envPath)
	apiKey := os.Getenv("ANTHROPIC_API_KEY")
	if apiKey == "" {
		fmt.Fprintln(os.Stderr, "ANTHROPIC_API_KEY not set (checked env and "+*envPath+")")
		os.Exit(1)
	}
	if err := os.MkdirAll(*cacheDir, 0o750); err != nil {
		fmt.Fprintln(os.Stderr, "mkdir cache:", err)
		os.Exit(1)
	}

	samples, err := loadCorpus(*corpusPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "corpus:", err)
		os.Exit(1)
	}
	projects := uniqueProjects(samples)
	if len(projects) < 2 {
		fmt.Fprintf(os.Stderr, "corpus has only %d distinct project(s); routing test needs >=2\n", len(projects))
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "loaded %d samples across %d projects: %v\n", len(samples), len(projects), projects)
	fmt.Fprintf(os.Stderr, "candidates per test: %d\n\n", *candidates)

	type row struct {
		idx                                   int
		utterance, groundTruth, pickedProject string
		targetPos, pickedIdx                  int
		correct                               bool
		confidence                            float64
		rationale                             string
		distractors                           string
	}
	results := make([]row, 0, len(samples))
	correct := 0
	rng := rand.New(rand.NewPCG(uint64(*seed), 0))

	for i, s := range samples {
		// Per-sample RNG, derived from the global seed, so distractors stay
		// stable across runs even if we change the iteration order.
		subRng := rand.New(rand.NewPCG(uint64(*seed), uint64(i)))
		cands, targetPos := buildCandidates(samples, i, *candidates, subRng)
		if len(cands) < 2 {
			continue
		}

		// distractor projects, in candidate order, for the CSV
		distractorNames := make([]string, 0, len(cands)-1)
		for j, c := range cands {
			if j == targetPos {
				continue
			}
			distractorNames = append(distractorNames, c.Project)
		}

		fmt.Fprintf(os.Stderr, "[%2d/%d] %-40s ", i+1, len(samples), oneLine(head(s.Utterance, 38)))
		res, err := llm.RunRoute(apiKey, *cacheDir, s.Utterance, cands)
		if err != nil {
			fmt.Fprintf(os.Stderr, "ERR: %v\n", err)
			results = append(results, row{
				idx: i, utterance: oneLine(head(s.Utterance, 200)),
				groundTruth: s.State.Project, targetPos: targetPos,
				pickedIdx: -1, rationale: err.Error(),
				distractors: strings.Join(distractorNames, "|"),
			})
			continue
		}
		pick, ok := getInt(res, "target_index")
		if !ok || pick < 0 || pick >= len(cands) {
			fmt.Fprintf(os.Stderr, "BAD pick=%v\n", res["target_index"])
			results = append(results, row{
				idx: i, utterance: oneLine(head(s.Utterance, 200)),
				groundTruth: s.State.Project, targetPos: targetPos,
				pickedIdx: -1, rationale: "bad target_index",
				distractors: strings.Join(distractorNames, "|"),
			})
			continue
		}
		isCorrect := pick == targetPos
		if isCorrect {
			correct++
		}
		conf, _ := res["confidence"].(float64)
		marker := "WRONG"
		if isCorrect {
			marker = "ok"
		}
		fmt.Fprintf(os.Stderr, "pick=%d target=%d %s (conf=%.2f)\n", pick, targetPos, marker, conf)
		_ = rng // reserved for any future global jitter
		results = append(results, row{
			idx: i, utterance: oneLine(head(s.Utterance, 200)),
			groundTruth:   s.State.Project,
			pickedProject: cands[pick].Project,
			targetPos:     targetPos,
			pickedIdx:     pick,
			correct:       isCorrect,
			confidence:    conf,
			rationale:     oneLine(head(getStr(res, "rationale"), 300)),
			distractors:   strings.Join(distractorNames, "|"),
		})
	}

	// write CSV
	f, err := os.Create(*out)
	if err != nil {
		fmt.Fprintln(os.Stderr, "create csv:", err)
		os.Exit(1)
	}
	w := csv.NewWriter(f)
	_ = w.Write([]string{
		"idx", "utterance", "ground_truth", "picked_project",
		"target_pos", "picked_idx", "correct", "confidence",
		"distractors", "rationale",
	})
	for _, r := range results {
		_ = w.Write([]string{
			fmt.Sprintf("%d", r.idx), r.utterance, r.groundTruth, r.pickedProject,
			fmt.Sprintf("%d", r.targetPos), fmt.Sprintf("%d", r.pickedIdx),
			fmt.Sprintf("%v", r.correct), fmt.Sprintf("%.2f", r.confidence),
			r.distractors, r.rationale,
		})
	}
	w.Flush()
	f.Close()

	// summary
	n := len(results)
	wrong := n - correct
	acc := 0.0
	if n > 0 {
		acc = float64(correct) / float64(n) * 100
	}
	fmt.Printf("\n=== summary (%d samples, router=%s, candidates=%d) ===\n", n, llm.RouterModel, *candidates)
	fmt.Printf("  correct:   %3d  %5.1f%%\n", correct, acc)
	fmt.Printf("  wrong:     %3d  %5.1f%%\n", wrong, 100-acc)
	fmt.Printf("\n  accuracy:  %d/%d = %.0f%%   (target >=80%%)\n", correct, n, acc)
	fmt.Printf("\nresults: %s\n", *out)
	fmt.Printf("inspect: column -t -s, < %s | less -S\n", *out)
}
