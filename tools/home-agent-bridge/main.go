package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const (
	jpeg              byte = 0x02
	controlAvatar     byte = 0x03
	controlMotion     byte = 0x04
	startCameraStream byte = 0x05
	stopCameraStream  byte = 0x06
	textMessage       byte = 0x07
	heartbeatPing     byte = 0x10
	heartbeatPong     byte = 0x11
	danceSequence     byte = 0x14
	aimedTakePhoto    byte = 0x1A
)

type bridge struct {
	relayURL string
	deviceID string
	token    string
	apiToken string

	mu           sync.RWMutex
	conn         *websocket.Conn
	connected    bool
	lastSeen     time.Time
	lastSnapshot []byte
	snapshotWait chan []byte
	writeMu      sync.Mutex
}

func main() {
	relay := flag.String("relay", env("STACKCHAN_RELAY_URL", "ws://127.0.0.1:8787/ws"), "relay ws/wss url")
	deviceID := flag.String("device-id", env("STACKCHAN_DEVICE_ID", ""), "StackChan device id")
	addr := flag.String("addr", env("STACKCHAN_BRIDGE_ADDR", "127.0.0.1:8790"), "local HTTP API listen address")
	token := flag.String("token", env("STACKCHAN_RELAY_TOKEN", ""), "relay authorization token")
	apiToken := flag.String("api-token", env("STACKCHAN_BRIDGE_TOKEN", ""), "optional local HTTP API bearer token")
	flag.Parse()

	if *deviceID == "" {
		log.Fatal("device id is required")
	}

	b := &bridge{relayURL: *relay, deviceID: *deviceID, token: *token, apiToken: *apiToken}
	go b.connectLoop(context.Background())

	http.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("ok")) })
	http.HandleFunc("/status", b.withAuth(b.handleStatus))
	http.HandleFunc("/say", b.withAuth(b.handleMessage))
	http.HandleFunc("/message", b.withAuth(b.handleMessage))
	http.HandleFunc("/look", b.withAuth(b.handleLook))
	http.HandleFunc("/motion", b.withAuth(b.handleMotion))
	http.HandleFunc("/avatar", b.withAuth(b.handleAvatar))
	http.HandleFunc("/dance", b.withAuth(b.handleDance))
	http.HandleFunc("/light", b.withAuth(b.handleLight))
	http.HandleFunc("/camera/start", b.withAuth(b.handleCameraStart))
	http.HandleFunc("/camera/stop", b.withAuth(b.handleCameraStop))
	http.HandleFunc("/snapshot", b.withAuth(b.handleSnapshot))
	http.HandleFunc("/snapshot/latest", b.withAuth(b.handleLatestSnapshot))

	log.Printf("home-agent bridge listening on http://%s", *addr)
	log.Fatal(http.ListenAndServe(*addr, nil))
}

func (b *bridge) connectLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		url, err := b.agentRelayURL()
		if err != nil {
			log.Printf("invalid relay url: %v", err)
			time.Sleep(3 * time.Second)
			continue
		}
		headers := http.Header{}
		if b.token != "" {
			headers.Set("Authorization", b.token)
		}
		conn, _, err := websocket.DefaultDialer.Dial(url, headers)
		if err != nil {
			log.Printf("relay connect failed: %v", err)
			time.Sleep(3 * time.Second)
			continue
		}
		log.Printf("connected to relay: %s", url)
		b.setConn(conn, true)
		heartbeatDone := make(chan struct{})
		go b.heartbeatLoop(heartbeatDone)
		b.readLoop(conn)
		close(heartbeatDone)
		b.setConn(nil, false)
		_ = conn.Close()
		time.Sleep(1 * time.Second)
	}
}

func (b *bridge) agentRelayURL() (string, error) {
	u, err := url.Parse(b.relayURL)
	if err != nil {
		return "", err
	}
	q := u.Query()
	q.Set("role", "agent")
	q.Set("deviceId", b.deviceID)
	u.RawQuery = q.Encode()
	return u.String(), nil
}

