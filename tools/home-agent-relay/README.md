# StackChan HomeAgent Relay

A minimal WSS-friendly relay for the HomeAgent route:

```text
StackChan firmware -> VPS/Cloudflare relay -> home computer bridge
```

Run locally:

```bash
go run . -addr :8787
```

Use a shared token in production:

```bash
export STACKCHAN_RELAY_TOKEN='replace-me'
go run . -addr :8787
```

Endpoints:

```text
/ws?role=device&deviceId=<id>
/ws?role=agent&deviceId=<id>
```

Both sides connect outbound. The relay forwards binary and text WebSocket messages between the paired device and agent bridge.
