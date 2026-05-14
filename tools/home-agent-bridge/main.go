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
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const (
	opus              byte = 0x01
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

	// HomeAgent 扩展能力
	getDeviceInfo    byte = 0x20
	getBatteryStatus byte = 0x21
	setBrightness    byte = 0x22
	setVolume        byte = 0x23
	rebootDevice     byte = 0x24
	factoryReset     byte = 0x25
	setRgbLed        byte = 0x26
	showRgbColor     byte = 0x27
	imuEvent         byte = 0x28
	headTouchEvent   byte = 0x29
	screenTouchEvent byte = 0x2A
	buttonEvent      byte = 0x2B
	irSend           byte = 0x30
	irLearnStart     byte = 0x31
	irEvent          byte = 0x32
	nfcRead          byte = 0x33
	nfcWrite         byte = 0x34
	nfcEvent         byte = 0x35
	playAudio        byte = 0x36
	micStreamStart   byte = 0x37
	micStreamStop    byte = 0x38
	micAudio         byte = 0x39
	screenCapture    byte = 0x3A
	sdList           byte = 0x3B
	micStatus        byte = 0x3C
	getDriverHealth  byte = 0x40
	sdRead           byte = 0x42
	sdWrite          byte = 0x43
	servoFeedback    byte = 0x44
	proximityLight   byte = 0x45
	capabilityError  byte = 0x4F
)

type bridge struct {
	relayURL string
	deviceID string
	token    string
	apiToken string

	mu             sync.RWMutex
	conn           *websocket.Conn
	connected      bool
	lastSeen       time.Time
	lastSnapshot   []byte
	snapshotWait   chan []byte
	writeMu        sync.Mutex
	mic            *micState
	sendPacketHook func(byte, []byte) error

	// 等待固件回包的 channel：key = 二进制 type id
	replyWaiters map[byte][]chan []byte

	// SSE 事件订阅
	subscribers map[string]*subscriber
}

type event struct {
	Type    string         `json:"type"`
	Payload map[string]any `json:"payload,omitempty"`
	Raw     string         `json:"raw,omitempty"`
}

