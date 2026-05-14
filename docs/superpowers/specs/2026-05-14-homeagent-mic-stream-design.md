# HomeAgent 麦克风 Opus 流（Phase2 设计）

日期：2026-05-14
状态：草案，等待用户审阅
关联：`docs/superpowers/specs/2026-05-14-homeagent-hardware-rollout-design.md`

## 目标

让 agent 能通过 HomeAgent 通道控制 StackChan 设备开/关麦克风，并实时获得 Opus 编码的音频流，用于语音指令、ASR、录音落盘等场景。复用小智 (`xiaozhi-esp32`) 已经验证的音频参数与编码组件，最小化新组件、最大化与现有 WS 二进制协议的一致性。

## 范围

In:
- 固件端：在 HomeAgent 模式下启用麦克风采集 + Opus 编码，按现有 `[type(1)][len(4 BE)][payload]` 帧上行；接收 `MicStreamStart/Stop` 控制；上报 mic 状态与错误。
- bridge 端：实装 `POST /mic/start`、`POST /mic/stop`，新增 `GET /mic/ws` 单订阅者出口透传 Opus 帧，`GET /mic/status` 返回当前状态；SSE 仅推状态/统计事件（不夹音频字节）。
- agent CLI（`skills/stackchan-home-agent`）：新增 `mic-listen` 命令，封装 start→ws 接收→stop，支持可选落盘。

Out（不做）:
- ASR/语音指令解析（由 agent 自行接其他模型/服务）。
- 设备端音频播放/双工对话（Phase3 单独评估）。
- 多订阅者扇出（明确单订阅者）。

## 协议

### WS 二进制帧（设备 ↔ relay ↔ bridge）

复用现有 type 字节：
- `MicStreamStart = 0x37`（入站，bridge→device）
  - payload: JSON
    ```json
    { "sample_rate": 16000, "channels": 1, "frame_duration_ms": 60, "duration_ms": 30000, "stream_id": "<uuid>" }
    ```
  - 字段缺省按上面默认值；`stream_id` 由 bridge 生成。
- `MicStreamStop = 0x38`（入站，bridge→device）
  - payload: JSON `{ "stream_id": "<uuid>" }`（缺省则停止当前流）。
- `MicAudio = 0x39`（出站，device→bridge）
  - payload 头 16 字节定长 + 裸 Opus：
    ```
    offset 0..3   uint32 BE  stream_id_hash（取 stream_id 的 FNV-1a 32 位，便于校验 + 调试）
    offset 4..7   uint32 BE  seq            （从 0 开始，每帧 +1）
    offset 8..15  uint64 BE  timestamp_ms   （设备 monotonic millis）
    offset 16..   bytes      opus_payload   （单个 Opus 帧，可变长，通常 ≤ 200 字节）
    ```
- 状态/错误：
  - `CapabilityError(0x4F)` 用于 start/stop 拒绝、编码器初始化失败、I2S 错误等；`capability="mic"`，code 取值见“错误处理”。
  - 新增 `MicStatus = 0x3C`（出站，device→bridge），payload 为 JSON：
    - `{ "event":"started", "stream_id":..., "sample_rate":16000, "channels":1, "frame_duration_ms":60, "duration_ms":30000 }`
    - `{ "event":"stopped", "stream_id":..., "reason":"user"|"timeout"|"error"|"disconnected", "frames":..., "bytes":... }`
    - `{ "event":"stats",   "stream_id":..., "frames":..., "bytes":..., "dropped":... }`（可选，1Hz）

### bridge HTTP API

- `POST /mic/start`
  - body（全部可选）：`{ "duration_ms": 30000 }`
  - duration 上限 300000ms（5分钟），缺省 30000ms。
  - 行为：若已有活动流则 409；否则下发 `MicStreamStart`，等待 `mic.started` 事件或 2s 超时；成功返回 `{ stream_id, started_at }`。
- `POST /mic/stop`
  - 行为：下发 `MicStreamStop`；返回 `{ stream_id, stopped }`。
- `GET /mic/status`
  - 返回 `{ active, stream_id?, started_at?, duration_ms?, frames?, bytes? }`。
