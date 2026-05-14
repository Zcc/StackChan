# StackChan HomeAgent Skill

Use the CLI from the repository root:

```bash
node skills/stackchan-home-agent/bin/stackchan-home-agent.js <command> [args]
```

Common commands:

```bash
node skills/stackchan-home-agent/bin/stackchan-home-agent.js status
node skills/stackchan-home-agent/bin/stackchan-home-agent.js say "Hello" --name HomeAgent
node skills/stackchan-home-agent/bin/stackchan-home-agent.js look --yaw 20 --pitch 0 --speed 500
node skills/stackchan-home-agent/bin/stackchan-home-agent.js light '#0000FF' --duration 1000
node skills/stackchan-home-agent/bin/stackchan-home-agent.js photo --out stackchan_snapshot.jpg
node skills/stackchan-home-agent/bin/stackchan-home-agent.js latest-photo --out stackchan_latest.jpg
node skills/stackchan-home-agent/bin/stackchan-home-agent.js camera-start
node skills/stackchan-home-agent/bin/stackchan-home-agent.js camera-stop
```

## mic-listen

Pull realtime microphone audio from StackChan as Opus 16 kHz mono 60 ms frames:

```bash
node skills/stackchan-home-agent/bin/stackchan-home-agent.js mic-listen --out captured.opus --duration-ms 10000
```

Decode the captured stream with an Opus tool such as:

```bash
opusdec --rate 16000 captured.opus captured.wav
```