type subscriber struct {
	ch      chan event
	dropped uint64
	types   map[string]struct{}
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

	b := &bridge{
		relayURL:     *relay,
		deviceID:     *deviceID,
		token:        *token,
		apiToken:     *apiToken,
		replyWaiters: make(map[byte][]chan []byte),
		subscribers:  make(map[string]*subscriber),
		mic:          newMicState(),
	}
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

	// HomeAgent 扩展 - A 类(固件已实现)
	http.HandleFunc("/device-info", b.withAuth(b.handleDeviceInfo))
	http.HandleFunc("/battery", b.withAuth(b.handleBattery))
	http.HandleFunc("/brightness", b.withAuth(b.handleBrightness))
	http.HandleFunc("/volume", b.withAuth(b.handleVolume))
	http.HandleFunc("/reboot", b.withAuth(b.handleReboot))
	http.HandleFunc("/factory-reset", b.withAuth(b.handleFactoryReset))
	http.HandleFunc("/rgb", b.withAuth(b.handleRgbLeds))
	http.HandleFunc("/rgb/all", b.withAuth(b.handleRgbAll))

	// HomeAgent 扩展 - B 类(固件 stub, 透传)
	http.HandleFunc("/ir/send", b.withAuth(b.handleIrSend))
	http.HandleFunc("/ir/learn/start", b.withAuth(b.handleIrLearn))
	http.HandleFunc("/nfc/read", b.withAuth(b.stubHandler(nfcRead, "nfc.read")))
	http.HandleFunc("/nfc/write", b.withAuth(b.stubHandler(nfcWrite, "nfc.write")))
	http.HandleFunc("/audio/play", b.withAuth(b.stubHandler(playAudio, "audio.play")))
	http.HandleFunc("/mic/start", b.withAuth(b.handleMicStart))
	http.HandleFunc("/mic/stop", b.withAuth(b.handleMicStop))
	http.HandleFunc("/mic/status", b.withAuth(b.handleMicStatus))
	http.HandleFunc("/screen/snapshot", b.withAuth(b.stubHandler(screenCapture, "screen.capture")))
	http.HandleFunc("/sd/list", b.withAuth(b.stubHandler(sdList, "sd.list")))
	http.HandleFunc("/sd/read", b.withAuth(b.stubHandler(sdRead, "sd.read")))
	http.HandleFunc("/sd/write", b.withAuth(b.stubHandler(sdWrite, "sd.write")))

	// 出站事件订阅
	http.HandleFunc("/events", b.withAuth(b.handleEventsSSE))
	http.HandleFunc("/capabilities", b.withAuth(b.handleCapabilities))
	http.HandleFunc("/health/drivers", b.withAuth(b.handleDriverHealth))

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
	if q.Get("clientId") == "" {
		q.Set("clientId", "home-agent-bridge")
	}
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
		case getDeviceInfo, getBatteryStatus, getDriverHealth, capabilityError:
			b.deliverReply(typeID, payload)
		case imuEvent:
			b.publishEvent("imu", payload)
		case headTouchEvent:
			b.publishEvent("headTouch", payload)
		case screenTouchEvent:
			b.publishEvent("screenTouch", payload)
		case buttonEvent:
			b.publishEvent("button", payload)
		case irEvent:
			b.publishEvent("ir", payload)
		case nfcEvent:
			b.publishEvent("nfc", payload)
		case servoFeedback:
			b.publishEvent("servoFeedback", payload)
		case proximityLight:
			b.publishEvent("proximityLight", payload)
		case micStatus:
			b.dispatchMicStatus(payload)
		case micAudio:
			// PCM/Opus 二进制流 - 暂只通过 SSE 通知一次大小; 真正的音频上行后续走专用 endpoint
			b.publishEvent("micAudio", []byte(fmt.Sprintf(`{"bytes":%d}`, len(payload))))
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

// ---------- HomeAgent 扩展能力: 回包等待 / SSE 事件 ----------

func (b *bridge) registerReplyWaiter(typeID byte) chan []byte {
	ch := make(chan []byte, 1)
	b.mu.Lock()
	b.replyWaiters[typeID] = append(b.replyWaiters[typeID], ch)
	b.mu.Unlock()
	return ch
}

func (b *bridge) deliverReply(typeID byte, payload []byte) {
	b.mu.Lock()
	waiters := b.replyWaiters[typeID]
	b.replyWaiters[typeID] = nil
	b.mu.Unlock()
	for _, ch := range waiters {
		select {
		case ch <- append([]byte(nil), payload...):
		default:
		}
	}
}

func (b *bridge) publishEvent(name string, payload []byte) {
	ev := event{Type: name}
	if len(payload) > 0 {
		var obj map[string]any
		if err := json.Unmarshal(payload, &obj); err == nil {
			ev.Payload = obj
		} else {
			ev.Raw = string(payload)
		}
	}
	b.mu.RLock()
	subs := make([]*subscriber, 0, len(b.subscribers))
	for _, sub := range b.subscribers {
		if len(sub.types) > 0 {
			if _, ok := sub.types[name]; !ok {
				continue
			}
		}
		subs = append(subs, sub)
	}
	b.mu.RUnlock()
	for _, sub := range subs {
		if sub == nil {
			continue
		}
		select {
		case sub.ch <- ev:
		default:
			<-sub.ch
			sub.ch <- ev
			sub.dropped++
		}
	}
}

func (b *bridge) requestReply(typeID byte, payload []byte, timeout time.Duration) ([]byte, error) {
	ch := b.registerReplyWaiter(typeID)
	if err := b.sendPacket(typeID, payload); err != nil {
		return nil, err
	}
	select {
	case data := <-ch:
		return data, nil
	case <-time.After(timeout):
		return nil, fmt.Errorf("timeout waiting for response 0x%02X", typeID)
	}
}

// ---------- HomeAgent 扩展能力: HTTP handler 实现 ----------

func (b *bridge) handleDeviceInfo(w http.ResponseWriter, r *http.Request) {
	data, err := b.requestReply(getDeviceInfo, nil, 5*time.Second)
	if err != nil {
		http.Error(w, err.Error(), http.StatusGatewayTimeout)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(data)
}

func (b *bridge) handleBattery(w http.ResponseWriter, r *http.Request) {
	data, err := b.requestReply(getBatteryStatus, nil, 5*time.Second)
	if err != nil {
		http.Error(w, err.Error(), http.StatusGatewayTimeout)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(data)
}

func (b *bridge) handleBrightness(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Value     int  `json:"value"`
		Permanent bool `json:"permanent"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Value < 0 || req.Value > 100 {
		http.Error(w, "value must be 0-100", http.StatusBadRequest)
		return
	}
	payload, _ := json.Marshal(req)
	respondErr(w, b.sendPacket(setBrightness, payload))
}

func (b *bridge) handleVolume(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Value     int  `json:"value"`
		Permanent bool `json:"permanent"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Value < 0 || req.Value > 100 {
		http.Error(w, "value must be 0-100", http.StatusBadRequest)
		return
	}
	payload, _ := json.Marshal(req)
	respondErr(w, b.sendPacket(setVolume, payload))
}

func (b *bridge) handleReboot(w http.ResponseWriter, r *http.Request) {
	respondErr(w, b.sendPacket(rebootDevice, nil))
}

func (b *bridge) handleFactoryReset(w http.ResponseWriter, r *http.Request) {
	respondErr(w, b.sendPacket(factoryReset, nil))
}

type rgbLed struct {
	Index int    `json:"i"`
	R     int    `json:"r"`
	G     int    `json:"g"`
	B     int    `json:"b"`
	Color string `json:"color,omitempty"`
}

func (b *bridge) handleRgbLeds(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Leds []rgbLed `json:"leds"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if len(req.Leds) == 0 {
		http.Error(w, "leds is required", http.StatusBadRequest)
		return
	}
	for i := range req.Leds {
		if req.Leds[i].Color != "" {
			rr, gg, bb, err := parseHexColor(req.Leds[i].Color)
			if err != nil {
				http.Error(w, fmt.Sprintf("leds[%d].color: %s", i, err), http.StatusBadRequest)
				return
			}
			req.Leds[i].R, req.Leds[i].G, req.Leds[i].B = int(rr), int(gg), int(bb)
		}
	}
	payload, _ := json.Marshal(map[string]any{"leds": req.Leds})
	respondErr(w, b.sendPacket(setRgbLed, payload))
}

func (b *bridge) handleRgbAll(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Color string `json:"color"`
		R     int    `json:"r"`
		G     int    `json:"g"`
		B     int    `json:"b"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	rr, gg, bb := uint8(req.R), uint8(req.G), uint8(req.B)
	if req.Color != "" {
		var err error
		rr, gg, bb, err = parseHexColor(req.Color)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}
	payload, _ := json.Marshal(map[string]any{"r": rr, "g": gg, "b": bb})
	respondErr(w, b.sendPacket(showRgbColor, payload))
}

func (b *bridge) handleIrSend(w http.ResponseWriter, r *http.Request) {
	payload, ok := readBody(w, r)
	if !ok {
		return
	}
	respondErr(w, b.sendPacket(irSend, payload))
}

func (b *bridge) handleIrLearn(w http.ResponseWriter, r *http.Request) {
	respondErr(w, b.sendPacket(irLearnStart, nil))
}

// stubHandler 把请求 body 原样转发给指定 type，固件目前会回 CapabilityError。
func (b *bridge) stubHandler(typeID byte, label string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var payload []byte
		if r.Body != nil {
			payload, _ = io.ReadAll(io.LimitReader(r.Body, 64*1024))
			_ = r.Body.Close()
		}
		if err := b.sendPacket(typeID, payload); err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		writeJSON(w, map[string]any{
			"ok":         true,
			"capability": label,
			"note":       "firmware stub: hardware driver not implemented yet, will return capabilityError",
		})
	}
}

// handleEventsSSE 把出站事件通过 Server-Sent Events 推给本地 agent。
func (b *bridge) handleEventsSSE(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	ch := make(chan event, 32)
	subID := fmt.Sprintf("sub-%d", time.Now().UnixNano())
	types := parseEventFilter(r.URL.Query().Get("types"))
	b.mu.Lock()
	b.subscribers[subID] = &subscriber{ch: ch, types: types}
	b.mu.Unlock()
	defer func() {
		b.mu.Lock()
		delete(b.subscribers, subID)
		b.mu.Unlock()
		close(ch)
	}()

	enc := json.NewEncoder(w)
	for {
		select {
		case <-r.Context().Done():
			return
		case ev := <-ch:
			_, _ = w.Write([]byte("event: " + ev.Type + "\n"))
			_, _ = w.Write([]byte("data: "))
			_ = enc.Encode(ev)
			_, _ = w.Write([]byte("\n"))
			flusher.Flush()
		case <-time.After(20 * time.Second):
			_, _ = w.Write([]byte(": ping\n\n"))
			flusher.Flush()
		}
	}
}

func (b *bridge) handleCapabilities(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{
		"imu":          "available",
		"headTouch":    "available",
		"screenTouch":  "available",
		"ir":           "stub",
		"nfc":          "stub",
		"ambientLight": "stub",
	})
}

func (b *bridge) handleDriverHealth(w http.ResponseWriter, r *http.Request) {
	data, err := b.requestReply(getDriverHealth, nil, 5*time.Second)
	if err != nil {
		http.Error(w, err.Error(), http.StatusGatewayTimeout)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(data)
}

func parseEventFilter(raw string) map[string]struct{} {
	out := map[string]struct{}{}
	for _, t := range strings.Split(raw, ",") {
		t = strings.TrimSpace(t)
		if t != "" {
			out[t] = struct{}{}
		}
	}
	return out
}

func mapCapabilityError(code string) int {
	switch code {
	case "bad_request":
		return http.StatusUnprocessableEntity
	case "driver_unavailable":
		return http.StatusServiceUnavailable
	default:
		return http.StatusConflict
	}
}

func parseHexColor(value string) (uint8, uint8, uint8, error) {
	v, err := normalizeHexColor(value)
	if err != nil {
		return 0, 0, 0, err
	}
	dec := func(c byte) byte {
		switch {
		case c >= '0' && c <= '9':
			return c - '0'
		case c >= 'a' && c <= 'f':
			return c - 'a' + 10
		case c >= 'A' && c <= 'F':
			return c - 'A' + 10
		}
		return 0
	}
	return dec(v[1])<<4 | dec(v[2]), dec(v[3])<<4 | dec(v[4]), dec(v[5])<<4 | dec(v[6]), nil
}