- `GET /mic/ws`
  - 升级到 WebSocket。**单订阅者**：若已有连接，第二个握手返回 HTTP 409。
  - 服务端推送二进制帧：直接转发设备 `MicAudio` 的 payload（含 16 字节头 + Opus），WS opcode = Binary。
  - 服务端推送文本帧（少量）：`{"type":"mic.started"|"mic.stopped"|"mic.stats", ...}` 以便客户端无需另开 SSE 即可知道流边界。
  - 客户端不需发任何消息；写入将被忽略。
  - 客户端断开时，bridge 不自动停止流（流仍然依据 timeout 或显式 `/mic/stop` 结束），但状态事件继续通过 SSE 暴露。

### 安全/限制

- bridge 与 agent 之间复用现有 `STACKCHAN_BRIDGE_TOKEN`：对 `/mic/*` HTTP 与 `/mic/ws` 升级都强制鉴权（Header `Authorization: Bearer <token>`，缺省允许 loopback 时同现有策略）。
- 启动时长强制上限 300000ms；超过则 400。
- 设备端如果在 xiaozhi 会话中或在 OTA/低电状态，拒绝 start，返回 `CapabilityError{ capability:"mic", code:"busy"|"unavailable" }`。

## 组件 / 数据流

```
┌──────────────┐  Opus 帧 0x39       ┌────────┐  Opus 帧 0x39      ┌────────────┐  二进制 WS
│ device (HAL) │ ──────────────────► │ relay  │ ────────────────► │ bridge      │ ──────────► /mic/ws (单订阅)
│  I2S → enc   │                     │ (pass) │                    │  router     │ ──────────► SSE 状态事件
│              │ ◄────── 0x37/0x38 ──│        │ ◄────────── 0x37/8 │             │ ◄────────── POST /mic/start/stop
└──────────────┘                     └────────┘                    └────────────┘                  ▲
                                                                                                   │
                                                                                  ┌──────────────────────────┐
                                                                                  │ agent CLI: mic-listen     │
                                                                                  │  ws 客户端 + 文件落盘    │
                                                                                  └──────────────────────────┘
```

### 固件单元

新增 `firmware/main/hal/hal_mic.cpp` / `hal_mic.h`：
- 职责：拥有 I2S 输入与 Opus 编码器（参数同小智：16k/1ch/60ms）。
- 接口（在 HAL 单例上暴露）：
  - `bool StartMicStream(const MicStreamConfig& cfg, std::string* err)`
  - `void StopMicStream(const std::string& reason)`
  - `signal<const MicFrame&> onMicFrame`（含 seq/ts/payload）
  - `signal<const MicStatusEvent&> onMicStatus`（started/stopped/stats）
- 内部：FreeRTOS 任务读取 I2S，每 60ms 一帧 PCM → Opus，推送到信号；定时器跟踪 duration_ms 自动停。
- 错误：编码器初始化失败、I2S 故障 → `onMicStatus` 发 stopped(reason=error) + 返回错误字符串。
- 与小智冲突：当处于 xiaozhi 模式（`AppAiAgent` 接管音频）时，HomeAgent 不应启动 mic；`AppHomeAgent` 入口已和 xiaozhi 互斥（main.cpp 切换）。在此基础上 `StartMicStream` 内再做一次 board audio codec 占用检查；占用则返回 `busy`。

`hal_ws_avatar.cpp` 改动：
- `handleMessage` 增加 `MicStreamStart/Stop` 处理：解析 JSON → 调用 `GetHAL().StartMicStream/StopMicStream` → 失败发 `CapabilityError`。
- 订阅 `onMicFrame`：序列化 16 字节头 + Opus → 走 `sendBinary(MicAudio, ...)`。
- 订阅 `onMicStatus`：序列化为事件 payload → `sendEvent("mic.started"/...)`。

### bridge 单元

