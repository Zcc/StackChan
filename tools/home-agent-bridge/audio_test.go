package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestAudioStateLifecycle(t *testing.T) {
	s := newAudioState()
	if s.snapshot().Active {
		t.Fatal("new state should be inactive")
	}
	if !s.beginStart("abc", 300000) {
		t.Fatal("first beginStart should succeed")
	}
	if s.beginStart("def", 300000) {
		t.Fatal("second beginStart should fail while pending")
	}
	s.markStarted("abc", time.Unix(100, 0))
	snap := s.snapshot()
	if !snap.Active || snap.StreamID != "abc" || snap.DurationMs != 300000 {
		t.Fatalf("snapshot mismatch: %+v", snap)
	}
	s.addFrame(120)
	s.addFrame(130)
	snap = s.snapshot()
	if snap.Frames != 2 || snap.Bytes != 250 {
		t.Fatalf("frame counting wrong: %+v", snap)
	}
	s.markStopped("abc", "user", 2, 250)
	if s.snapshot().Active {
		t.Fatal("should be inactive after stop")
	}
}

func TestAudioStateIgnoresWrongStreamID(t *testing.T) {
	s := newAudioState()
	s.beginStart("abc", 10000)
	s.markStarted("abc", time.Now())
	s.markStopped("wrong-id", "user", 0, 0)
	if !s.snapshot().Active {
		t.Fatal("stop with wrong stream_id should be ignored")
	}
}

func TestAudioStateSetCounts(t *testing.T) {
	s := newAudioState()
	s.beginStart("x", 1000)
	s.markStarted("x", time.Now())
	s.setCounts(42, 9999)
	snap := s.snapshot()
	if snap.Frames != 42 || snap.Bytes != 9999 {
		t.Fatalf("setCounts: %+v", snap)
	}
}

func TestAudioStateCurrentStreamID(t *testing.T) {
	s := newAudioState()
	if s.currentStreamID() != "" {
		t.Fatal("idle should return empty")
	}
	s.beginStart("pending-id", 1000)
	if s.currentStreamID() != "pending-id" {
		t.Fatalf("pending should return pending id, got %s", s.currentStreamID())
	}
	s.markStarted("pending-id", time.Now())
	if s.currentStreamID() != "pending-id" {
		t.Fatalf("active should return stream id, got %s", s.currentStreamID())
	}
}

func newTestBridgeWithAudio() *bridge {
	b := &bridge{
		mic:   newMicState(),
		audio: newAudioState(),
	}
	b.sendPacketHook = func(byte, []byte) error { return nil }
	return b
}

func TestHandleAudioStartAccepted(t *testing.T) {
	b := newTestBridgeWithAudio()
	req := httptest.NewRequest(http.MethodPost, "/audio/start", strings.NewReader(`{"duration_ms":60000}`))
	w := httptest.NewRecorder()
	b.handleAudioStart(w, req)
	if w.Code != http.StatusAccepted {
		t.Fatalf("want 202, got %d body=%s", w.Code, w.Body.String())
	}
	var resp audioStartResp
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.StreamID == "" {
		t.Fatal("stream_id should not be empty")
	}
	if resp.DurationMs != 60000 {
		t.Fatalf("duration_ms want 60000, got %d", resp.DurationMs)
	}
}

func TestHandleAudioStartCapsDuration(t *testing.T) {
	b := newTestBridgeWithAudio()
	req := httptest.NewRequest(http.MethodPost, "/audio/start", strings.NewReader(`{"duration_ms":999999}`))
	w := httptest.NewRecorder()
	b.handleAudioStart(w, req)
	if w.Code != http.StatusAccepted {
		t.Fatalf("want 202, got %d", w.Code)
	}
	var resp audioStartResp
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.DurationMs != 300000 {
		t.Fatalf("duration not capped: got %d", resp.DurationMs)
	}
}

func TestHandleAudioStartConflict(t *testing.T) {
	b := newTestBridgeWithAudio()
	req1 := httptest.NewRequest(http.MethodPost, "/audio/start", strings.NewReader(`{}`))
	w1 := httptest.NewRecorder()
	b.handleAudioStart(w1, req1)
	if w1.Code != http.StatusAccepted {
		t.Fatalf("first start want 202, got %d", w1.Code)
	}

	req2 := httptest.NewRequest(http.MethodPost, "/audio/start", strings.NewReader(`{}`))
	w2 := httptest.NewRecorder()
	b.handleAudioStart(w2, req2)
	if w2.Code != http.StatusConflict {
		t.Fatalf("second start want 409, got %d body=%s", w2.Code, w2.Body.String())
	}
}

func TestHandleAudioStopWhenIdle(t *testing.T) {
	b := newTestBridgeWithAudio()
	req := httptest.NewRequest(http.MethodPost, "/audio/stop", nil)
	w := httptest.NewRecorder()
	b.handleAudioStop(w, req)
	if w.Code != http.StatusNoContent {
		t.Fatalf("stop while idle should be 204, got %d", w.Code)
	}
}

