package orchestrator

import (
	"errors"
	"testing"

	llm "saturday/llmcore"
)

func TestAnswerAskStreamingSkipsSpeakStreamWhenAlreadyCancelled(t *testing.T) {
	o := New(Config{
		SpeakStream: func(<-chan string, func() bool) error {
			t.Fatal("SpeakStream should not be called for an already-cancelled call — that's a wasted TTS dial for a reply nobody will hear")
			return nil
		},
	})
	reply, err := o.answerAskStreaming("hello", llm.AskContext{}, func() bool { return true })
	if !errors.Is(err, llm.ErrCancelled) {
		t.Fatalf("err = %v, want llm.ErrCancelled", err)
	}
	if reply != "" {
		t.Errorf("reply = %q, want empty", reply)
	}
}