func (b *bridge) heartbeatLoop(done <-chan struct{}) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			return
		case <-ticker.C:
			if err := b.sendPacket(heartbeatPing, nil); err != nil {
				return
			}
		}
	}
}

func (b *bridge) readLoop(conn *websocket.Conn) {
	conn.SetReadLimit(8 << 20)
	for {
		messageType, data, err := conn.ReadMessage()
		if err != nil {
			log.Printf("relay read ended: %v", err)
			return
		}
		b.mu.Lock()
		b.lastSeen = time.Now()
		b.mu.Unlock()

		if messageType != websocket.BinaryMessage || len(data) < 5 {
			continue
		}
		typeID := data[0]
		payload := data[5:]
		switch typeID {
		case jpeg, aimedTakePhoto:
			b.mu.Lock()
			b.lastSnapshot = append([]byte(nil), payload...)
			waiter := b.snapshotWait
			b.snapshotWait = nil
			b.mu.Unlock()
			if waiter != nil {
				select {
				case waiter <- append([]byte(nil), payload...):
				default:
				}
			}
		case heartbeatPing:
			_ = b.sendPacket(heartbeatPong, nil)
		}
	}
}

func (b *bridge) setConn(conn *websocket.Conn, connected bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.conn = conn
	b.connected = connected
	b.lastSeen = time.Now()
}

func (b *bridge) sendPacket(typeID byte, payload []byte) error {
	b.mu.RLock()
	conn := b.conn
	connected := b.connected
	b.mu.RUnlock()
	if !connected || conn == nil {
		return fmt.Errorf("bridge is not connected to relay")
	}

	buf := bytes.NewBuffer(make([]byte, 0, 5+len(payload)))
	buf.WriteByte(typeID)
	_ = binary.Write(buf, binary.BigEndian, uint32(len(payload)))
	buf.Write(payload)
	b.writeMu.Lock()
	defer b.writeMu.Unlock()
	return conn.WriteMessage(websocket.BinaryMessage, buf.Bytes())
}

func (b *bridge) withAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if b.apiToken != "" && r.Header.Get("Authorization") != "Bearer "+b.apiToken {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

func (b *bridge) handleStatus(w http.ResponseWriter, r *http.Request) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	writeJSON(w, map[string]any{
		"connected":       b.connected,
		"deviceId":        b.deviceID,
		"lastSeen":        b.lastSeen.Format(time.RFC3339),
		"hasSnapshot":     len(b.lastSnapshot) > 0,
		"snapshotByteLen": len(b.lastSnapshot),
		"relayUrl":        b.relayURL,
	})
}

func (b *bridge) handleMessage(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name    string `json:"name"`
		Content string `json:"content"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Name == "" {
		req.Name = "HomeAgent"
	}
	payload, _ := json.Marshal(req)
	respondErr(w, b.sendPacket(textMessage, payload))
}

func (b *bridge) handleLook(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Yaw   int `json:"yaw"`
		Pitch int `json:"pitch"`
		Speed int `json:"speed"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Speed <= 0 {
		req.Speed = 500
	}
	payload, _ := json.Marshal(map[string]any{
		"yawServo": map[string]any{
			"angle": req.Yaw,
			"speed": req.Speed,
		},
		"pitchServo": map[string]any{
			"angle": req.Pitch,
			"speed": req.Speed,
		},
	})
	respondErr(w, b.sendPacket(controlMotion, payload))
}

func (b *bridge) handleMotion(w http.ResponseWriter, r *http.Request) {
	payload, ok := readBody(w, r)
	if !ok {
		return
	}
	respondErr(w, b.sendPacket(controlMotion, payload))
}

func (b *bridge) handleAvatar(w http.ResponseWriter, r *http.Request) {
	payload, ok := readBody(w, r)
	if !ok {
		return
	}
	respondErr(w, b.sendPacket(controlAvatar, payload))
}

