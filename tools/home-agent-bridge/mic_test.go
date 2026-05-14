package main

import (
	"testing"
	"time"
)

func TestMicStateLifecycle(t *testing.T) {
	s := newMicState()
	if s.snapshot().Active {
		t.Fatalf("new state should be inactive")
	}
	if !s.beginStart("abc", 30000, time.Unix(100, 0)) {
		t.Fatalf("first beginStart should succeed")
	}
	if s.beginStart("def", 30000, time.Unix(101, 0)) {
		t.Fatalf("second beginStart should fail while pending")
	}
	s.markStarted("abc", time.Unix(102, 0))
	snap := s.snapshot()
	if !snap.Active || snap.StreamID != "abc" || snap.DurationMs != 30000 {
		t.Fatalf("snapshot mismatch: %+v", snap)
	}
	s.markStopped("abc", "user", 10, 1234)
	if s.snapshot().Active {
		t.Fatalf("should be inactive after stop")
	}
}
