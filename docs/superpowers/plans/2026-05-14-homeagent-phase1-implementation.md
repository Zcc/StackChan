# HomeAgent Hardware Phase 1 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make HomeAgent Phase 1 production-ready by shipping reliable structured event streaming (IMU/head-touch/screen-touch), standardized capability errors, and driver health/capability introspection while preserving current APIs.

**Architecture:** Keep the current WS packet protocol and extend it in place. Firmware becomes the authoritative event producer (including touch sampling + structured payloads + typed capability errors). The Go bridge becomes a reliable fan-out hub with typed SSE events, backpressure handling, and capability/health endpoints that reflect firmware state.

**Tech Stack:** ESP-IDF C++ (ArduinoJson, FreeRTOS tasks), Go (net/http, gorilla/websocket), existing StackChan HAL / HomeAgent bridge.

---

## File Structure (planned changes)

- Modify: `firmware/main/hal/hal_ws_avatar.cpp`  
  Responsibility: firmware-side WS event producer, command handler, capability error payloads.
- Modify: `firmware/main/hal/hal.h`  
  Responsibility: add signals/data structs for screen touch + driver health snapshots.
- Modify: `firmware/main/hal/hal.cpp`  
  Responsibility: wire touch polling/task start and expose helper getters for event payload fields.
- Create: `firmware/main/hal/hal_homeagent_drivers.h`  
  Responsibility: IR/NFC/ambient adapter interface definitions + health/result structs.
- Create: `firmware/main/hal/hal_homeagent_drivers.cpp`  
  Responsibility: Null/Probe adapter implementations used in Phase 1.
- Modify: `tools/home-agent-bridge/main.go`  
  Responsibility: robust SSE hub, typed event output, capability/health endpoints, error mapping.
- Create: `tools/home-agent-bridge/main_test.go`  
  Responsibility: regression tests for SSE fan-out, capability error mapping, and endpoint behavior.
- Modify: `tools/home-agent-bridge/README.md`  
  Responsibility: document payload schemas, error codes, and new endpoints.

---

### Task 1: Firmware event model + capability error schema

**Files:**
- Modify: `firmware/main/hal/hal.h`
- Modify: `firmware/main/hal/hal_ws_avatar.cpp`
- Test: `firmware/main/hal/hal_ws_avatar.cpp` (build-only verification via `idf.py build`)

- [ ] **Step 1: Add explicit payload structs/signals in HAL header**

```cpp
// hal.h
struct ScreenTouchEvent_t {
    int x = -1;
    int y = -1;
    bool pressed = false;
    uint32_t tsMs = 0;
};

struct DriverHealth_t {
    bool ready = false;
    std::string lastError;
    uint32_t lastTickMs = 0;
};

uitk::Signal<const ScreenTouchEvent_t&> onScreenTouchEvent;
```

- [ ] **Step 2: Emit structured IMU/head-touch payloads (not string-only)**

```cpp
// hal_ws_avatar.cpp
ArduinoJson::JsonDocument doc;
doc["event"] = "shake";
doc["accel"]["x"] = accel_x;
doc["accel"]["y"] = accel_y;
doc["accel"]["z"] = accel_z;
doc["ts"] = GetHAL().millis();
std::string json;
ArduinoJson::serializeJson(doc, json);
sendPacket(DataType::ImuEvent, (const uint8_t*)json.c_str(), json.size());
```

- [ ] **Step 3: Add screen-touch WS event forwarding**

```cpp
GetHAL().onScreenTouchEvent.connect([this](const ScreenTouchEvent_t& e) {
    if (!isConnected()) return;
    ArduinoJson::JsonDocument doc;
    doc["state"] = e.pressed ? "down" : "up";
    doc["x"] = e.x;
    doc["y"] = e.y;
    doc["ts"] = e.tsMs;
    std::string json;
    ArduinoJson::serializeJson(doc, json);
    sendPacket(DataType::ScreenTouchEvent, (const uint8_t*)json.c_str(), json.size());
});
```

- [ ] **Step 4: Normalize `CapabilityError` payload shape**

```cpp
static std::string makeCapabilityError(int type, const char* capability, const char* code, const char* message) {
    ArduinoJson::JsonDocument doc;
    doc["type"] = type;
    doc["capability"] = capability;
    doc["code"] = code;
    doc["message"] = message;
    doc["details"] = ArduinoJson::JsonObject();
    std::string out;
    ArduinoJson::serializeJson(doc, out);
    return out;
}
```

