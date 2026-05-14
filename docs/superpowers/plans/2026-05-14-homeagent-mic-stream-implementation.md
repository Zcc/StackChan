# HomeAgent 麦克风 Opus 流 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 在 HomeAgent 通道上实装麦克风采集与 Opus 实时上行，bridge 暴露 `/mic/start` `/mic/stop` `/mic/status` `/mic/ws`，agent CLI 增加 `mic-listen` 命令。

**Architecture:** 复用小智 (`xiaozhi-esp32`) 的 `esp_opus_enc_*` 与音频参数（16 kHz / 单声道 / 60 ms）。设备端用现有 WS 二进制协议 `[type(1)][len(4 BE)][payload]` 出帧；新增 `MicStatus(0x3C)` 状态事件；`MicAudio(0x39)` payload 加 16 字节定长头（stream_hash + seq + ts）。bridge 端 single-subscriber `/mic/ws` 原样转发。

**Tech Stack:** ESP-IDF v5.5.4 + `esp_audio_codec`（Opus）+ I2S；Go 1.26 + `gorilla/websocket`；Node 子命令在 `skills/stackchan-home-agent`。

**Spec:** `docs/superpowers/specs/2026-05-14-homeagent-mic-stream-design.md`

---

## File Structure

新建：
- `tools/home-agent-bridge/mic.go` —— mic 子模块：`micState`、`micSub`、`/mic/*` handler、`MicAudio` fanout、`MicStatus` 入站解析。
- `tools/home-agent-bridge/mic_test.go` —— 上述子模块的回归测试。
- `firmware/main/hal/hal_mic.h` —— `MicStreamConfig` / `MicStatusEvent` / `MicFrame` 类型与 HAL mic 接口（成员函数声明在 `HAL` 类内）。
- `firmware/main/hal/hal_mic.cpp` —— PCM 采集 + Opus 编码 + duration timer 实现。
- `skills/stackchan-home-agent/bin/mic-listen.js`（或同文件内子命令分支，按现有代码风格）—— CLI 子命令。

修改：
- `tools/home-agent-bridge/main.go`：
  - 路由：把 stub 替换为 `b.handleMicStart/Stop/Status` + 注册 `/mic/ws`。
  - `readLoop` 的 `MicAudio(0x39)` 分支：替换 "占位 bytes" 逻辑，调用新增 `b.dispatchMicAudio(payload)`。
  - 新增 `MicStatus(0x3C)` case。
  - `handleCapabilities` 输出 mic 描述。
- `tools/home-agent-bridge/README.md`：新增 mic 章节。
- `firmware/main/hal/hal.h`：声明 mic 信号、`StartMicStream`/`StopMicStream`/`SetMicStatsCallback` 等公有 API。
- `firmware/main/hal/hal.cpp`：在 `HAL` 构造/析构里初始化/释放 mic 子系统（forward 到 `hal_mic.cpp`）。
- `firmware/main/hal/hal_ws_avatar.cpp`：实装 `MicStreamStart/Stop` 解析；订阅 mic 信号并发 `MicAudio`/`MicStatus` 帧；`CapabilityError` 中新增 mic 错误码。
- `firmware/main/CMakeLists.txt`：把 `hal/hal_mic.cpp` 加入 SRCS。
- `skills/stackchan-home-agent/SKILL.md` 与 `skills/stackchan-home-agent/README.md`：mic-listen 用例。

---

## Task 1: bridge — `mic.go` 骨架与 `micState`

**Files:**
- Create: `tools/home-agent-bridge/mic.go`
- Test:   `tools/home-agent-bridge/mic_test.go`

- [ ] **Step 1: 写失败测试**

写入 `tools/home-agent-bridge/mic_test.go`：

```go
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
```

- [ ] **Step 2: 运行测试确认失败**

```bash
cd tools/home-agent-bridge && go test ./...
```
预期：`undefined: newMicState` 等编译错。

- [ ] **Step 3: 写最小实现**

新建 `tools/home-agent-bridge/mic.go`：

```go
package main

import (
	"sync"
	"time"
)

type micStatusSnapshot struct {
	Active     bool   `json:"active"`
	StreamID   string `json:"stream_id,omitempty"`
	StartedAt  int64  `json:"started_at,omitempty"` // unix millis
	DurationMs int    `json:"duration_ms,omitempty"`
	Frames     uint64 `json:"frames,omitempty"`
	Bytes      uint64 `json:"bytes,omitempty"`
}

type micState struct {
	mu         sync.Mutex
	pending    string // streamID assigned by /mic/start but not yet confirmed by device
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
```

- [ ] **Step 4: 运行测试通过**

```bash
cd tools/home-agent-bridge && go test ./... -run TestMicStateLifecycle -v
```
预期：PASS。

- [ ] **Step 5: 提交**

```bash
git add tools/home-agent-bridge/mic.go tools/home-agent-bridge/mic_test.go
git commit -m "bridge(mic): add micState lifecycle scaffolding"
```

---

## Task 2: bridge — `POST /mic/start` 冲突与时长上限

**Files:**
- Modify: `tools/home-agent-bridge/mic.go`
- Modify: `tools/home-agent-bridge/main.go:153`
- Test:   `tools/home-agent-bridge/mic_test.go`

- [ ] **Step 1: 测试**

追加到 `mic_test.go`：