`tools/home-agent-bridge/main.go`：
- 新增 `micState` 结构：`{ mu sync.Mutex; active bool; streamID string; startedAt time.Time; durationMs int; frames, bytes uint64; subscriber *micSub }`。
- `micSub` 结构：`{ conn *websocket.Conn; send chan []byte; closed chan struct{} }`，背压策略：`send` buffer = 64 帧；满则关闭订阅（不影响设备流）。
- 路由：
  - `POST /mic/start` `POST /mic/stop` `GET /mic/status` `GET /mic/ws`。
- 设备入站事件分发：
  - 收到 `MicAudio(0x39)` 时：更新计数；若有订阅者则非阻塞发送（满即关订阅）。
  - 收到 `mic.started/stopped/stats` 事件时：更新 `micState`；推 SSE（types 过滤可用 `mic.*`）；同时通过 `/mic/ws` 文本帧通知订阅者。
- `/capabilities` 输出加入 `"mic": { "format":"opus", "sample_rate":16000, "channels":1, "frame_duration_ms":60, "max_duration_ms":300000 }`。

### agent CLI 单元

`skills/stackchan-home-agent`：
- 新增子命令 `mic-listen [--duration-ms <n>] [--out <file.opus>]`：
  - 调 `/mic/start`，拿 stream_id。
  - 拨 `/mic/ws`：消费二进制帧（剥 16 字节头）→ 写入 `--out`（裸 Opus 拼接，可后续 `opusdec` 解码）或丢弃；订阅文本帧得到 stopped 后退出。
  - Ctrl-C → 调 `/mic/stop`；超时同。
  - 退出码：0 正常完成，2 设备拒绝，3 网络断开，4 订阅冲突。
- SKILL.md / README 更新用例与示例命令。

## 错误处理

- bridge 启动时 mic 冲突：`POST /mic/start` 409 `{ code:"mic.busy" }`。
- bridge `/mic/ws` 第二订阅：HTTP 409 `mic.subscriber.exists`。
- 设备返回 `CapabilityError{capability:"mic"}`：bridge 把 code 映射为 HTTP（`busy→409`、`unavailable→503`、`invalid_args→400`、其他→502）。
- 设备掉线（已有重连逻辑）：bridge 把当前 `micState` 标记为 stopped(reason="disconnected") 并断开订阅 WS。

## 测试

bridge（Go，单测）：
- `TestMicStartConflict`：第二次 start 在已 active 时返回 409。
- `TestMicWSSingleSubscriber`：第二次 `/mic/ws` 升级返回 409。
- `TestMicFrameFanout`：注入 `MicAudio` 帧，订阅者收到的二进制 payload 与注入一致。
- `TestMicBackpressureDropsSubscriber`：订阅者不消费，缓冲满后被踢，设备帧继续被计数。
- `TestMicStatusReflectsEvents`：注入 mic.started → `/mic/status` active；mic.stopped → idle。
- `TestMicCapabilityErrorMapping`：busy→409、unavailable→503。

agent CLI（node 测试）：
- 单测命令解析；mock bridge：mic-listen 落盘字节数等于注入字节数；ctrl-c 触发 /mic/stop。

固件（构建级）：
- `idf.py build` 通过；分区余量观察。
- 实机 smoke（用户执行）：start → /mic/ws 抓 5s → opusdec 还原能听到人声；start 超时自动 stop；stop 显式触发立即 stopped。

## 风险与回退

- 分区紧（当前 ~2% free）：Opus 编码器（已被 xiaozhi 引用，组件本身已链入）应不带来明显增量；若 build 超分区，可在 Kconfig 增加 `CONFIG_HOMEAGENT_MIC` 默认 off，作为可选编译。
- I2S 与板载麦冲突：在非 CoreS3/无内置麦克风板，start 返回 unavailable；`/capabilities` 通过 driver probe 标记 mic 为 unhealthy。
- 流量：60ms/帧、平均 ~30kbps，单订阅可控；不做扇出避免相关复杂度。

## 待执行步骤

1. 用户审阅 spec。
2. 通过后由 writing-plans 产出实施计划（含 TDD 顺序）。
3. 按计划执行：bridge 测试先红 → bridge 实现 → 固件 HAL/路由 → CLI → 构建 + 实机 smoke。
