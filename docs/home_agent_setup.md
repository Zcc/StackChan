# StackChan HomeAgent Setup

This document is the repeatable setup path for the HomeAgent route:

```text
StackChan HomeAgent -> public relay -> home computer bridge -> local agents
```

The repository must not contain private relay URLs, tokens, or device IDs. Put real values only in local env files that are ignored by Git.

## 1. Clone And Checkout

```bash
git clone https://github.com/Zcc/StackChan.git
cd StackChan
git checkout codex/diy
```

## 2. Create Local Secrets

```bash
cp tools/.env.example tools/.env.local
```

Edit `tools/.env.local` on your machine:

```bash
RELAY_URL=wss://your-relay.example.com/ws
RELAY_TOKEN=your-private-relay-token
DEVICE_ID=AABBCCDDEEFF
BRIDGE_TOKEN=local-agent-token
```

Rules:

- `tools/.env.local` is ignored by Git.
- Do not put real tokens into source code, docs, commit messages, or screenshots.
- Rotate `RELAY_TOKEN` if it was ever pushed or shared accidentally.

## 3. Configure StackChan Over BLE

Start StackChan and enter the BLE/app configuration mode used by the firmware. Then run:

```bash
./tools/config_homeagent.sh set
./tools/config_homeagent.sh get
```

Expected result:

- `set` prints the relay URL and a masked token.
- `get` returns `notifyHomeAgent` with `enabled: true`, the relay URL, the device ID, and `hasToken: true`.

To clear HomeAgent configuration:

```bash
./tools/config_homeagent.sh reset
```

## 4. Flash Firmware

From the firmware directory, use your local ESP-IDF environment:

```bash
cd firmware
. ~/esp/esp-idf-v5.5.4/export.sh
IDF_SKIP_CHECK_SUBMODULES=1 idf.py build
IDF_SKIP_CHECK_SUBMODULES=1 idf.py -p /dev/cu.usbmodem14401 flash
```

Adjust the serial port for your machine.

## 5. Run The Relay On VPS

The relay should load its token from a private env file on the server, not from the repo.

Example `/etc/stackchan-relay.env`:

```bash
STACKCHAN_RELAY_TOKEN=your-private-relay-token
```

Run manually for testing:

```bash
cd tools/home-agent-relay
set -a
source /etc/stackchan-relay.env
set +a
go run . -addr :8787
```

For systemd, start from `tools/home-agent-relay/stackchan-relay.service.example` and keep the real token in `/etc/stackchan-relay.env`.

Status check:

```bash
curl -H "Authorization: $STACKCHAN_RELAY_TOKEN" https://your-relay.example.com/status
```

## 6. Run The Bridge On The Home Computer

On the home computer, reuse `tools/.env.local`:

```bash
set -a
source tools/.env.local
set +a

export STACKCHAN_RELAY_URL="$RELAY_URL"
export STACKCHAN_RELAY_TOKEN="$RELAY_TOKEN"
export STACKCHAN_DEVICE_ID="$DEVICE_ID"
export STACKCHAN_BRIDGE_TOKEN="$BRIDGE_TOKEN"

cd tools/home-agent-bridge
go run .
```

The bridge listens on `127.0.0.1:8790` by default.

For systemd, start from `tools/home-agent-bridge/stackchan-bridge.service.example` and keep real values in `/etc/stackchan-bridge.env`.

## 7. Verify End To End

Open `HOME.AGENT` on StackChan. It should connect to Wi-Fi, then to the relay.

From the home computer:

```bash
curl http://127.0.0.1:8790/status

curl -X POST http://127.0.0.1:8790/say \
  -H 'Content-Type: application/json' \
  -d '{"name":"Codex","content":"HomeAgent route is online."}'

curl -X POST http://127.0.0.1:8790/look \
  -H 'Content-Type: application/json' \
  -d '{"yaw":20,"pitch":0,"speed":500}'

curl -X POST http://127.0.0.1:8790/light \
  -H 'Content-Type: application/json' \
  -d '{"color":"#0000FF","durationMs":1000}'

curl -X POST http://127.0.0.1:8790/light \
  -H 'Content-Type: application/json' \
  -d '{"color":"#000000","durationMs":500}'

curl -o snapshot.jpg -X POST http://127.0.0.1:8790/snapshot
```

If `BRIDGE_TOKEN` is set:

```bash
curl -H "Authorization: Bearer $BRIDGE_TOKEN" http://127.0.0.1:8790/status
```

## 8. First-Phase Acceptance

The first phase is considered stable when:

- The Git repo contains no private relay URL, token, or device ID.
- A fresh clone can configure StackChan using `tools/.env.local` and `tools/config_homeagent.sh`.
- StackChan reconnects to the relay after reboot.
- Relay heartbeat stays healthy.
- The home bridge can send text to the screen.
- Basic control commands, such as motion and light control, work through the bridge or relay client.

## Troubleshooting

If HomeAgent says `Set relay in the app`, run `./tools/config_homeagent.sh get` and confirm `relayUrl` and `hasToken`.

If BLE times out, make sure macOS has Bluetooth permission for the terminal app you are using, then restart the terminal.

If the relay status shows only the agent or only the device, verify both sides use the same `DEVICE_ID` and `RELAY_TOKEN`.

If Wi-Fi is unavailable, re-enter Wi-Fi configuration mode and write the current Wi-Fi or phone hotspot credentials before opening `HOME.AGENT` again.