```go
func TestHandleMicStartCapsDuration(t *testing.T) {
	b := newTestBridge() // helper that constructs bridge with no real WS conn
	res := b.startMicForTest(t, `{"duration_ms": 999999}`)
	if res.HTTPStatus != 202 {
		t.Fatalf("want 202, got %d body=%s", res.HTTPStatus, res.Body)
	}
	if res.RequestedDuration != 300000 {
		t.Fatalf("duration not capped: got %d", res.RequestedDuration)
	}
}

func TestHandleMicStartConflict(t *testing.T) {
	b := newTestBridge()
	if r := b.startMicForTest(t, `{}`); r.HTTPStatus != 202 {
		t.Fatalf("first start want 202, got %d", r.HTTPStatus)
	}
	r2 := b.startMicForTest(t, `{}`)
	if r2.HTTPStatus != 409 {
		t.Fatalf("second start want 409, got %d body=%s", r2.HTTPStatus, r2.Body)
	}
}
```

并在 `mic_test.go` 顶部加 helper（newTestBridge / startMicForTest 见 Step 3）。

- [ ] **Step 2: 运行测试确认失败**

```bash
cd tools/home-agent-bridge && go test ./... -run TestHandleMicStart -v
```
预期：FAIL（函数不存在）。

- [ ] **Step 3: 实现**

在 `mic.go` 追加：

```go
import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"crypto/rand"
	"encoding/hex"
)

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
	var req micStartReq
	if r.ContentLength > 0 {
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
	if err := b.sendBinary(micStreamStart, payload); err != nil {
		b.mic.markStopped(streamID, "error", 0, 0)
		http.Error(w, "device send failed: "+err.Error(), http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(micStartResp{StreamID: streamID, DurationMs: dur})
}

// --- test helpers ---

type micStartTestResult struct {
	HTTPStatus        int
	Body              string
	RequestedDuration int
}

func newTestBridge() *bridge {
	return &bridge{mic: newMicState(), outbound: make(chan []byte, 8)}
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
```

并在 `main.go` 中：
1. 替换 `/mic/start` 路由的 stub：
   ```go
   http.HandleFunc("/mic/start", b.withAuth(b.handleMicStart))
   ```
2. 在 `type bridge struct {` 内添加字段 `mic *micState` 与 `outbound chan []byte`（如不存在）。
3. 在 `func main()` 构造 bridge 时 `b.mic = newMicState()`。
4. 引入 `sendBinary(typeByte byte, payload []byte) error`：如果不存在，添加最小实现（将 `[1+4+len]` 帧写入 `b.outbound` 或现有 WS 写入函数；命名以现仓库实际函数为准——若已有写帧函数（如 `b.writeBinary` / `b.sendToDevice`），直接复用并去掉这一辅助）。

> 注意：在仓库中 grep `sendBinary|writeBinary|writeToDevice` 找现有发送函数；如已有则不要新建，仅在 mic.go 中调用现成的。

- [ ] **Step 4: 运行测试通过**

```bash
cd tools/home-agent-bridge && go test ./... -run TestHandleMicStart -v
```

- [ ] **Step 5: 提交**

```bash
git add tools/home-agent-bridge/mic.go tools/home-agent-bridge/mic_test.go tools/home-agent-bridge/main.go
git commit -m "bridge(mic): implement POST /mic/start with conflict and duration cap"
```

---

## Task 3: bridge — `POST /mic/stop` 与 `GET /mic/status`

**Files:**
- Modify: `tools/home-agent-bridge/mic.go`
- Modify: `tools/home-agent-bridge/main.go:154`
- Test:   `tools/home-agent-bridge/mic_test.go`

- [ ] **Step 1: 测试**

```go
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
		if w.Code != 200 || !strings.Contains(w.Body.String(), `"active":false`) {
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
```

- [ ] **Step 2: 失败**

```bash
cd tools/home-agent-bridge && go test ./... -run TestHandleMicStop -v
cd tools/home-agent-bridge && go test ./... -run TestHandleMicStatus -v
```

- [ ] **Step 3: 实现**

```go
func (b *bridge) handleMicStop(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	streamID := b.mic.currentStreamID()
	if streamID == "" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	payload, _ := json.Marshal(map[string]string{"stream_id": streamID})
	_ = b.sendBinary(micStreamStop, payload) // best-effort; status will reflect via device event
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"stream_id": streamID, "stopping": "true"})
}

func (b *bridge) handleMicStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(b.mic.snapshot())
}
```

`main.go` 路由：

```go
http.HandleFunc("/mic/stop",   b.withAuth(b.handleMicStop))
http.HandleFunc("/mic/status", b.withAuth(b.handleMicStatus))
```

- [ ] **Step 4: 测试通过**

```bash
cd tools/home-agent-bridge && go test ./... -v
```

- [ ] **Step 5: 提交**

```bash
git add tools/home-agent-bridge/mic.go tools/home-agent-bridge/mic_test.go tools/home-agent-bridge/main.go
git commit -m "bridge(mic): implement /mic/stop and /mic/status"
```

---

## Task 4: bridge — 入站 `MicStatus(0x3C)` 分发

**Files:**
- Modify: `tools/home-agent-bridge/mic.go`
- Modify: `tools/home-agent-bridge/main.go:235`（`readLoop` switch）
- Test:   `tools/home-agent-bridge/mic_test.go`

