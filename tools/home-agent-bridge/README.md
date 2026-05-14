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
