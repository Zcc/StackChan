# StackChan HomeAgent Bridge

For the repeatable no-secrets-in-Git setup flow, see `../../docs/home_agent_setup.md`.


Runs on the home computer and exposes a local HTTP API for Codex, OpenClaw, Hermes, or any other local agent.

```bash
export STACKCHAN_DEVICE_ID='<same id configured in firmware>'
export STACKCHAN_RELAY_URL='wss://relay.example.com/ws'
export STACKCHAN_RELAY_TOKEN='replace-me'
export STACKCHAN_BRIDGE_TOKEN='local-agent-token' # optional
go run .
```

Local API:

```bash
curl http://127.0.0.1:8790/status
curl -X POST http://127.0.0.1:8790/say \
  -H 'Content-Type: application/json' \
  -d '{"name":"Codex","content":"我在家里的电脑上，已经接管 HomeAgent。"}'
curl -X POST http://127.0.0.1:8790/look \
  -H 'Content-Type: application/json' \
  -d '{"yaw":200,"pitch":80,"speed":500}'
curl -X POST http://127.0.0.1:8790/motion -d '{"yawServo":{"angle":200,"speed":500}}'
curl -X POST http://127.0.0.1:8790/light \
  -H 'Content-Type: application/json' \
  -d '{"color":"#0000FF","durationMs":1000}'
curl -X POST http://127.0.0.1:8790/light \
  -H 'Content-Type: application/json' \
  -d '{"color":"#000000","durationMs":500}'
curl -X POST http://127.0.0.1:8790/camera/start
curl -X POST http://127.0.0.1:8790/camera/stop
curl -o snapshot.jpg -X POST http://127.0.0.1:8790/snapshot
curl -o latest.jpg http://127.0.0.1:8790/snapshot/latest
```

If `STACKCHAN_BRIDGE_TOKEN` is set, include it on local API calls:

```bash
curl -H 'Authorization: Bearer local-agent-token' http://127.0.0.1:8790/status
```

## 完整能力列表

把 M5Stack StackChan 文档里的全部硬件能力暴露给本地 agent。每个 endpoint 都走二进制 WS 协议 `[Type(1)][Length(4 BE)][Payload]`。

### 已在固件里实现（A 类）

| HTTP | WS Type | 说明 |
|---|---|---|
| `GET /device-info` | `0x20` | 返回 `{mac, deviceName, firmware, battery, charging, brightness, volume, wifiSsid, wifiIp, wifiSignal, freeHeap, psramFree}` |
| `GET /battery` | `0x21` | `{level, charging}` |
| `POST /brightness` `{value:0-100,permanent?}` | `0x22` | 调 LCD 背光 |
| `POST /volume` `{value:0-100,permanent?}` | `0x23` | 调扬声器音量 |
| `POST /reboot` | `0x24` | 软重启 |
| `POST /factory-reset` | `0x25` | ⚠️ 出厂重置 |
| `POST /rgb` `{leds:[{i,r,g,b}\|{i,color:"#RRGGBB"}]}` | `0x26` | 12 颗 RGB 单独控制 |
| `POST /rgb/all` `{color:"#RRGGBB"}` 或 `{r,g,b}` | `0x27` | 12 颗整体显示一种颜色 |
| `GET /events` (SSE) | — | 订阅出站事件：`imu`、`headTouch`、`screenTouch`、`button`、`ir`、`nfc`、`servoFeedback`、`proximityLight`、`mic.started`、`mic.stopped`、`mic.stats`、`audio.started`、`audio.stopped`、`audio.stats` |
| `GET /capabilities` | — | 查询能力状态：`available/stub` |
| `GET /health/drivers` | `0x40` | 查询固件驱动健康状态（Phase1 为 probe/null 驱动） |

固件主动上报（出现在 `/events`）：
- `imu` `{event:"shake"\|"pickup", ts}` — 来自 `Hal::onImuMotionEvent`
- `headTouch` `{gesture:"press"\|"release"\|"swipeForward"\|"swipeBackward", ts}` — 来自 `Hal::onHeadPetGesture`
- `screenTouch` `{state:"down"\|"move"\|"up", x, y, pressed, ts}` — 来自 LVGL 触摸读取回调

`/events` 支持类型过滤：`GET /events?types=imu,headTouch`。SSE 帧带 `event:` 字段，慢消费者会丢弃最旧消息以避免全局阻塞。

### Microphone (Opus stream)

