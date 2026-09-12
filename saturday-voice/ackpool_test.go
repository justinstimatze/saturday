package main

import (
	"math/rand"
	"testing"
)

func TestAckPoolRandomNilSafe(t *testing.T) {
	var p *ackPool
	if got := p.random(); got != nil {
		t.Errorf("nil *ackPool.random() = %v, want nil", got)
	}
}

func TestAckPoolRandomEmptySafe(t *testing.T) {
	p := &ackPool{}
	if got := p.random(); got != nil {
		t.Errorf("empty ackPool.random() = %v, want nil", got)
	}
}

func TestAckPoolRandomReturnsAClip(t *testing.T) {
	want := [][]float64{{1, 2, 3}, {4, 5, 6}}
	p := &ackPool{clips: want, rng: rand.New(rand.NewSource(1))}
	got := p.random()
	if got == nil {
		t.Fatal("random() = nil, want a clip")
	}
	found := false
	for _, w := range want {
		if len(got) == len(w) {
			found = true
		}
	}
	if !found {
		t.Errorf("random() = %v, not one of %v", got, want)
	}
}
