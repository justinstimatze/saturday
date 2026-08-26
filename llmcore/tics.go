package llmcore

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
)

// ticsPath is basanite's user-curated vocabulary-tic reference — the file
// basanite itself reads and writes (github.com/justinstimatze/basanite),
// never a copy embedded in this repo. Its own docs are explicit that "the
// list is the user's to curate," so mayor reads it fresh on every prompt
// build rather than baking in a snapshot that would drift from edits.
func ticsPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config", "basanite", "known-tics.txt")
}

// LoadVocabularyTics reads basanite's known-tics.txt and splits it by shape,
// same format basanite documents for the file itself: one entry per line,
// '#' comments and blank lines ignored, a line with a space is a phrase
// matched verbatim, anything else is a single word. Phrases (e.g. "worth
// noting") are distinctive constructions safe to name outright. Single
// words are kept separate and phrased softer in the prompt — basanite's own
// design note is that these are "a reference, not a denylist," since an
// ordinary word like "arm" is sometimes genuinely the right word.
//
// A missing file — basanite not installed, or its state dir absent — is not
// an error: this is optional enrichment of the voice register, never a
// startup dependency. Both return values are nil in that case.
func LoadVocabularyTics() (phrases, words []string) {
	path := ticsPath()
	if path == "" {
		return nil, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, nil
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.Contains(line, " ") {
			phrases = append(phrases, line)
		} else {
			words = append(words, line)
		}
	}
	return phrases, words
}
