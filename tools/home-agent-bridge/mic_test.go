package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
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
