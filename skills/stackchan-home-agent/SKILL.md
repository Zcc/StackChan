---
name: stackchan-home-agent
description: Control a StackChan running HomeAgent through the local home-agent bridge. Use this skill when an agent needs to make StackChan speak, look around, control RGB lights, or capture a camera snapshot.
---

# StackChan HomeAgent Skill

This skill controls StackChan through the local HomeAgent bridge HTTP API. The bridge must already be running on the home computer and connected to the relay.

Default bridge URL:

```text
http://127.0.0.1:8790
```

Configuration is read from environment variables first, then from the repository-local `tools/.env.local` file when present.

Supported environment variables:

```text
HOMEAGENT_BRIDGE_URL=http://127.0.0.1:8790
HOMEAGENT_BRIDGE_TOKEN=<optional local bridge token>
```

The skill must not read or expose relay tokens, relay URLs, or device IDs unless the user explicitly asks for diagnostics. Prefer local bridge operations over direct relay/WebSocket access.

## Command

Use the bundled command from the repository root:

```bash
node skills/stackchan-home-agent/bin/stackchan-home-agent.js <command> [args]
```

Commands return JSON on success or a non-zero exit code with an error JSON object.

## Tools

### Status

Check whether the bridge is connected to StackChan:

```bash
node skills/stackchan-home-agent/bin/stackchan-home-agent.js status
```

Use this before camera or motion operations when connection state is uncertain.

### Say

Display a short message on StackChan:

```bash
node skills/stackchan-home-agent/bin/stackchan-home-agent.js say "Hello from the home agent" --name Codex
```

Keep messages concise. Avoid sending sensitive content to the device screen unless the user requested it.

### Look

Move StackChan's head:

```bash
node skills/stackchan-home-agent/bin/stackchan-home-agent.js look --yaw 20 --pitch 0 --speed 500
```

Use small movement values by default. Avoid rapid repeated movement commands.

### Light

Set both RGB lights:

```bash
node skills/stackchan-home-agent/bin/stackchan-home-agent.js light '#0000FF' --duration 1000
```

Turn lights off:

```bash
node skills/stackchan-home-agent/bin/stackchan-home-agent.js light-off
```

### Photo

Take one snapshot and save it to a local file:

```bash
node skills/stackchan-home-agent/bin/stackchan-home-agent.js photo --out /tmp/stackchan_snapshot.jpg
```

When no `--out` is provided, the command saves to `/tmp/stackchan_snapshot_<timestamp>.jpg` and prints the file path as JSON.

### Latest Photo

Save the latest cached snapshot from the bridge:

```bash
node skills/stackchan-home-agent/bin/stackchan-home-agent.js latest-photo --out /tmp/stackchan_latest.jpg
```

### Camera Stream

Start or stop camera streaming:

```bash
node skills/stackchan-home-agent/bin/stackchan-home-agent.js camera-start
node skills/stackchan-home-agent/bin/stackchan-home-agent.js camera-stop
```

Prefer single snapshots over streaming unless the user needs continuous visual feedback.

## Safety And Behavior

- Check `status` before assuming StackChan is reachable.
- Use `light-off` after visual tests that turn lights on.
- Use single `photo` calls before continuous camera streaming.
- Do not print bridge tokens or relay tokens.
- Treat photos as local user data. Do not upload or share them unless the user asks.
- If the bridge is not connected, tell the user to start `tools/home-agent-bridge` and open `HOME.AGENT` on StackChan.