- [ ] **Step 1: 测试**

```go
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
```

- [ ] **Step 2: 失败**

```bash
go test ./... -run TestDispatchMicStatus -v
```

- [ ] **Step 3: 实现**

`mic.go` 追加：

```go
type micStatusEvt struct {
	Event           string `json:"event"`
	StreamID        string `json:"stream_id"`
	Reason          string `json:"reason,omitempty"`
	Frames          uint64 `json:"frames,omitempty"`
	Bytes           uint64 `json:"bytes,omitempty"`
}

func (b *bridge) dispatchMicStatus(payload []byte) {
	var e micStatusEvt
	if err := json.Unmarshal(payload, &e); err != nil {
		return
	}
	switch e.Event {
	case "started":
		b.mic.markStarted(e.StreamID, time.Now())
	case "stopped":
		b.mic.markStopped(e.StreamID, e.Reason, e.Frames, e.Bytes)
	}
	b.publishEvent("mic."+e.Event, payload)
}
```

`main.go` 在 `readLoop` 的 type switch 中加入：

```go
case micStatus:
	b.dispatchMicStatus(payload)
```

并在 type 常量区添加：

```go
const (
	// ... existing
	micStreamStart byte = 0x37
	micStreamStop  byte = 0x38
	micAudio       byte = 0x39
	micStatus      byte = 0x3C
)
```

（若上述常量已经声明，跳过这一步。）

- [ ] **Step 4: 测试通过**

```bash
cd tools/home-agent-bridge && go test ./... -v
```

- [ ] **Step 5: 提交**

```bash
git add tools/home-agent-bridge/mic.go tools/home-agent-bridge/mic_test.go tools/home-agent-bridge/main.go
git commit -m "bridge(mic): dispatch MicStatus(0x3C) into mic state and SSE"
```

---

## Task 5: bridge — `/mic/ws` 单订阅者与 `MicAudio` 扇出

**Files:**
- Modify: `tools/home-agent-bridge/mic.go`
- Modify: `tools/home-agent-bridge/main.go:148-156` & `:265-290`（路由 + MicAudio 分支）
- Test:   `tools/home-agent-bridge/mic_test.go`

- [ ] **Step 1: 测试（含 single subscriber、扇出、背压）**

```go
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
		t.Fatalf("want 409, got %v", resp)
	}
}

func TestMicAudioFanout(t *testing.T) {
	b := newTestBridge()
	srv := httptest.NewServer(http.HandlerFunc(b.handleMicWS))
	defer srv.Close()
	c, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil { t.Fatal(err) }
	defer c.Close()

	frame := make([]byte, 16+8)
	for i := range frame { frame[i] = byte(i) }
	b.dispatchMicAudio(frame)

	c.SetReadDeadline(time.Now().Add(time.Second))
	mt, data, err := c.ReadMessage()
	if err != nil { t.Fatalf("read: %v", err) }
	if mt != websocket.BinaryMessage { t.Fatalf("expect binary, got %d", mt) }
	if !bytes.Equal(data, frame) { t.Fatalf("payload mismatch") }
}

func TestMicAudioBackpressureDropsSubscriber(t *testing.T) {
	b := newTestBridge()
	srv := httptest.NewServer(http.HandlerFunc(b.handleMicWS))
	defer srv.Close()
	c, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil { t.Fatal(err) }
	defer c.Close()
	// do not read; flood
	for i := 0; i < 256; i++ {
		b.dispatchMicAudio(make([]byte, 64))
	}
	time.Sleep(50 * time.Millisecond)
	if b.micSubscriberCount() != 0 {
		t.Fatalf("backpressured subscriber should have been dropped")
	}
}
```

记得加 imports：`bytes`, `github.com/gorilla/websocket`。

- [ ] **Step 2: 失败**

```bash
cd tools/home-agent-bridge && go test ./... -run TestMicWS -v
```

- [ ] **Step 3: 实现**

`mic.go` 追加：

```go
import "github.com/gorilla/websocket"

type micSub struct {
	conn *websocket.Conn
	send chan []byte
	done chan struct{}
}

var micUpgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

func (b *bridge) handleMicWS(w http.ResponseWriter, r *http.Request) {
	b.micSubMu.Lock()
	if b.micSub != nil {
		b.micSubMu.Unlock()
		http.Error(w, `{"code":"mic.subscriber.exists"}`, http.StatusConflict)
		return
	}
	conn, err := micUpgrader.Upgrade(w, r, nil)
	if err != nil { b.micSubMu.Unlock(); return }
	sub := &micSub{conn: conn, send: make(chan []byte, 64), done: make(chan struct{})}
	b.micSub = sub
	b.micSubMu.Unlock()

	defer func() {
		b.micSubMu.Lock()
		if b.micSub == sub { b.micSub = nil }
		b.micSubMu.Unlock()
		close(sub.done)
		conn.Close()
	}()

	go func() { // drain inbound (we ignore but need to read for ping)
		for {
			if _, _, err := conn.NextReader(); err != nil { return }
		}
	}()

	for msg := range sub.send {
		_ = conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
		if err := conn.WriteMessage(websocket.BinaryMessage, msg); err != nil { return }
	}
}

func (b *bridge) dispatchMicAudio(payload []byte) {
	b.mic.addFrame(len(payload))
	b.micSubMu.Lock()
	sub := b.micSub
	b.micSubMu.Unlock()
	if sub == nil { return }
	select {
	case sub.send <- payload:
	default:
		// backpressure: drop subscriber
		b.micSubMu.Lock()
		if b.micSub == sub {
			close(sub.send)
			b.micSub = nil
		}
		b.micSubMu.Unlock()
	}
}

func (b *bridge) micSubscriberCount() int {
	b.micSubMu.Lock()
	defer b.micSubMu.Unlock()
	if b.micSub == nil { return 0 }
	return 1
}
```

