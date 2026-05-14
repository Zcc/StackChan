# HomeAgent 硬件能力落地设计（Phase 1）

## 背景与目标

当前 HomeAgent 协议与桥 API 已经覆盖了大部分硬件能力名，但仍存在两类差异：

1. **A 类能力**已接入且可用（设备信息、电池、亮度、音量、RGB、重启、部分事件）。
2. **B 类能力**仅为协议占位（IR/NFC/环境光等），缺少驱动和实机验证链路。

本设计的目标是完成 **Phase 1 可交付**：

- 把已有硬件钩子做成稳定可消费能力（尤其事件链路）。
- 给 IR/NFC/环境光建立统一驱动框架与可验证空跑路径。
- 不在本阶段承诺 IR/NFC/环境光“实机功能可用”，直到寄存器与连线验证完成。

## 范围

### In Scope

1. 固件 `hal_ws_avatar.cpp`
   - 结构化并强化事件 payload：
     - `ScreenTouchEvent`
     - `HeadTouchEvent`（补充位置/强度元数据）
     - `ImuEvent`（补充原始加速度摘要）
   - 统一错误响应规范：`CapabilityError` 增加 `code/message/capability/details` 字段。
2. 固件 HAL
   - 新增触摸屏事件采集与发送任务（基于 `hal_bridge::get_touch_point()`）。
   - 电池状态扩展字段（level/charging/discharging/source）。
   - IR/NFC/环境光的驱动抽象接口与任务框架（空跑实现 + 健康状态）。
3. bridge `tools/home-agent-bridge/main.go`
   - SSE 可靠性增强（订阅生命周期、慢消费者保护、心跳与类型过滤）。
   - 事件 schema 与错误透传统一。
   - 新增 `/capabilities` 与 `/health/drivers` 查询。
4. 文档
   - 更新 bridge README 的事件 payload 与错误码规范。

### Out of Scope

- IR/NFC/环境光的寄存器级真实驱动完成与实机调优。
- 麦克风上行、音频播放、SD 读写、LCD 抓帧的真实驱动实现。
- Flutter/server 端协议镜像改造。

## 方案对比

### 方案 A（采用）
先完成“已有钩子真实可用 + 新驱动框架可运行”，把 IR/NFC/环境光留在框架层（空跑+健康状态）。

**优点**
- 本轮即可交付稳定可用能力（事件、状态、SSE）。
- 不把“未验证驱动”伪装成完成，风险可控。
- 为 Phase 2 实机驱动补齐保留清晰边界。

**缺点**
- 用户感知上 IR/NFC/环境光仍非最终形态。

### 方案 B
本轮一次性实现全部驱动并保证实机可用。

**优点**：理论上一步到位。  
**缺点**：范围过大、硬件依赖重、回归成本高、失败风险高。

### 方案 C
仅修补桥层，不动固件驱动层。

**优点**：改动小。  
**缺点**：无法解决能力真实性与可靠性问题，价值低。

## 架构与组件设计

### 1. Event Producer 层（Firmware）

新增 `HomeAgentEventPublisher`（`hal_ws_avatar.cpp` 内部私有辅助）：

- 统一 `emitJson(type, payloadObj)`；
- 统一 timestamp/deviceId 注入；
- 对发送失败计数并上报 `onWsLog`。

事件类型：

- `ImuEvent`: `{"event":"shake|pickup","accel":{"x":..,"y":..,"z":..},"ts":...}`
- `HeadTouchEvent`: `{"gesture":"press|release|swipeForward|swipeBackward","strength":[a,b,c],"ts":...}`
- `ScreenTouchEvent`: `{"state":"down|move|up","x":..,"y":..,"ts":...}`

### 2. Driver Adapter 层（Firmware）

新增接口（同文件或新 `hal_homeagent_drivers.*`）：

- `IrDriverAdapter`
- `NfcDriverAdapter`
- `AmbientDriverAdapter`

接口统一：

- `Init() -> bool`
- `Poll() -> optional<Event>`
- `Health() -> {ready,lastError,lastTick}`
- `HandleCommand(payload) -> Result`

Phase 1 默认实现为 **Null/Probe Adapter**：

- 尝试初始化；
- 未发现硬件或无实现时返回结构化 `CapabilityError`；
- 在 `/health/drivers` 可见状态。

### 3. Bridge Event Hub（Go）

现有 `eventSubs` 改造成 `EventHub`：

- 每订阅独立 ring buffer（固定上限）；
- 慢消费者丢弃最旧消息并计数；
- 心跳 `: ping` 保持连接；
- 支持 `?types=imu,headTouch` 过滤；
- SSE 输出统一：
  - `event: <type>`
  - `data: <json>`

### 4. Capability & Health API（Go）

- `GET /capabilities`
  - 返回每项能力状态：`available|stub|disabled|error`
- `GET /health/drivers`
  - 汇总 firmware 返回的 adapter 健康信息。

## 数据流

1. 传感器/触摸在固件任务中采样。
2. `HomeAgentEventPublisher` 序列化为 WS 二进制包发送。
3. bridge `readLoop` 解包后写入 `EventHub`。
4. 本地 agent 通过 `/events` 订阅并按事件类型消费。
5. IR/NFC/环境光命令先经 adapter 路由，成功执行或返回结构化错误。

## 错误处理规范

`CapabilityError (0x4F)` payload：

```json
{
  "capability": "ir.send",
  "code": "not_implemented|driver_unavailable|bad_request|hw_fault",
  "message": "human readable",
  "details": {}
}
```

桥层规则：

- 固件返回 `CapabilityError` 时 HTTP 映射为 `409/422/503`（按 code 分类）。
- SSE 中仍保留 `error` 事件流，便于 agent 自恢复。

## 测试与验收

1. **编译验收**
   - `tools/home-agent-bridge`: `go build ./...`, `go vet ./...`
   - `firmware`: `idf.py build`
2. **行为验收**
   - `/events` 可稳定接收 `imu/headTouch/screenTouch`。
   - 断连重连后 SSE 自动恢复订阅。
   - `IR/NFC/环境光` 返回结构化错误，不出现 silent failure。
3. **回归**
   - 现有 `/say /look /motion /snapshot` 行为不退化。

## 实施顺序

1. 固件事件增强（screenTouch/headTouch/imu payload）
2. bridge EventHub 重构与 `/events` 强化
3. driver adapter 框架 + `/health/drivers`
4. 错误码与 README 同步

## 风险与缓解

- **风险**：事件频率过高导致 SSE 堵塞  
  **缓解**：每订阅 ring buffer + 采样限频（屏幕触摸 move 事件节流）
- **风险**：硬件未接入导致误报“可用”  
  **缓解**：`/capabilities` 明确状态，不把 stub 标成 available
- **风险**：固件任务增加影响实时性  
  **缓解**：独立低优先级任务 + 发送队列长度控制
