package main

import (
	"sync"
	"time"
)

type micStatusSnapshot struct {
	Active     bool   `json:"active"`
	StreamID   string `json:"stream_id,omitempty"`
	StartedAt  int64  `json:"started_at,omitempty"`
	DurationMs int    `json:"duration_ms,omitempty"`
	Frames     uint64 `json:"frames,omitempty"`
	Bytes      uint64 `json:"bytes,omitempty"`
}

type micState struct {
	mu         sync.Mutex
	pending    string
	active     bool
	streamID   string
	startedAt  time.Time
	durationMs int
	frames     uint64
	bytes      uint64
}

func newMicState() *micState { return &micState{} }

func (s *micState) beginStart(streamID string, durationMs int, now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.active || s.pending != "" {
		return false
	}
	s.pending = streamID
	s.durationMs = durationMs
	return true
}

func (s *micState) markStarted(streamID string, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pending = ""
	s.active = true
	s.streamID = streamID
	s.startedAt = now
	s.frames = 0
	s.bytes = 0
}

func (s *micState) markStopped(streamID, reason string, frames, bytes uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.streamID != streamID && s.pending != streamID {
		return
	}
	s.active = false
	s.pending = ""
	s.frames = frames
	s.bytes = bytes
}

func (s *micState) addFrame(payloadBytes int) {
	s.mu.Lock()
	s.frames++
	s.bytes += uint64(payloadBytes)
	s.mu.Unlock()
}

func (s *micState) snapshot() micStatusSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := micStatusSnapshot{
		Active:     s.active,
		StreamID:   s.streamID,
		DurationMs: s.durationMs,
		Frames:     s.frames,
		Bytes:      s.bytes,
	}
	if !s.startedAt.IsZero() {
		out.StartedAt = s.startedAt.UnixMilli()
	}
	return out
}

func (s *micState) currentStreamID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.streamID != "" {
		return s.streamID
	}
	return s.pending
}