func (b *bridge) handleDance(w http.ResponseWriter, r *http.Request) {
	payload, ok := readBody(w, r)
	if !ok {
		return
	}
	respondErr(w, b.sendPacket(danceSequence, payload))
}

func (b *bridge) handleLight(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Color      string `json:"color"`
		LeftColor  string `json:"leftColor"`
		RightColor string `json:"rightColor"`
		DurationMs int    `json:"durationMs"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}

	if req.Color != "" {
		if req.LeftColor == "" {
			req.LeftColor = req.Color
		}
		if req.RightColor == "" {
			req.RightColor = req.Color
		}
	}
	if req.LeftColor == "" {
		req.LeftColor = "#000000"
	}
	if req.RightColor == "" {
		req.RightColor = req.LeftColor
	}
	if req.DurationMs <= 0 {
		req.DurationMs = 1000
	}

	left, err := normalizeHexColor(req.LeftColor)
	if err != nil {
		http.Error(w, "leftColor: "+err.Error(), http.StatusBadRequest)
		return
	}
	right, err := normalizeHexColor(req.RightColor)
	if err != nil {
		http.Error(w, "rightColor: "+err.Error(), http.StatusBadRequest)
		return
	}

	payload, _ := json.Marshal([]map[string]any{{
		"leftEye":       neutralLightPart(),
		"rightEye":      neutralLightPart(),
		"mouth":         neutralMouthPart(),
		"yawServo":      map[string]any{"angle": 0, "speed": 0},
		"pitchServo":    map[string]any{"angle": 0, "speed": 0},
		"leftRgbColor":  left,
		"rightRgbColor": right,
		"durationMs":    req.DurationMs,
	}})
	respondErr(w, b.sendPacket(danceSequence, payload))
}

func (b *bridge) handleCameraStart(w http.ResponseWriter, r *http.Request) {
	respondErr(w, b.sendPacket(startCameraStream, nil))
}

func (b *bridge) handleCameraStop(w http.ResponseWriter, r *http.Request) {
	respondErr(w, b.sendPacket(stopCameraStream, nil))
}

func (b *bridge) handleLatestSnapshot(w http.ResponseWriter, r *http.Request) {
	b.mu.RLock()
	img := append([]byte(nil), b.lastSnapshot...)
	b.mu.RUnlock()
	if len(img) == 0 {
		http.Error(w, "no snapshot cached", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "image/jpeg")
	_, _ = w.Write(img)
}

func (b *bridge) handleSnapshot(w http.ResponseWriter, r *http.Request) {
	waiter := make(chan []byte, 1)
	b.mu.Lock()
	b.snapshotWait = waiter
	b.mu.Unlock()
	if err := b.sendPacket(aimedTakePhoto, nil); err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	select {
	case img := <-waiter:
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write(img)
	case <-time.After(8 * time.Second):
		http.Error(w, "snapshot timeout", http.StatusGatewayTimeout)
	}
}

func decodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	defer r.Body.Close()
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return false
	}
	return true
}

func readBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	defer r.Body.Close()
	data, err := io.ReadAll(io.LimitReader(r.Body, 64*1024))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return nil, false
	}
	return data, true
}

func respondErr(w http.ResponseWriter, err error) {
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func normalizeHexColor(value string) (string, error) {
	if len(value) == 6 {
		value = "#" + value
	}
	if len(value) != 7 || value[0] != '#' {
		return "", fmt.Errorf("expected #RRGGBB")
	}
	for _, ch := range value[1:] {
		if !((ch >= '0' && ch <= '9') || (ch >= 'a' && ch <= 'f') || (ch >= 'A' && ch <= 'F')) {
			return "", fmt.Errorf("expected #RRGGBB")
		}
	}
	return value, nil
}

func neutralLightPart() map[string]any {
	return map[string]any{
		"position": map[string]any{"x": 0, "y": 0},
		"rotation": 0,
		"weight":   100,
		"size":     0,
	}
}

func neutralMouthPart() map[string]any {
	part := neutralLightPart()
	part["weight"] = 0
	return part
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func env(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