- `POST /mic/start` body `{duration_ms?: <=300000}` returns `{stream_id, duration_ms}`; returns `409` when a stream is already active.
- `POST /mic/stop` returns `{stream_id, stopping:true}`; returns `204` when idle.
- `GET /mic/status` returns `{active, stream_id?, started_at?, duration_ms?, frames, bytes}`.
- `GET /mic/ws` upgrades to a WebSocket. It allows one subscriber and sends binary frames as `[16-byte header + Opus payload]`; text frames carry `{"type":"mic.started|stopped|stats", ...}`; returns `409` if already subscribed.
- SSE event types: `mic.started`, `mic.stopped`, `mic.stats` (filter via `/events?types=mic.*`).

Header layout (binary): `u32 BE stream_hash | u32 BE seq | u64 BE timestamp_ms | opus_payload`.

### Audio Playback (Opus stream)

- `POST /audio/start` body `{duration_ms?: <=300000}` returns `{stream_id, duration_ms}`; returns `409` when a stream is already active.
- `POST /audio/feed` body = raw Opus frame bytes; returns `{ok:true}`. Feed frames after starting a stream.
- `POST /audio/stop` returns `{stream_id, stopping:true}`; returns `204` when idle.
- `GET /audio/status` returns `{active, stream_id?, started_at?, duration_ms?, frames, bytes}`.
- `POST /audio/play` — alias for `/audio/start`.
- SSE event types: `audio.started`, `audio.stopped`, `audio.stats`.

### TTS (Text-to-Speech via Edge-TTS)

- `POST /tts/speak` body `{text, voice?, duration_ms?}` returns `{stream_id, status:"streaming", voice}`.
  Connects to Microsoft Edge-TTS, receives raw 24kHz PCM, resamples to 16kHz, encodes to Opus, and streams frames to the firmware speaker in real-time.
- Default voice: `zh-CN-XiaoxiaoNeural`. Common voices: `en-US-EmmaMultilingualNeural`, `ja-JP-NanamiNeural`.
- Requires `libopus-dev` on the host (`apt install libopus-dev`).

```bash
# Speak Chinese
curl -X POST http://127.0.0.1:8790/tts/speak \
  -H 'Content-Type: application/json' \
  -d '{"text":"你好，我是 StackChan！"}'

# Speak English with a different voice
curl -X POST http://127.0.0.1:8790/tts/speak \
  -H 'Content-Type: application/json' \
  -d '{"text":"Hello world!","voice":"en-US-EmmaMultilingualNeural"}'

# Monitor audio events
curl -N "http://127.0.0.1:8790/events?types=audio.started,audio.stopped"
```

### 协议号已保留、固件 stub（B 类）

调用会立即返回 200（桥已发包），但固件目前会回 `CapabilityError (0x4F)`，payload 结构：

```json
{
  "type": 48,
  "capability": "ir.send",
  "code": "not_implemented",
  "message": "capability exists but hardware driver is not implemented",
  "ts": 123456,
  "details": {}
}
```

| HTTP | WS Type | 说明 |
|---|---|---|
| `POST /ir/send` | `0x30` | 红外发射（NEC/RC5 等） |
| `POST /ir/learn/start` | `0x31` | 进入红外学习模式 |
| `POST /nfc/read` | `0x33` | 读取 NFC 卡 |
| `POST /nfc/write` | `0x34` | 写入 NFC 卡 |
| `GET  /screen/snapshot` | `0x3A` | 抓取 LCD 帧 |
| `GET  /sd/list?path=/` | `0x3B` | microSD 列目录 |
| `GET  /sd/read?path=…` | `0x3C` | microSD 读文件 |
| `POST /sd/write` | `0x3D` | microSD 写文件 |

事件类（出站、固件待实现）：`screenTouch (0x2A)`、`button (0x2B)`、`ir (0x32)`、`nfc (0x35)`、`servoFeedback (0x3E)`、`proximityLight (0x3F)`。

### 示例

```bash
curl http://127.0.0.1:8790/device-info | jq
curl http://127.0.0.1:8790/battery | jq
curl -X POST http://127.0.0.1:8790/brightness -H 'Content-Type: application/json' -d '{"value":40}'
curl -X POST http://127.0.0.1:8790/volume     -H 'Content-Type: application/json' -d '{"value":60}'
curl -X POST http://127.0.0.1:8790/rgb        -H 'Content-Type: application/json' \
     -d '{"leds":[{"i":0,"color":"#FF0000"},{"i":6,"color":"#00FF00"}]}'
curl -X POST http://127.0.0.1:8790/rgb/all    -H 'Content-Type: application/json' -d '{"color":"#0033FF"}'
curl -X POST http://127.0.0.1:8790/reboot

# 订阅事件流（含 IMU/触摸/按钮/IR/NFC 等）
curl -N "http://127.0.0.1:8790/events?types=imu,headTouch,screenTouch"

# 查询能力与驱动健康
curl http://127.0.0.1:8790/capabilities | jq
curl http://127.0.0.1:8790/health/drivers | jq
```
