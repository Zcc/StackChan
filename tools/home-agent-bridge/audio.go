package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"
)

type audioStatusSnapshot struct {
	Active     bool   `json:"active"`
	StreamID   string `json:"stream_id,omitempty"`
	StartedAt  int64  `json:"started_at,omitempty"`
	DurationMs int    `json:"duration_ms,omitempty"`
	Frames     uint64 `json:"frames,omitempty"`
	Bytes      uint64 `json:"bytes,omitempty"`
}

type audioState struct {
	mu         sync.Mutex
	pending    string
	active     bool
	streamID   string
	startedAt  time.Time
	durationMs int
	frames     uint64
	bytes      uint64
}

func newAudioState() *audioState { return &audioState{} }

func (s *audioState) beginStart(streamID string, durationMs int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.active || s.pending != "" {
		return false
	}
	s.pending = streamID
	s.durationMs = durationMs
	return true
}

func (s *audioState) markStarted(streamID string, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pending = ""
	s.active = true
	s.streamID = streamID
	s.startedAt = now
	s.frames = 0
	s.bytes = 0
}

func (s *audioState) markStopped(streamID, reason string, frames, bytes uint64) {
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

func (s *audioState) addFrame(payloadBytes int) {
	s.mu.Lock()
	s.frames++
	s.bytes += uint64(payloadBytes)
	s.mu.Unlock()
}

func (s *audioState) setCounts(frames, bytes uint64) {
	s.mu.Lock()
	s.frames = frames
	s.bytes = bytes
	s.mu.Unlock()
}

func (s *audioState) snapshot() audioStatusSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := audioStatusSnapshot{
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

func (s *audioState) currentStreamID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.streamID != "" {
		return s.streamID
	}
	return s.pending
}

const (
	audioDefaultDurationMs = 300000
	audioMaxDurationMs     = 300000
	audioSampleRate        = 16000
	audioFrameDurationMs   = 60
)

type audioStartReq struct {
	DurationMs int `json:"duration_ms"`
}

type audioStartResp struct {
	StreamID   string `json:"stream_id"`
	DurationMs int    `json:"duration_ms"`
}

// handleAudioStart sends a PlayAudio JSON config frame to the firmware to begin
// an audio playback stream.
func (b *bridge) handleAudioStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if b.audio == nil {
		b.audio = newAudioState()
	}
	var req audioStartReq
	if r.Body != nil {
		defer r.Body.Close()
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !strings.Contains(err.Error(), "EOF") {
			http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
			return
		}
	}
	dur := req.DurationMs
	if dur <= 0 {
		dur = audioDefaultDurationMs
	}
	if dur > audioMaxDurationMs {
		dur = audioMaxDurationMs
	}
	streamID := newStreamID()
	if !b.audio.beginStart(streamID, dur) {
		http.Error(w, `{"code":"audio.busy"}`, http.StatusConflict)
		return
	}
	cfg := map[string]any{
		"sample_rate":       audioSampleRate,
		"channels":          1,
		"frame_duration_ms": audioFrameDurationMs,
		"duration_ms":       dur,
		"stream_id":         streamID,
	}
	payload, _ := json.Marshal(cfg)
	if err := b.sendAudioPacket(playAudio, payload); err != nil {
		b.audio.markStopped(streamID, "error", 0, 0)
		http.Error(w, "device send failed: "+err.Error(), http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(audioStartResp{StreamID: streamID, DurationMs: dur})
}

// handleAudioFeed sends raw Opus frames to the firmware for playback.
// The request body should contain the raw Opus frame data.
func (b *bridge) handleAudioFeed(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if b.audio == nil {
		b.audio = newAudioState()
	}
	payload, ok := readBody(w, r)
	if !ok {
		return
	}
	if len(payload) == 0 {
		http.Error(w, "empty body", http.StatusBadRequest)
		return
	}
	if err := b.sendAudioPacket(playAudio, payload); err != nil {
		http.Error(w, "device send failed: "+err.Error(), http.StatusBadGateway)
		return
	}
	b.audio.addFrame(len(payload))
	writeJSON(w, map[string]any{"ok": true})
}

// handleAudioStop sends a stop command to the firmware to end playback.
func (b *bridge) handleAudioStop(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if b.audio == nil {
		b.audio = newAudioState()
	}
	streamID := b.audio.currentStreamID()
	if streamID == "" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	payload, _ := json.Marshal(map[string]string{"stream_id": streamID})
	_ = b.sendAudioPacket(stopAudioStream, payload)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"stream_id": streamID, "stopping": "true"})
}

// handleAudioStatus returns the current audio playback state.
func (b *bridge) handleAudioStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if b.audio == nil {
		b.audio = newAudioState()
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(b.audio.snapshot())
}

func (b *bridge) sendAudioPacket(typeID byte, payload []byte) error {
	if b.sendPacketHook != nil {
		return b.sendPacketHook(typeID, payload)
	}
	return b.sendPacket(typeID, payload)
}

type audioStatusEvt struct {
	Event    string `json:"event"`
	StreamID string `json:"stream_id"`
	Reason   string `json:"reason,omitempty"`
	Frames   uint64 `json:"frames,omitempty"`
	Bytes    uint64 `json:"bytes,omitempty"`
}

func (b *bridge) dispatchAudioStatus(payload []byte) {
	if b.audio == nil {
		b.audio = newAudioState()
	}
	var e audioStatusEvt
	if err := json.Unmarshal(payload, &e); err != nil {
		return
	}
	switch e.Event {
	case "started":
		b.audio.markStarted(e.StreamID, time.Now())
	case "stopped":
		b.audio.markStopped(e.StreamID, e.Reason, e.Frames, e.Bytes)
	case "stats":
		b.audio.setCounts(e.Frames, e.Bytes)
	default:
		return
	}
	b.publishEvent("audio."+e.Event, payload)
}