- [ ] **Step 5: Build firmware and verify compile success**

Run:
```bash
cd firmware
source ~/esp/esp-idf-v5.5.4/export.sh
idf.py build
```

Expected: build completes with `Project build complete`.

- [ ] **Step 6: Commit firmware schema/event changes**

```bash
git add firmware/main/hal/hal.h firmware/main/hal/hal_ws_avatar.cpp
git commit -m "feat: structure homeagent firmware events and capability errors"
```

---

### Task 2: Firmware driver adapter framework (IR/NFC/ambient) + health snapshot

**Files:**
- Create: `firmware/main/hal/hal_homeagent_drivers.h`
- Create: `firmware/main/hal/hal_homeagent_drivers.cpp`
- Modify: `firmware/main/hal/hal_ws_avatar.cpp`
- Modify: `firmware/main/CMakeLists.txt`
- Test: `firmware` build (`idf.py build`)

- [ ] **Step 1: Define adapter interfaces and result/health types**

```cpp
// hal_homeagent_drivers.h
struct DriverResult {
    bool ok = false;
    std::string code;
    std::string message;
};

struct DriverHealth {
    bool ready = false;
    std::string name;
    std::string lastError;
    uint32_t lastTickMs = 0;
};

class HomeAgentDriverAdapter {
public:
    virtual ~HomeAgentDriverAdapter() = default;
    virtual bool Init() = 0;
    virtual DriverResult HandleCommand(std::string_view payload) = 0;
    virtual DriverHealth Health() const = 0;
};
```

- [ ] **Step 2: Implement Null/Probe adapters with deterministic errors**

```cpp
// hal_homeagent_drivers.cpp
class NullIrAdapter : public HomeAgentDriverAdapter {
public:
    bool Init() override { health_.ready = false; health_.name = "ir"; health_.lastError = "driver_unavailable"; return false; }
    DriverResult HandleCommand(std::string_view) override {
        return DriverResult{.ok = false, .code = "driver_unavailable", .message = "IR driver is not implemented on this firmware build"};
    }
    DriverHealth Health() const override { return health_; }
private:
    DriverHealth health_{};
};
```

- [ ] **Step 3: Register adapters inside WS handler and use them for IR/NFC/ambient types**

```cpp
// hal_ws_avatar.cpp (inside class WebSocketAvatar)
std::unique_ptr<HomeAgentDriverAdapter> _ir;
std::unique_ptr<HomeAgentDriverAdapter> _nfc;
std::unique_ptr<HomeAgentDriverAdapter> _ambient;
```

```cpp
// init()
_ir = createIrAdapter();
_nfc = createNfcAdapter();
_ambient = createAmbientAdapter();
if (_ir) _ir->Init();
if (_nfc) _nfc->Init();
if (_ambient) _ambient->Init();
```

- [ ] **Step 4: Expose driver health query packet handling**

```cpp
// new DataType (example)
GetDriverHealth = 0x40
```

```cpp
// case DataType::GetDriverHealth
ArduinoJson::JsonDocument doc;
appendHealth(doc["drivers"], _ir.get());
appendHealth(doc["drivers"], _nfc.get());
appendHealth(doc["drivers"], _ambient.get());
std::string out;
ArduinoJson::serializeJson(doc, out);
sendPacket(DataType::GetDriverHealth, (const uint8_t*)out.c_str(), out.size());
```

- [ ] **Step 5: Wire new source file into component build**

```cmake
# firmware/main/CMakeLists.txt
list(APPEND SOURCES "hal/hal_homeagent_drivers.cpp")
```

- [ ] **Step 6: Build firmware and verify adapter framework links**

Run:
```bash
cd firmware
source ~/esp/esp-idf-v5.5.4/export.sh
idf.py build
```

Expected: no undefined symbol errors for adapter factories/health query paths.

- [ ] **Step 7: Commit adapter framework**

```bash
git add firmware/main/hal/hal_homeagent_drivers.h firmware/main/hal/hal_homeagent_drivers.cpp firmware/main/hal/hal_ws_avatar.cpp firmware/main/CMakeLists.txt
git commit -m "feat: add homeagent driver adapter framework and health query"
```

---

### Task 3: Bridge SSE reliability + capability/driver health endpoints

**Files:**
- Modify: `tools/home-agent-bridge/main.go`
- Test: `tools/home-agent-bridge/main_test.go` (created in Task 4)