`bridge` struct 增加字段：

```go
micSubMu sync.Mutex
micSub   *micSub
```

`main.go`：
- 路由：`http.HandleFunc("/mic/ws", b.withAuth(b.handleMicWS))`。
- `readLoop` 的 `MicAudio(0x39)` 分支替换为 `b.dispatchMicAudio(payload)`。

- [ ] **Step 4: 测试通过**

```bash
cd tools/home-agent-bridge && go test ./... -v
```

- [ ] **Step 5: 提交**

```bash
git add tools/home-agent-bridge/mic.go tools/home-agent-bridge/mic_test.go tools/home-agent-bridge/main.go
git commit -m "bridge(mic): single-subscriber /mic/ws with backpressure"
```

---

## Task 6: bridge — `CapabilityError` mic 映射与 `/capabilities`

**Files:**
- Modify: `tools/home-agent-bridge/main.go`（`mapCapabilityError`、`handleCapabilities`）
- Modify: `tools/home-agent-bridge/main_test.go`

- [ ] **Step 1: 测试**

在 `main_test.go` 追加：

```go
func TestMapMicCapabilityError(t *testing.T) {
	cases := map[string]int{
		"busy": 409, "unavailable": 503, "invalid_args": 400, "unknown": 502,
	}
	for code, want := range cases {
		got := mapCapabilityError("mic", code)
		if got != want { t.Fatalf("mic/%s want %d got %d", code, want, got) }
	}
}

func TestCapabilitiesIncludesMic(t *testing.T) {
	b := newTestBridge()
	req := httptest.NewRequest(http.MethodGet, "/capabilities", nil)
	w := httptest.NewRecorder()
	b.handleCapabilities(w, req)
	if !strings.Contains(w.Body.String(), `"mic"`) ||
		!strings.Contains(w.Body.String(), `"frame_duration_ms":60`) {
		t.Fatalf("mic capability missing: %s", w.Body.String())
	}
}
```

- [ ] **Step 2: 失败**

```bash
cd tools/home-agent-bridge && go test ./... -run "TestMapMic|TestCapabilitiesIncludesMic" -v
```

- [ ] **Step 3: 实现**

在 `mapCapabilityError` 中确保 `mic` capability 时映射如下（多数已通用，确认无误即可）：

```go
switch code {
case "busy":         return http.StatusConflict
case "unavailable":  return http.StatusServiceUnavailable
case "invalid_args": return http.StatusBadRequest
default:             return http.StatusBadGateway
}
```

在 `handleCapabilities` 输出 map 中添加：

```go
"mic": map[string]any{
	"format":            "opus",
	"sample_rate":       micSampleRate,
	"channels":          1,
	"frame_duration_ms": micFrameDurationMs,
	"max_duration_ms":   micMaxDurationMs,
},
```

- [ ] **Step 4: 通过**

```bash
cd tools/home-agent-bridge && go test ./... -v
```

- [ ] **Step 5: 提交**

```bash
git add tools/home-agent-bridge/main.go tools/home-agent-bridge/main_test.go
git commit -m "bridge(mic): expose capability map and error mapping"
```

---

## Task 7: firmware — `hal_mic.h` 接口

**Files:**
- Create: `firmware/main/hal/hal_mic.h`
- Modify: `firmware/main/hal/hal.h`
- Modify: `firmware/main/CMakeLists.txt`

> 固件部分没有现成的 host 单测框架，按"build 即测"。每个 firmware 任务以 `idf.py build` 通过作为完成判据。

- [ ] **Step 1: 创建头文件**

`firmware/main/hal/hal_mic.h`：

```cpp
#pragma once
#include <cstdint>
#include <functional>
#include <string>
#include <vector>

namespace stackchan::hal {

struct MicStreamConfig {
    uint32_t sample_rate       = 16000;
    uint8_t  channels          = 1;
    uint16_t frame_duration_ms = 60;
    uint32_t duration_ms       = 30000;
    std::string stream_id;
};

struct MicFrame {
    uint32_t stream_hash;
    uint32_t seq;
    uint64_t timestamp_ms;
    std::vector<uint8_t> opus_payload;
};

struct MicStatusEvent {
    enum class Kind { Started, Stopped, Stats };
    Kind   kind;
    std::string stream_id;
    std::string reason;       // for Stopped
    uint64_t frames = 0;
    uint64_t bytes  = 0;
    MicStreamConfig started_cfg{}; // valid for Started
};

class MicSubsystem {
public:
    using FrameCallback  = std::function<void(const MicFrame&)>;
    using StatusCallback = std::function<void(const MicStatusEvent&)>;

    MicSubsystem();
    ~MicSubsystem();

    // Returns false and fills `err` if can't start.
    bool Start(const MicStreamConfig& cfg, std::string* err);
    void Stop(const std::string& reason);
    bool IsActive() const;

    void SetFrameCallback(FrameCallback cb);
    void SetStatusCallback(StatusCallback cb);

private:
    struct Impl;
    Impl* impl_;
};

}  // namespace stackchan::hal
```

