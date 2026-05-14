package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMapCapabilityError(t *testing.T) {
	if got := mapCapabilityError("bad_request"); got != http.StatusUnprocessableEntity {
		t.Fatalf("bad_request => %d", got)
	}
	if got := mapCapabilityError("driver_unavailable"); got != http.StatusServiceUnavailable {
		t.Fatalf("driver_unavailable => %d", got)
	}
	if got := mapCapabilityError("not_implemented"); got != http.StatusConflict {
		t.Fatalf("not_implemented => %d", got)
	}
}

func TestParseEventFilter(t *testing.T) {
	got := parseEventFilter("imu, headTouch,screenTouch")
	if _, ok := got["imu"]; !ok {
		t.Fatal("imu missing")
	}
	if _, ok := got["headTouch"]; !ok {
		t.Fatal("headTouch missing")
	}
	if _, ok := got["screenTouch"]; !ok {
		t.Fatal("screenTouch missing")
	}
}

func TestPublishEventDropsOldestWhenSubscriberIsSlow(t *testing.T) {
	b := &bridge{
		subscribers: map[string]*subscriber{},
	}
	sub := &subscriber{
		ch:    make(chan event, 1),
		types: map[string]struct{}{"imu": {}},
	}
	b.subscribers["s1"] = sub

	sub.ch <- event{Type: "imu", Raw: "old"}
	b.publishEvent("imu", []byte(`{"event":"shake"}`))

	if sub.dropped != 1 {
		t.Fatalf("expected dropped=1, got %d", sub.dropped)
	}
	got := <-sub.ch
	if got.Type != "imu" {
		t.Fatalf("unexpected event type: %s", got.Type)
	}
}

func TestHandleCapabilities(t *testing.T) {
	b := &bridge{}
	req := httptest.NewRequest(http.MethodGet, "/capabilities", nil)
	rec := httptest.NewRecorder()
	b.handleCapabilities(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("invalid json: %v", err)
	}
	if body["ir"] != "stub" {
		t.Fatalf("expected ir=stub, got %#v", body["ir"])
	}
	if body["imu"] != "available" {
		t.Fatalf("expected imu=available, got %#v", body["imu"])
	}
}

func TestMapMicCapabilityError(t *testing.T) {
	cases := map[string]int{
		"busy":         http.StatusConflict,
		"unavailable":  http.StatusServiceUnavailable,
		"invalid_args": http.StatusBadRequest,
		"unknown":      http.StatusBadGateway,
	}
	for code, want := range cases {
		if got := mapCapabilityError("mic", code); got != want {
			t.Fatalf("mic/%s want %d got %d", code, want, got)
		}
	}
}

func TestCapabilitiesIncludesMic(t *testing.T) {
	b := &bridge{}
	req := httptest.NewRequest(http.MethodGet, "/capabilities", nil)
	rec := httptest.NewRecorder()
	b.handleCapabilities(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"mic"`) || !strings.Contains(body, `"format":"opus"`) || !strings.Contains(body, `"frame_duration_ms":60`) {
		t.Fatalf("mic capability missing: %s", body)
	}
}
