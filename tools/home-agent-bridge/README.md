# StackChan HomeAgent Bridge

Runs on the home computer and exposes a local HTTP API for Codex, OpenClaw, Hermes, or any other local agent.

```bash
export STACKCHAN_DEVICE_ID='<same id configured in firmware>'
export STACKCHAN_RELAY_URL='wss://relay.example.com/ws'
export STACKCHAN_RELAY_TOKEN='replace-me'
go run .
```

Local API:

```bash
curl http://127.0.0.1:8790/status
curl -X POST http://127.0.0.1:8790/message \
  -H 'Content-Type: application/json' \
  -d '{"name":"Codex","content":"我在家里的电脑上，已经接管 HomeAgent。"}'
curl -X POST http://127.0.0.1:8790/motion -d '{"yawServo":{"angle":200,"speed":500}}'
curl -X POST http://127.0.0.1:8790/camera/start
curl -X POST http://127.0.0.1:8790/camera/stop
curl -o snapshot.jpg -X POST http://127.0.0.1:8790/snapshot
```