`hal.h` 在 `class HAL` 内添加（按既有命名风格）：

```cpp
// in public:
stackchan::hal::MicSubsystem& Mic();

// in private:
stackchan::hal::MicSubsystem mic_;
```

`CMakeLists.txt`（main 组件）：把 `"hal/hal_mic.cpp"` 加进 SRCS。

- [ ] **Step 2: 编译**

```bash
source ~/esp/esp-idf-v5.5.4/export.sh
cd firmware && idf.py build
```
此时会因 `hal_mic.cpp` 未实现而链接失败 —— OK，下一任务实现。

- [ ] **Step 3: 提交（暂不编译通过，task 8 后一并提交）**

跳过提交，保留 worktree dirty。

---

## Task 8: firmware — `hal_mic.cpp` 实现

**Files:**
- Create: `firmware/main/hal/hal_mic.cpp`

参考实现（关键骨架；详细 I2S/编码器调用参考 `firmware/xiaozhi-esp32/main/audio/audio_service.cc:75-440`）：

- [ ] **Step 1: 实现**

```cpp
#include "hal_mic.h"
#include "hal.h"
#include "esp_log.h"
#include "esp_audio_enc.h"
#include "esp_audio_types.h"
#include "esp_opus_enc.h"
#include "driver/i2s_std.h"
#include "freertos/FreeRTOS.h"
#include "freertos/task.h"
#include <atomic>
#include <chrono>
#include <cstring>
#include <mutex>

namespace stackchan::hal {

static constexpr const char* TAG = "hal_mic";

struct MicSubsystem::Impl {
    std::mutex mu;
    std::atomic<bool> running{false};
    MicStreamConfig cfg{};
    void* opus_enc = nullptr;
    int   enc_frame_samples = 0;
    int   enc_outbuf_size   = 0;
    TaskHandle_t task = nullptr;
    uint32_t seq = 0;
    uint64_t frames = 0;
    uint64_t bytes = 0;
    uint64_t deadline_ms = 0;
    FrameCallback frame_cb;
    StatusCallback status_cb;

    static uint32_t fnv32(const std::string& s) {
        uint32_t h = 2166136261u;
        for (char c : s) { h ^= (uint8_t)c; h *= 16777619u; }
        return h;
    }

    static void task_trampoline(void* arg) {
        static_cast<Impl*>(arg)->run();
        vTaskDelete(nullptr);
    }

    void run() {
        const size_t pcm_samples = cfg.sample_rate / 1000 * cfg.frame_duration_ms;
        std::vector<int16_t> pcm(pcm_samples);
        std::vector<uint8_t> out(enc_outbuf_size);
        size_t read_bytes = 0;
        // I2S handle: reuse board audio codec input via GetHAL().AudioCodec()->ReadPcm(...)
        // The exact API name depends on board impl. Use audio_service.cc:Read() pattern.
        auto& hal = ::GetHAL();
        auto* codec = hal.AudioCodec();   // assumed accessor; if missing, fetch via board
        uint32_t start_tick = xTaskGetTickCount();
        while (running.load()) {
            size_t want = pcm.size() * sizeof(int16_t);
            if (!codec->ReadInputPcm((uint8_t*)pcm.data(), want)) {
                ESP_LOGW(TAG, "PCM read failed");
                stopInternal("error");
                return;
            }
            esp_audio_enc_in_frame_t in{ .buffer=(uint8_t*)pcm.data(), .len=(uint32_t)want };
            esp_audio_enc_out_frame_t outf{ .buffer=out.data(), .len=(uint32_t)out.size(), .encoded_bytes=0 };
            if (esp_opus_enc_process(opus_enc, &in, &outf) != ESP_AUDIO_ERR_OK) {
                ESP_LOGW(TAG, "opus encode failed");
                continue;
            }
            uint64_t now_ms = (uint64_t)xTaskGetTickCount() * portTICK_PERIOD_MS;
            MicFrame f;
            f.stream_hash = fnv32(cfg.stream_id);
            f.seq         = seq++;
            f.timestamp_ms = now_ms;
            f.opus_payload.assign(out.data(), out.data() + outf.encoded_bytes);
            { std::lock_guard<std::mutex> _l(mu);
              frames++; bytes += outf.encoded_bytes; }
            if (frame_cb) frame_cb(f);
            if (deadline_ms && now_ms >= deadline_ms) {
                stopInternal("timeout");
                return;
            }
        }
    }

    void stopInternal(const std::string& reason) {
        if (!running.exchange(false)) return;
        if (opus_enc) { esp_opus_enc_close(opus_enc); opus_enc = nullptr; }
        if (status_cb) {
            MicStatusEvent ev; ev.kind = MicStatusEvent::Kind::Stopped;
            ev.stream_id = cfg.stream_id; ev.reason = reason;
            ev.frames = frames; ev.bytes = bytes;
            status_cb(ev);
        }
    }
};

MicSubsystem::MicSubsystem() : impl_(new Impl()) {}
MicSubsystem::~MicSubsystem() { Stop("shutdown"); delete impl_; }

bool MicSubsystem::IsActive() const { return impl_->running.load(); }
void MicSubsystem::SetFrameCallback(FrameCallback cb)   { impl_->frame_cb = std::move(cb); }
void MicSubsystem::SetStatusCallback(StatusCallback cb) { impl_->status_cb = std::move(cb); }

bool MicSubsystem::Start(const MicStreamConfig& cfg, std::string* err) {
    if (impl_->running.load()) { if (err) *err = "busy"; return false; }
    impl_->cfg = cfg;
    impl_->seq = 0;
    impl_->frames = 0;
    impl_->bytes = 0;
    impl_->deadline_ms = cfg.duration_ms ?
        (uint64_t)xTaskGetTickCount() * portTICK_PERIOD_MS + cfg.duration_ms : 0;

    esp_opus_enc_config_t ec = ESP_OPUS_ENC_CONFIG_DEFAULT();
    ec.sample_rate    = cfg.sample_rate;
    ec.channel        = ESP_AUDIO_MONO;
    ec.bitrate        = 24000;
    ec.frame_duration = (esp_opus_enc_frame_duration_t)cfg.frame_duration_ms;
    if (esp_opus_enc_open(&ec, sizeof(ec), &impl_->opus_enc) != ESP_AUDIO_ERR_OK || !impl_->opus_enc) {
        if (err) *err = "unavailable";
        return false;
    }
    int frame_size = 0;
    esp_opus_enc_get_frame_size(impl_->opus_enc, &frame_size, &impl_->enc_outbuf_size);
    impl_->enc_frame_samples = frame_size / (int)sizeof(int16_t);

    impl_->running.store(true);
    if (impl_->status_cb) {
        MicStatusEvent ev; ev.kind = MicStatusEvent::Kind::Started;
        ev.stream_id = cfg.stream_id; ev.started_cfg = cfg;
        impl_->status_cb(ev);
    }
    xTaskCreatePinnedToCore(&Impl::task_trampoline, "halmic", 6144, impl_, 5, &impl_->task, 1);
    return true;
}

void MicSubsystem::Stop(const std::string& reason) {
    impl_->stopInternal(reason.empty() ? std::string("user") : reason);
}

}  // namespace stackchan::hal
```

