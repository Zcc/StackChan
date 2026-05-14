# Copilot Instructions for StackChan

StackChan is a monorepo for the M5Stack StackChan robot. The top-level components are independent projects that work together at runtime but do not share source code:

- `firmware/` - ESP-IDF C++ firmware for the ESP32-S3/CoreS3 robot.
- `app/` - Flutter mobile app for iOS/Android.
- `server/` - Go/GoFrame backend with MySQL.
- `remote/code/` - ESP-IDF firmware for the StickC-Plus ESP-NOW remote controller.
- `tools/home-agent-relay/` and `tools/home-agent-bridge/` - Go utilities for the HomeAgent relay path.

## Build, test, and lint commands

Run commands from the component directory unless noted.

### Firmware (`firmware/`)

Requires ESP-IDF v5.5.4 for ESP32-S3.

```bash
python3 ./fetch_repos.py  # clones pinned external dependencies from repos.json and applies patches
idf.py build
idf.py flash
idf.py -p /dev/cu.usbmodem14501 monitor  # example serial monitor port on macOS
```

There is no dedicated firmware unit-test command in the repository. Board, language, OTA, wake-word, and feature choices are Kconfig-driven (`idf.py menuconfig`).

### Mobile app (`app/`)

Requires Flutter/Dart 3.x.

```bash
flutter pub get
flutter analyze
flutter test
flutter test test/widget_test.dart  # single test file
flutter run -d ios                  # or: flutter run -d android
flutter build ios --release
flutter build apk --release
flutter build appbundle --release
```

### Server (`server/`)

`go.mod` currently declares Go 1.26.2. The server uses GoFrame and listens on port 12800.

```bash
go mod download
go run main.go
go test ./internal/...
go test ./internal/service -run TestCreateMac  # single Go test
make build                                    # runs gf build -ew
make ctrl                                     # gf gen ctrl
make dao                                      # gf gen dao
make service                                  # gf gen service
```

The Makefile auto-installs the GoFrame CLI (`gf`) through `hack/hack-cli.mk` if it is missing. Database setup is in `check_list/create_mysql_database.sql`; runtime config is in `manifest/config/config.yaml`.

### Remote controller (`remote/code/`)

Requires ESP-IDF for ESP32; the README specifies ESP-IDF v5.4.2 and target device StickC-Plus + Hat Mini JoyC.

```bash
idf.py build
idf.py flash -b 1500000
```

Before building, the remote README notes that `M5GFX` may need every `__has_include(<driver/i2c_master.h>)` occurrence replaced with `0`.

### HomeAgent tools

```bash
cd tools/home-agent-relay
go run . -addr :8787

cd ../home-agent-bridge
export STACKCHAN_DEVICE_ID='<same id configured in firmware>'
export STACKCHAN_RELAY_URL='wss://relay.example.com/ws'
export STACKCHAN_RELAY_TOKEN='replace-me'
export STACKCHAN_BRIDGE_TOKEN='local-agent-token' # optional local API bearer token
go run .
```

Bridge smoke checks:

```bash
curl http://127.0.0.1:8790/status
curl -X POST http://127.0.0.1:8790/say -H 'Content-Type: application/json' -d '{"name":"Copilot","content":"hello"}'
curl -o latest.jpg http://127.0.0.1:8790/snapshot/latest
```

## Architecture

### Firmware

`firmware/main/main.cpp` initializes the HAL singleton (`GetHAL()`), configures the UI timing hooks, installs Mooncake apps, and runs the Mooncake event loop until Xiaozhi mode is requested. Installed apps include Launcher, AI Agent, HomeAgent, Avatar, ESP-NOW Control, App Center, Ezdata, Dance, and Setup.

The HAL layer (`firmware/main/hal/`) owns hardware and connectivity services: display/LVGL, Wi-Fi, NVS settings, BLE config/control, OTA/reboot, camera/WebSocket transport, and HomeAgent config persistence. The StackChan core (`firmware/main/stackchan/`) owns avatar state, motion/servo control, animation modifiers, and JSON parsing for avatar/motion/RGB/dance data.

External firmware sources are not submodules. `firmware/repos.json` is the source of truth for cloned dependencies such as Mooncake, ArduinoJson, ESP-NOW, and `xiaozhi-esp32`; `fetch_repos.py` clones pinned refs and applies `firmware/patches/xiaozhi-esp32.patch`.

The WebSocket avatar protocol uses binary packets shaped as `[1 byte type][4 byte big-endian payload length][payload]`. Important message types are mirrored in the Flutter app (`app/lib/model/msg_type.dart`) and HomeAgent bridge: JPEG `0x02`, avatar `0x03`, motion `0x04`, camera start/stop `0x05/0x06`, text message `0x07`, dance `0x14`, aimed photo `0x1A`, ping/pong `0x10/0x11`.

