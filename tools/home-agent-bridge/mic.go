package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
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

func (s *micState) setCounts(frames, bytes uint64) {
	s.mu.Lock()
	s.frames = frames
	s.bytes = bytes
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

const (
	micDefaultDurationMs = 30000
	micMaxDurationMs     = 300000
	micFrameDurationMs   = 60
	micSampleRate        = 16000
)

type micStartReq struct {
	DurationMs int `json:"duration_ms"`
}

type micStartResp struct {
	StreamID   string `json:"stream_id"`
	DurationMs int    `json:"duration_ms"`
}

func newStreamID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func (b *bridge) handleMicStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if b.mic == nil {
		b.mic = newMicState()
	}
	var req micStartReq
	if r.Body != nil {
		defer r.Body.Close()
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !strings.Contains(err.Error(), "EOF") {
			http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
			return
		}
	}
	dur := req.DurationMs
	if dur <= 0 {
		dur = micDefaultDurationMs
	}
	if dur > micMaxDurationMs {
		dur = micMaxDurationMs
	}
	streamID := newStreamID()
	if !b.mic.beginStart(streamID, dur, time.Now()) {
		http.Error(w, `{"code":"mic.busy"}`, http.StatusConflict)
		return
	}
	cfg := map[string]any{
		"sample_rate":       micSampleRate,
		"channels":          1,
		"frame_duration_ms": micFrameDurationMs,
		"duration_ms":       dur,
		"stream_id":         streamID,
	}
	payload, _ := json.Marshal(cfg)
	if err := b.sendMicPacket(micStreamStart, payload); err != nil {
		b.mic.markStopped(streamID, "error", 0, 0)
		http.Error(w, "device send failed: "+err.Error(), http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(micStartResp{StreamID: streamID, DurationMs: dur})
}

func (b *bridge) sendMicPacket(typeID byte, payload []byte) error {
	if b.sendPacketHook != nil {
		return b.sendPacketHook(typeID, payload)
	}
	return b.sendPacket(typeID, payload)
}

func (b *bridge) handleMicStop(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if b.mic == nil {
		b.mic = newMicState()
	}
	streamID := b.mic.currentStreamID()
	if streamID == "" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	payload, _ := json.Marshal(map[string]string{"stream_id": streamID})
	_ = b.sendMicPacket(micStreamStop, payload)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"stream_id": streamID, "stopping": "true"})
}

func (b *bridge) handleMicStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if b.mic == nil {
		b.mic = newMicState()
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(b.mic.snapshot())
}

type micStatusEvt struct {
	Event      string `json:"event"`
	StreamID   string `json:"stream_id"`
	Reason     string `json:"reason,omitempty"`
	DurationMs int    `json:"duration_ms,omitempty"`
	Frames     uint64 `json:"frames,omitempty"`
	Bytes      uint64 `json:"bytes,omitempty"`
}

func (b *bridge) dispatchMicStatus(payload []byte) {
	if b.mic == nil {
		b.mic = newMicState()
	}
	var e micStatusEvt
	if err := json.Unmarshal(payload, &e); err != nil {
		return
	}
	switch e.Event {
	case "started":
		b.mic.markStarted(e.StreamID, time.Now())
	case "stopped":
		b.mic.markStopped(e.StreamID, e.Reason, e.Frames, e.Bytes)
	case "stats":
		b.mic.setCounts(e.Frames, e.Bytes)
	default:
		return
	}
	b.publishEvent("mic."+e.Event, payload)
}