> 关键不确定点：`codec->ReadInputPcm(...)` 与 `GetHAL().AudioCodec()` 的真实接口名。若现有 HAL 未暴露 PCM 读取，需要在 `hal_bridge.cc` / `stackchan.cc` 上加一层 `ReadInputPcm`，将 xiaozhi 的 I2S 读取（`audio_service.cc` 中通过 `codec->InputData(...)`）镜像过来。**实现者应先在仓库内 grep `InputPcm|input_sample|i2s_channel_read` 找到现成读取路径再下笔。**

- [ ] **Step 2: 构建**

```bash
source ~/esp/esp-idf-v5.5.4/export.sh
cd firmware && idf.py build 2>&1 | tail -40
```
预期通过。若链接失败提示 `ReadInputPcm` 未定义，按上述说明在板级 audio codec 类里加一层薄包装，并把该函数的实现接入既有 I2S 读取（同 `audio_service.cc`）。

- [ ] **Step 3: 提交（连同 Task 7）**

```bash
git add firmware/main/hal/hal_mic.h firmware/main/hal/hal_mic.cpp \
        firmware/main/hal/hal.h firmware/main/CMakeLists.txt
git commit -m "firmware(hal): add MicSubsystem with opus encoder"
```

---

## Task 9: firmware — `hal_ws_avatar.cpp` 接入 mic

**Files:**
- Modify: `firmware/main/hal/hal_ws_avatar.cpp`

- [ ] **Step 1: 改动**

1. 在 `DataType` enum 内更新注释；添加 `MicStatus = 0x3C`（其余已有）。
2. 在 `init()` 里订阅 `GetHAL().Mic()`：

```cpp
GetHAL().Mic().SetFrameCallback([this](const stackchan::hal::MicFrame& f) {
    std::vector<uint8_t> buf; buf.reserve(16 + f.opus_payload.size());
    auto put32 = [&](uint32_t v){ for(int i=3;i>=0;i--) buf.push_back((v>>(i*8))&0xff); };
    auto put64 = [&](uint64_t v){ for(int i=7;i>=0;i--) buf.push_back((v>>(i*8))&0xff); };
    put32(f.stream_hash); put32(f.seq); put64(f.timestamp_ms);
    buf.insert(buf.end(), f.opus_payload.begin(), f.opus_payload.end());
    sendBinary(DataType::MicAudio, buf.data(), buf.size());
});
GetHAL().Mic().SetStatusCallback([this](const stackchan::hal::MicStatusEvent& ev) {
    cJSON* root = cJSON_CreateObject();
    cJSON_AddStringToObject(root, "event",
        ev.kind == stackchan::hal::MicStatusEvent::Kind::Started ? "started" :
        ev.kind == stackchan::hal::MicStatusEvent::Kind::Stopped ? "stopped" : "stats");
    cJSON_AddStringToObject(root, "stream_id", ev.stream_id.c_str());
    if (ev.kind == stackchan::hal::MicStatusEvent::Kind::Started) {
        cJSON_AddNumberToObject(root, "sample_rate", ev.started_cfg.sample_rate);
        cJSON_AddNumberToObject(root, "channels", ev.started_cfg.channels);
        cJSON_AddNumberToObject(root, "frame_duration_ms", ev.started_cfg.frame_duration_ms);
        cJSON_AddNumberToObject(root, "duration_ms", ev.started_cfg.duration_ms);
    } else if (ev.kind == stackchan::hal::MicStatusEvent::Kind::Stopped) {
        cJSON_AddStringToObject(root, "reason", ev.reason.c_str());
        cJSON_AddNumberToObject(root, "frames", (double)ev.frames);
        cJSON_AddNumberToObject(root, "bytes",  (double)ev.bytes);
    }
    char* s = cJSON_PrintUnformatted(root);
    sendBinary(DataType::MicStatus, (uint8_t*)s, strlen(s));
    cJSON_free(s); cJSON_Delete(root);
});
```