HomeAgent is a fork-specific path: `AppHomeAgent` starts the same WebSocket avatar service but can point it at a relay configured over BLE. HomeAgent settings are persisted in the HAL under the `home_agent` namespace and configured with BLE commands `setHomeAgent`, `getHomeAgent`, and `resetHomeAgent`.

### Flutter app

The app entry point (`app/lib/main.dart`) registers `AppState` with GetX, initializes persisted app/device state, starts the audio engine, and runs `App`. Global state lives in `AppState.shared` (`GetxController`), with reactive fields modeled as `Rx`, `Rxn`, and `RxList`; UI code commonly rebuilds with `Obx`.

The app is Cupertino-first and organized by responsibility: `lib/view/` for screens and widgets, `lib/network/` for HTTP/WebSocket, `lib/util/` for singleton services, and `lib/model/` for DTOs and protocol models. Important singletons include `BlueUtil.shared` for BLE, `WebSocketUtil.shared` for backend/device WebSocket traffic, and `AppState.shared` for cross-screen state.

BLE supports both setup/config and dance/control services. `BlueUtil` scans for `e2e5e5ff-1234-5678-1234-56789abcdef0` and `e2e5e5e0-1234-5678-1234-56789abcdef0`; motion/avatar/config/RGB characteristics use `e2e5e5e1` through `e2e5e5e4`. HomeAgent mobile configuration is exposed from settings via `HomeAgentConfigPage`.

Backend endpoints are configured in `app/lib/network/urls.dart`; RSA and shared constants live in `app/lib/util/value_constant.dart`. Do not commit real production keys, keystores, or passwords.

### Server

The server is a GoFrame application. `server/main.go` delegates to `internal/cmd.Main`; `internal/cmd/cmd.go` wires CORS, `/stackChan/ws`, static `/file/*`, `/stackChan/v2`, `/stackChan`, and `/admin/stackChan` route groups, then sets the port to 12800.

The server follows GoFrame's generated layered structure:

- `api/` defines request/response structs.
- `internal/controller/` handles route methods.
- `internal/service/` defines service-facing functions/interfaces.
- `internal/logic/` contains business logic implementations when present.
- `internal/dao/` and `internal/model/{do,entity}/` are generated from the database schema.
- `internal/web_socket/` brokers StackChan device and app WebSocket clients.
- `internal/xiaozhi/` integrates with XiaoZhi APIs.

Token middleware is applied at the route-group level. `/stackChan/ws` is bound directly to the WebSocket handler, which authenticates using the `Authorization` header and routes app/device messages.

### HomeAgent tools

`tools/home-agent-relay` is a minimal WebSocket relay. Device and agent clients connect to `/ws?role=device|agent&deviceId=<id>`, and the relay forwards binary/text frames between paired peers. If `STACKCHAN_RELAY_TOKEN` is set, clients must send it as the raw `Authorization` header.

`tools/home-agent-bridge` connects as the agent side and exposes a local HTTP API on `127.0.0.1:8790` by default. Its routes translate local HTTP calls (`/say`, `/look`, `/motion`, `/avatar`, `/dance`, `/camera/start`, `/camera/stop`, `/snapshot`) into the same WebSocket binary packet protocol used by the firmware and app.

## Key conventions

- Firmware C++ style is configured by `.clang-format`: Google base style, 4-space indent, 120-column limit, left pointer alignment.
- Firmware hardware access should go through `GetHAL()` and robot/avatar/motion state through `GetStackChan()` rather than adding parallel globals.
- Firmware app UI code should use LVGL under the existing lock pattern (`LvglLockGuard`) when touching UI objects.
- Keep WebSocket message type IDs synchronized across firmware (`hal_ws_avatar.cpp`), Flutter (`msg_type.dart`), server (`internal/web_socket`), and HomeAgent bridge.
- BLE config commands are JSON messages handled in `hal_ble.cpp`; add new config commands there and mirror request/response models in the Flutter app.
- Do not manually edit GoFrame files with generation headers, especially `server/internal/dao/internal/*`, generated `server/internal/model/{do,entity}/*`, generated controller scaffolding, and generated service interfaces. Use `make dao`, `make ctrl`, or `make service`.
- Flutter state changes that affect UI should use GetX reactive fields and `Obx`; persistent app/device settings use `SharedPreferencesAsync` through `AppState`.
- App release signing files (`android/key.properties`, `*.jks`) and real server/RSA/JWT/XiaoZhi secrets must remain outside git.
- The repository may lag behind released firmware/app binaries; verify behavior against source before assuming factory firmware behavior matches this branch.
