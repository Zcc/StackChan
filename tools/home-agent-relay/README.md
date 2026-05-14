# StackChan HomeAgent Relay

For the repeatable no-secrets-in-Git setup flow, see `../../docs/home_agent_setup.md`.


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
/ws?role=agent&deviceId=<id>&clientId=<agent-name>
```

Both sides connect outbound. The relay forwards binary and text WebSocket messages between one device and any number of agent clients for the same `deviceId`. Use `clientId` to keep bridge, CLI, and skill connections visible in `/status` without replacing each other.

Status endpoint:

```bash
curl -H 'Authorization: replace-me' https://relay.example.com/status
```

It returns the paired device/agent connection state for each `deviceId`.


Multiple agent clients can be connected at the same time. For example, the home bridge can use `clientId=home-agent-bridge`, while a temporary CLI can use `clientId=ws-control`. Device messages are broadcast to all connected agents; agent messages are forwarded to the device.
