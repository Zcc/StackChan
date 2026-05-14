package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
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

func TestHandleMicStartCapsDuration(t *testing.T) {
	b := newTestBridge()
	res := b.startMicForTest(t, `{"duration_ms": 999999}`)
	if res.HTTPStatus != http.StatusAccepted {
		t.Fatalf("want 202, got %d body=%s", res.HTTPStatus, res.Body)
	}
	if res.RequestedDuration != 300000 {
		t.Fatalf("duration not capped: got %d", res.RequestedDuration)
	}
}

func TestHandleMicStartConflict(t *testing.T) {
	b := newTestBridge()
	if r := b.startMicForTest(t, `{}`); r.HTTPStatus != http.StatusAccepted {
		t.Fatalf("first start want 202, got %d", r.HTTPStatus)
	}
	r2 := b.startMicForTest(t, `{}`)
	if r2.HTTPStatus != http.StatusConflict {
		t.Fatalf("second start want 409, got %d body=%s", r2.HTTPStatus, r2.Body)
	}
}

type micStartTestResult struct {
	HTTPStatus        int
	Body              string
	RequestedDuration int
}

func newTestBridge() *bridge {
	b := &bridge{mic: newMicState()}
	b.sendPacketHook = func(byte, []byte) error { return nil }
	return b
}

func (b *bridge) startMicForTest(t *testing.T, body string) micStartTestResult {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/mic/start", strings.NewReader(body))
	w := httptest.NewRecorder()
	b.handleMicStart(w, req)
	res := micStartTestResult{HTTPStatus: w.Code, Body: w.Body.String()}
	if w.Code == http.StatusAccepted {
		var r micStartResp
		_ = json.Unmarshal(w.Body.Bytes(), &r)
		res.RequestedDuration = r.DurationMs
	}
	return res
}

func TestHandleMicStopWhenIdle(t *testing.T) {
	b := newTestBridge()
	req := httptest.NewRequest(http.MethodPost, "/mic/stop", nil)
	w := httptest.NewRecorder()
	b.handleMicStop(w, req)
	if w.Code != http.StatusOK && w.Code != http.StatusNoContent {
		t.Fatalf("stop while idle should be 200/204, got %d", w.Code)
	}
}

func TestHandleMicStopWhenActive(t *testing.T) {
	b := newTestBridge()
	_ = b.startMicForTest(t, `{}`)
	req := httptest.NewRequest(http.MethodPost, "/mic/stop", nil)
	w := httptest.NewRecorder()
	b.handleMicStop(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("stop want 200, got %d", w.Code)
	}
}

func TestHandleMicStatusReflectsLifecycle(t *testing.T) {
	b := newTestBridge()
	{
		req := httptest.NewRequest(http.MethodGet, "/mic/status", nil)
		w := httptest.NewRecorder()
		b.handleMicStatus(w, req)
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"active":false`) {
			t.Fatalf("want inactive, got %s", w.Body.String())
		}
	}
	r := b.startMicForTest(t, `{}`)
	b.mic.markStarted(getStreamIDFromBody(r.Body), time.Now())
	req := httptest.NewRequest(http.MethodGet, "/mic/status", nil)
	w := httptest.NewRecorder()
	b.handleMicStatus(w, req)
	if !strings.Contains(w.Body.String(), `"active":true`) {
		t.Fatalf("want active, got %s", w.Body.String())
	}
}

func getStreamIDFromBody(s string) string {
	var r micStartResp
	_ = json.Unmarshal([]byte(s), &r)
	return r.StreamID
}

func TestDispatchMicStatusUpdatesState(t *testing.T) {
	b := newTestBridge()
	_ = b.startMicForTest(t, `{}`)
	sid := b.mic.currentStreamID()

	b.dispatchMicStatus([]byte(`{"event":"started","stream_id":"` + sid + `","sample_rate":16000,"channels":1,"frame_duration_ms":60,"duration_ms":30000}`))
	if !b.mic.snapshot().Active {
		t.Fatalf("started event should activate mic state")
	}

	b.dispatchMicStatus([]byte(`{"event":"stopped","stream_id":"` + sid + `","reason":"user","frames":12,"bytes":3456}`))
	if b.mic.snapshot().Active {
		t.Fatalf("stopped event should deactivate mic state")
	}
}

func TestMicWSSingleSubscriber(t *testing.T) {
	b := newTestBridge()
	srv := httptest.NewServer(http.HandlerFunc(b.handleMicWS))
	defer srv.Close()

	c1, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatalf("first dial: %v", err)
	}
	defer c1.Close()

	_, resp, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err == nil {
		t.Fatalf("second dial should fail")
	}
	if resp == nil || resp.StatusCode != http.StatusConflict {
		t.Fatalf("want 409, got %#v", resp)
	}
}

func TestMicAudioFanout(t *testing.T) {
	b := newTestBridge()
	srv := httptest.NewServer(http.HandlerFunc(b.handleMicWS))
	defer srv.Close()

	c, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	frame := make([]byte, 16+8)
	for i := range frame {
		frame[i] = byte(i)
	}
	b.dispatchMicAudio(frame)

	_ = c.SetReadDeadline(time.Now().Add(time.Second))
	mt, data, err := c.ReadMessage()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if mt != websocket.BinaryMessage {
		t.Fatalf("expect binary, got %d", mt)
	}
	if !bytes.Equal(data, frame) {
		t.Fatalf("payload mismatch: got %x want %x", data, frame)
	}
}

func TestMicAudioBackpressureDropsSubscriber(t *testing.T) {
	b := newTestBridge()
	sub := &micSub{send: make(chan []byte, 1), done: make(chan struct{})}
	sub.send <- []byte("queued")
	b.micSub = sub

	b.dispatchMicAudio(make([]byte, 64))

	if b.micSubscriberCount() != 0 {
		t.Fatalf("backpressured subscriber should have been dropped")
	}
	if got := b.mic.snapshot().Frames; got != 1 {
		t.Fatalf("device frame should still be counted, got %d", got)
	}
}