func TestHandleAudioStopWhenActive(t *testing.T) {
	b := newTestBridgeWithAudio()
	req1 := httptest.NewRequest(http.MethodPost, "/audio/start", strings.NewReader(`{}`))
	w1 := httptest.NewRecorder()
	b.handleAudioStart(w1, req1)

	req := httptest.NewRequest(http.MethodPost, "/audio/stop", nil)
	w := httptest.NewRecorder()
	b.handleAudioStop(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("stop want 200, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), `"stopping"`) {
		t.Fatalf("response should contain stopping: %s", w.Body.String())
	}
}

func TestHandleAudioStatusReflectsLifecycle(t *testing.T) {
	b := newTestBridgeWithAudio()

	req := httptest.NewRequest(http.MethodGet, "/audio/status", nil)
	w := httptest.NewRecorder()
	b.handleAudioStatus(w, req)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"active":false`) {
		t.Fatalf("want inactive, got %s", w.Body.String())
	}

	req2 := httptest.NewRequest(http.MethodPost, "/audio/start", strings.NewReader(`{}`))
	w2 := httptest.NewRecorder()
	b.handleAudioStart(w2, req2)
	var resp audioStartResp
	_ = json.Unmarshal(w2.Body.Bytes(), &resp)
	b.audio.markStarted(resp.StreamID, time.Now())

	req3 := httptest.NewRequest(http.MethodGet, "/audio/status", nil)
	w3 := httptest.NewRecorder()
	b.handleAudioStatus(w3, req3)
	if !strings.Contains(w3.Body.String(), `"active":true`) {
		t.Fatalf("want active, got %s", w3.Body.String())
	}
}

func TestHandleAudioFeed(t *testing.T) {
	b := newTestBridgeWithAudio()
	b.audio.beginStart("test-stream", 300000)
	b.audio.markStarted("test-stream", time.Now())

	req := httptest.NewRequest(http.MethodPost, "/audio/feed", strings.NewReader("fake-opus-data"))
	w := httptest.NewRecorder()
	b.handleAudioFeed(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("feed want 200, got %d body=%s", w.Code, w.Body.String())
	}
	if b.audio.snapshot().Frames != 1 {
		t.Fatalf("frame count should be 1, got %d", b.audio.snapshot().Frames)
	}
}

func TestHandleAudioFeedEmptyBody(t *testing.T) {
	b := newTestBridgeWithAudio()
	req := httptest.NewRequest(http.MethodPost, "/audio/feed", strings.NewReader(""))
	w := httptest.NewRecorder()
	b.handleAudioFeed(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("empty feed should be 400, got %d", w.Code)
	}
}

func TestDispatchAudioStatusUpdatesState(t *testing.T) {
	b := newTestBridgeWithAudio()
	b.subscribers = make(map[string]*subscriber)

	b.audio.beginStart("s1", 300000)

	b.dispatchAudioStatus([]byte(`{"event":"started","stream_id":"s1"}`))
	if !b.audio.snapshot().Active {
		t.Fatal("started event should activate audio state")
	}

	b.dispatchAudioStatus([]byte(`{"event":"stats","stream_id":"s1","frames":50,"bytes":6000}`))
	snap := b.audio.snapshot()
	if snap.Frames != 50 || snap.Bytes != 6000 {
		t.Fatalf("stats event not applied: %+v", snap)
	}

	b.dispatchAudioStatus([]byte(`{"event":"stopped","stream_id":"s1","reason":"timeout","frames":100,"bytes":12000}`))
	if b.audio.snapshot().Active {
		t.Fatal("stopped event should deactivate audio state")
	}
}

func TestDispatchAudioStatusIgnoresUnknownEvent(t *testing.T) {
	b := newTestBridgeWithAudio()
	b.subscribers = make(map[string]*subscriber)

	b.audio.beginStart("s1", 300000)
	b.audio.markStarted("s1", time.Now())

	b.dispatchAudioStatus([]byte(`{"event":"unknown","stream_id":"s1"}`))
	if !b.audio.snapshot().Active {
		t.Fatal("unknown event should not change state")
	}
}

func TestMapAudioCapabilityError(t *testing.T) {
	cases := map[string]int{
		"busy":         http.StatusConflict,
		"unavailable":  http.StatusServiceUnavailable,
		"invalid_args": http.StatusBadRequest,
		"unknown":      http.StatusBadGateway,
	}
	for code, want := range cases {
		if got := mapCapabilityError("audio", code); got != want {
			t.Fatalf("audio/%s want %d got %d", code, want, got)
		}
	}
}

func TestCapabilitiesIncludesAudio(t *testing.T) {
	b := &bridge{}
	req := httptest.NewRequest(http.MethodGet, "/capabilities", nil)
	rec := httptest.NewRecorder()
	b.handleCapabilities(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"audio"`) || !strings.Contains(body, `"format":"opus"`) {
		t.Fatalf("audio capability missing: %s", body)
	}
}
