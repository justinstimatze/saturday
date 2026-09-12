package main

import "unicode"

// minMeaningfulChars is the smallest number of alphanumeric characters an
// accumulated utterance must contain before it's treated as real user
// speech worth acting on.
const minMeaningfulChars = 2

// hasMeaningfulContent reports whether s contains enough actual content to
// be worth firing a quick-ack or an orchestrator.Handle call over, as
// opposed to a stray non-content token STT occasionally transcribes from
// background noise — a literal "*" was observed live (2026-08-26) with
// the turn-taking model reporting prs2=0.954, high enough to fire a
// confirmed pause on a single meaningless character, which the classifier
// then defaulted to ask-mode and Saturday spoke an unprompted status
// summary. Requires at least minMeaningfulChars Unicode letters/digits,
// not just non-whitespace bytes — "*" and "..." both fail this; "ok" or
// "no" pass.
func hasMeaningfulContent(s string) bool {
	n := 0
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			n++
			if n >= minMeaningfulChars {
				return true
			}
		}
	}
	return false
}