- [ ] **Step 1: Replace raw `eventSubs` fan-out with bounded subscriber queue**

```go
type subscriber struct {
    ch      chan event
    dropped uint64
    types   map[string]struct{}
}
```

```go
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
        <-sub.ch // drop oldest
        sub.ch <- ev
        sub.dropped++
    }
    }
}
```

- [ ] **Step 2: Add SSE type filter parsing (`/events?types=imu,headTouch`)**

```go
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
```

- [ ] **Step 3: Emit SSE frames with explicit event names**

```go
_, _ = w.Write([]byte("event: " + ev.Type + "\n"))
_, _ = w.Write([]byte("data: "))
_ = enc.Encode(ev)
_, _ = w.Write([]byte("\n"))
flusher.Flush()
```

- [ ] **Step 4: Map firmware capability errors to HTTP status codes**

```go
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
```

- [ ] **Step 5: Add capability and driver health endpoints**

```go
http.HandleFunc("/capabilities", b.withAuth(b.handleCapabilities))
http.HandleFunc("/health/drivers", b.withAuth(b.handleDriverHealth))
```

```go
func (b *bridge) handleCapabilities(w http.ResponseWriter, r *http.Request) {
    writeJSON(w, map[string]any{
        "imu": "available",
        "headTouch": "available",
        "screenTouch": "available",
        "ir": "stub",
        "nfc": "stub",
        "ambient": "stub",
    })
}
```

- [ ] **Step 6: Build bridge to verify compile**

Run:
```bash
cd tools/home-agent-bridge
go build ./...
go vet ./...
```

Expected: both commands exit 0.

- [ ] **Step 7: Commit bridge runtime changes**

```bash
git add tools/home-agent-bridge/main.go
git commit -m "feat: improve homeagent SSE reliability and capability health APIs"
```

---

### Task 4: Bridge regression tests + docs sync

**Files:**
- Create: `tools/home-agent-bridge/main_test.go`
- Modify: `tools/home-agent-bridge/README.md`
- Test: `tools/home-agent-bridge/main_test.go`

- [ ] **Step 1: Write failing tests for capability error mapping + event filters**

```go
func TestMapCapabilityError(t *testing.T) {
    if got := mapCapabilityError("bad_request"); got != http.StatusUnprocessableEntity {
        t.Fatalf("bad_request => %d", got)
    }
    if got := mapCapabilityError("driver_unavailable"); got != http.StatusServiceUnavailable {
        t.Fatalf("driver_unavailable => %d", got)
    }
}

func TestParseEventFilter(t *testing.T) {
    f := parseEventFilter("imu, headTouch")
    if _, ok := f["imu"]; !ok { t.Fatal("imu missing") }
    if _, ok := f["headTouch"]; !ok { t.Fatal("headTouch missing") }
}
```

- [ ] **Step 2: Add SSE publish backpressure behavior test**

```go
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
```

- [ ] **Step 3: Run tests and verify pass**

Run:
```bash
cd tools/home-agent-bridge
go test ./...
```

Expected: `ok  	stackchan-home-agent-bridge`.

- [ ] **Step 4: Document event/error/capability contracts**

```md
## Event Stream
- `GET /events?types=imu,headTouch`
- SSE frame includes `event:` name and JSON `data:`

## Capability Errors
- `code=driver_unavailable` => HTTP 503
- `code=bad_request` => HTTP 422
```

- [ ] **Step 5: Re-run build + tests as final gate**

Run:
```bash
cd tools/home-agent-bridge && go test ./... && go build ./... && go vet ./...
cd /Users/zccc/Repo/StackChan/firmware && source ~/esp/esp-idf-v5.5.4/export.sh && idf.py build
```

Expected: all commands exit 0.

- [ ] **Step 6: Commit tests and docs**

```bash
git add tools/home-agent-bridge/main_test.go tools/home-agent-bridge/README.md
git commit -m "test/docs: lock homeagent event and capability contracts"
```

---

## Spec Coverage Check

- Structured events (IMU/head-touch/screen-touch): **Task 1**
- Capability error schema standardization: **Task 1 + Task 3**
- IR/NFC/ambient driver framework + health: **Task 2 + Task 3**
- SSE reliability (fan-out/backpressure/filter): **Task 3 + Task 4**
- README contract update: **Task 4**

No spec requirement is left without a mapped task.