3. 在 `handleMessage` switch 内：

```cpp
case DataType::MicStreamStart: {
    cJSON* root = cJSON_ParseWithLength((const char*)msg.payload.data(), msg.payload.size());
    if (!root) { sendCapabilityError("mic", "invalid_args", "bad json"); break; }
    stackchan::hal::MicStreamConfig cfg;
    auto* sr = cJSON_GetObjectItem(root, "sample_rate");       if (cJSON_IsNumber(sr)) cfg.sample_rate = sr->valueint;
    auto* ch = cJSON_GetObjectItem(root, "channels");           if (cJSON_IsNumber(ch)) cfg.channels = ch->valueint;
    auto* fd = cJSON_GetObjectItem(root, "frame_duration_ms");  if (cJSON_IsNumber(fd)) cfg.frame_duration_ms = fd->valueint;
    auto* du = cJSON_GetObjectItem(root, "duration_ms");        if (cJSON_IsNumber(du)) cfg.duration_ms = du->valueint;
    auto* id = cJSON_GetObjectItem(root, "stream_id");          if (cJSON_IsString(id)) cfg.stream_id = id->valuestring;
    cJSON_Delete(root);
    std::string err;
    if (!GetHAL().Mic().Start(cfg, &err)) sendCapabilityError("mic", err, "start failed");
    break;
}
case DataType::MicStreamStop: {
    GetHAL().Mic().Stop("user");
    break;
}
```

4. 移除/替换已有的 mic stub 分支（如有）。

- [ ] **Step 2: 构建**

```bash
source ~/esp/esp-idf-v5.5.4/export.sh
cd firmware && idf.py build 2>&1 | tail -30
```
预期通过；记录分区余量（`Built target app: ...` 后的 `*.bin` 大小 vs 分区大小）。若分区溢出，按 spec "风险与回退" 加 Kconfig `CONFIG_HOMEAGENT_MIC` 默认 off 并以 `#ifdef` 包裹本任务的所有 mic 代码。

- [ ] **Step 3: 提交**

```bash
git add firmware/main/hal/hal_ws_avatar.cpp
git commit -m "firmware(ws): wire MicStreamStart/Stop and MicAudio/MicStatus frames"
```

---

## Task 10: agent CLI — `mic-listen` 子命令

**Files:**
- Modify: `skills/stackchan-home-agent/bin/stackchan-home-agent.js`
- Modify: `skills/stackchan-home-agent/SKILL.md`
- Modify: `skills/stackchan-home-agent/README.md`
- Create: `skills/stackchan-home-agent/test/mic-listen.test.mjs`

- [ ] **Step 1: 测试（node, mock bridge）**

```js
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { createServer } from 'node:http';
import { WebSocketServer } from 'ws';
import { runMicListen } from '../bin/stackchan-home-agent.js';

test('mic-listen writes payload to file and stops on session end', async () => {
  let started = false, stopped = false;
  const http = createServer((req, res) => {
    if (req.url === '/mic/start' && req.method === 'POST') {
      started = true; res.setHeader('content-type','application/json');
      res.end(JSON.stringify({ stream_id: 'x', duration_ms: 1000 }));
    } else if (req.url === '/mic/stop' && req.method === 'POST') {
      stopped = true; res.end('{}');
    } else { res.statusCode = 404; res.end(); }
  });
  const wss = new WebSocketServer({ noServer: true });
  http.on('upgrade', (req, sock, head) => {
    if (req.url !== '/mic/ws') { sock.destroy(); return; }
    wss.handleUpgrade(req, sock, head, ws => {
      ws.send(Buffer.from([1,2,3,4,5,6,7,8, 0,0,0,0, 0,0,0,0, 0xaa, 0xbb])); // 16 hdr + 2 opus
      setTimeout(() => ws.close(), 50);
    });
  });
  await new Promise(r => http.listen(0, r));
  const port = http.address().port;
  const tmp = `/tmp/mic-${Date.now()}.opus`;
  const code = await runMicListen({ bridge: `http://127.0.0.1:${port}`, token: 't', out: tmp, durationMs: 1000 });
  assert.equal(started, true);
  assert.equal(stopped, true);
  assert.equal(code, 0);
  const fs = await import('node:fs/promises');
  const buf = await fs.readFile(tmp);
  assert.deepEqual([...buf], [0xaa, 0xbb]); // header stripped
  http.close();
});
```

- [ ] **Step 2: 失败**

```bash
cd skills/stackchan-home-agent && node --test test/mic-listen.test.mjs
```

- [ ] **Step 3: 实现**

在 `bin/stackchan-home-agent.js` 中追加导出（保持现有 ESM 风格）：

```js
import WebSocket from 'ws';
import fs from 'node:fs';

