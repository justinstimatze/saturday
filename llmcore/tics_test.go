package llmcore

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadVocabularyTicsSplitsPhrasesFromWords(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	confDir := filepath.Join(dir, ".config", "basanite")
	if err := os.MkdirAll(confDir, 0o755); err != nil {
		t.Fatal(err)
	}
	content := "# comment\n\nload-bearing\nworth noting\nsubstrate\n"
	if err := os.WriteFile(filepath.Join(confDir, "known-tics.txt"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	phrases, words := LoadVocabularyTics()
	if len(phrases) != 1 || phrases[0] != "worth noting" {
		t.Errorf("phrases = %v, want [\"worth noting\"]", phrases)
	}
	if len(words) != 2 || words[0] != "load-bearing" || words[1] != "substrate" {
		t.Errorf("words = %v, want [load-bearing substrate]", words)
	}
}

func TestLoadVocabularyTicsMissingFileReturnsNil(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	phrases, words := LoadVocabularyTics()
	if phrases != nil || words != nil {
		t.Errorf("expected nil, nil for a missing file, got %v, %v", phrases, words)
	}
}

func TestEffigyForPromptAppendsVocabularyBlockWhenTicsExist(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	confDir := filepath.Join(dir, ".config", "basanite")
	if err := os.MkdirAll(confDir, 0o755); err != nil {
		t.Fatal(err)
	}
	content := "load-bearing\nworth noting\n"
	if err := os.WriteFile(filepath.Join(confDir, "known-tics.txt"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	got := EffigyForPrompt()
	if !strings.Contains(got, "VOCABULARY[") {
		t.Errorf("expected a VOCABULARY block, got:\n%s", got)
	}
	if !strings.Contains(got, "load-bearing") || !strings.Contains(got, "worth noting") {
		t.Errorf("expected both tics present, got:\n%s", got)
	}
}