export async function runMicListen({ bridge, token, out, durationMs }) {
  const headers = token ? { Authorization: `Bearer ${token}` } : {};
  const startRes = await fetch(`${bridge}/mic/start`, {
    method: 'POST', headers: { ...headers, 'Content-Type': 'application/json' },
    body: JSON.stringify({ duration_ms: durationMs ?? 30000 }),
  });
  if (!startRes.ok) return 2;
  const { stream_id } = await startRes.json();

  const wsUrl = bridge.replace(/^http/, 'ws') + '/mic/ws';
  const ws = new WebSocket(wsUrl, { headers });
  const file = out ? fs.createWriteStream(out) : null;
  await new Promise((resolve, reject) => {
    ws.on('message', (data, isBinary) => {
      if (!isBinary || data.length < 16) return;
      const opus = data.slice(16);
      if (file) file.write(opus);
    });
    ws.on('close', resolve);
    ws.on('error', reject);
    process.on('SIGINT', () => ws.close());
  }).catch(() => {});
  if (file) await new Promise(r => file.end(r));

  await fetch(`${bridge}/mic/stop`, { method: 'POST', headers });
  return 0;
}
```

并把 `mic-listen` 注册到 CLI 主 dispatcher（与 `say` / `look` 等同样的方式）。

- [ ] **Step 4: 通过**

```bash
cd skills/stackchan-home-agent && node --test test/mic-listen.test.mjs
```

- [ ] **Step 5: 文档 + 提交**

更新 SKILL.md / README.md：

```
### mic-listen
拉取实时麦克风音频（Opus 16kHz mono 60ms 帧）。
用法：
  stackchan-home-agent mic-listen --out captured.opus --duration-ms 10000
随后可用 opusdec 解码：
  opusdec --rate 16000 captured.opus captured.wav
```

```bash
git add skills/stackchan-home-agent
git commit -m "skill(home-agent): add mic-listen command and tests"
```

---

## Task 11: 全量校验 + 文档

- [ ] **Step 1: bridge 全测**

```bash
cd tools/home-agent-bridge && go test ./... && go vet ./... && go build ./...
```

- [ ] **Step 2: relay 不回归**

```bash
cd tools/home-agent-relay && go vet ./... && go build ./...
```

- [ ] **Step 3: firmware build**

```bash
source ~/esp/esp-idf-v5.5.4/export.sh
cd firmware && idf.py build 2>&1 | tail -20
```
观察 `app` 分区余量；若 < 1% 在 commit message 中写明并加 Kconfig 开关。

- [ ] **Step 4: 更新 bridge README**

`tools/home-agent-bridge/README.md` 新增章节：

```
### Microphone (Opus stream)

- POST /mic/start    body {duration_ms?: <=300000}, returns {stream_id, duration_ms}, 409 on busy
- POST /mic/stop     returns {stream_id, stopping:true}, 204 if idle
- GET  /mic/status   returns {active, stream_id?, started_at?, duration_ms?, frames, bytes}
- GET  /mic/ws       WebSocket, single subscriber, binary frames [16-byte header + Opus payload];
                     text frames carry {"type":"mic.started|stopped|stats", ...}; 409 if already subscribed
- SSE event types: mic.started, mic.stopped, mic.stats (filter via /events?types=mic.*)

Header layout (binary):  u32 BE stream_hash | u32 BE seq | u64 BE timestamp_ms | opus_payload
```

- [ ] **Step 5: 提交收尾**

```bash
git add tools/home-agent-bridge/README.md
git commit -m "docs(bridge): document mic stream API"
```

- [ ] **Step 6: 实机 smoke（用户执行，不在 CI）**

1. 烧录新固件。
2. 启动 bridge + relay。
3. `stackchan-home-agent mic-listen --out /tmp/a.opus --duration-ms 5000`
4. `opusdec --rate 16000 /tmp/a.opus /tmp/a.wav` 应能听到声音。
5. 验证：再开第二个 mic-listen 应得到 409；不传 duration 30s 后自动停止；显式 stop 立即停止；并发 say/look 不被阻塞。

---

## Self-Review Notes

- Spec 中所有功能都有任务覆盖：协议（Task 4/5/9）、HTTP API（Task 2/3/6）、固件采集（Task 7/8/9）、CLI（Task 10）、错误处理（Task 6）、测试（每个 bridge 任务 TDD，CLI Task 10 含测试，固件以 build smoke 验证）。
- 未在固件层为 PCM 输入提供具体 API 名（codec→ReadInputPcm）—— 已在 Task 8 明确：实现者先在仓库内确认现有 I2S 读取入口，必要时增加薄包装；这是仓库依赖性 hot spot，不应在计划阶段虚构。
- 单订阅者一致性：Task 5 测试覆盖 409；handleMicWS 单次写入语义与 dispatchMicAudio 的"满即关订阅"一致。
- 错误码：`busy/unavailable/invalid_args` 在 spec 与 Task 6 一致。
- 分区风险已在 Task 9 标注回退方案（Kconfig 开关）。
