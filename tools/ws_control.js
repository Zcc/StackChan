#!/usr/bin/env node
// Connect to StackChan relay as agent and send commands
// Binary protocol: [1 byte type][4 byte BE payload length][payload]
// Usage: node ws_control.js [command] [args...]
//   say <name> <text>    - show text message on device
//   avatar <json>        - send avatar control JSON
//   motion <json>        - send motion control JSON
//   look <yaw> <pitch>   - look at angle (e.g. look 30 -10)
//   dance <json>         - send dance sequence
//   light <color> [ms]   - set both RGB lights, e.g. light #0000FF 1000
//   ping                 - send heartbeat ping
//   interactive          - enter interactive mode

const WebSocket = require('ws');
const fs = require('fs');
const path = require('path');

function loadLocalEnv() {
    const envPath = process.env.HOMEAGENT_ENV_FILE || path.join(__dirname, '.env.local');
    if (!fs.existsSync(envPath)) return;

    const lines = fs.readFileSync(envPath, 'utf8').split(/\r?\n/);
    for (const line of lines) {
        const trimmed = line.trim();
        if (!trimmed || trimmed.startsWith('#')) continue;
        const eq = trimmed.indexOf('=');
        if (eq <= 0) continue;
        const key = trimmed.slice(0, eq).trim();
        const value = trimmed.slice(eq + 1).trim().replace(/^(["'])(.*)\1$/, '$2');
        if (!process.env[key]) process.env[key] = value;
    }
}

loadLocalEnv();

const RELAY_URL = process.env.RELAY_URL || '';
const DEVICE_ID = process.env.DEVICE_ID || '';
const TOKEN = process.env.RELAY_TOKEN || '';

if (!RELAY_URL || !DEVICE_ID || !TOKEN) {
    console.error('Error: missing required env vars');
    console.error('  RELAY_URL    e.g. ws://relay.example.com:8787/ws');
    console.error('  DEVICE_ID    e.g. AABBCCDDEEFF (device MAC)');
    console.error('  RELAY_TOKEN  authorization token');
    process.exit(1);
}

// Message types
const TYPE = {
    JPEG: 0x02,
    AVATAR: 0x03,
    MOTION: 0x04,
    CAMERA_START: 0x05,
    CAMERA_STOP: 0x06,
    TEXT: 0x07,
    PING: 0x10,
    PONG: 0x11,
    DANCE: 0x14,
};

function buildPacket(type, payload) {
    const payloadBuf = payload ? Buffer.from(payload, 'utf8') : Buffer.alloc(0);
    const header = Buffer.alloc(5);
    header[0] = type;
    header.writeUInt32BE(payloadBuf.length, 1);
    return Buffer.concat([header, payloadBuf]);
}

function buildTextMessage(name, content) {
    const json = JSON.stringify({ name, content });
    return buildPacket(TYPE.TEXT, json);
}

function buildMotion(yaw, pitch, speed = 500) {
    const servoSpeed = Math.max(1, parseInt(speed, 10) || 500);
    const json = JSON.stringify({
        yawServo: {
            angle: parseFloat(yaw) || 0,
            speed: servoSpeed,
        },
        pitchServo: {
            angle: parseFloat(pitch) || 0,
            speed: servoSpeed,
        },
    });
    return buildPacket(TYPE.MOTION, json);
}

function normalizeHexColor(value) {
    if (!value) return '#000000';
    if (/^[0-9a-fA-F]{6}$/.test(value)) return `#${value}`;
    if (/^#[0-9a-fA-F]{6}$/.test(value)) return value;
    throw new Error('color must be #RRGGBB');
}

function neutralLightPart(weight) {
    return {
        position: { x: 0, y: 0 },
        rotation: 0,
        weight,
        size: 0,
    };
}

function buildLight(color, durationMs = 1000) {
    const rgb = normalizeHexColor(color);
    const sequence = [{
        leftEye: neutralLightPart(100),
        rightEye: neutralLightPart(100),
        mouth: neutralLightPart(0),
        yawServo: { angle: 0, speed: 0 },
        pitchServo: { angle: 0, speed: 0 },
        leftRgbColor: rgb,
        rightRgbColor: rgb,
        durationMs: Math.max(1, parseInt(durationMs, 10) || 1000),
    }];
    return buildPacket(TYPE.DANCE, JSON.stringify(sequence));
}

function parsePacket(data) {
    if (data.length < 5) return null;
    const type = data[0];
    const len = data.readUInt32BE(1);
    const payload = data.slice(5, 5 + len);
    return { type, payload };
}

const typeName = (t) => Object.keys(TYPE).find(k => TYPE[k] === t) || `0x${t.toString(16)}`;

function connect(onOpen) {
    const url = `${RELAY_URL}?role=agent&deviceId=${DEVICE_ID}`;
    const ws = new WebSocket(url, { headers: { Authorization: TOKEN } });

    ws.on('open', () => {
        console.log('✅ Connected to relay as agent');
        onOpen(ws);
    });

    ws.on('message', (data) => {
        const buf = Buffer.from(data);
        const pkt = parsePacket(buf);
        if (!pkt) { console.log('⬅ raw:', buf.toString('hex')); return; }

        if (pkt.type === TYPE.PONG) {
            console.log('⬅ PONG');
        } else if (pkt.type === TYPE.JPEG) {
            console.log(`⬅ JPEG frame (${pkt.payload.length} bytes)`);
        } else if (pkt.type === TYPE.TEXT) {
            try {
                const msg = JSON.parse(pkt.payload.toString('utf8'));
                console.log(`⬅ TEXT: [${msg.name}] ${msg.content}`);
            } catch { console.log(`⬅ TEXT: ${pkt.payload.toString('utf8')}`); }
        } else {
            const payloadStr = pkt.payload.length > 0 ? pkt.payload.toString('utf8') : '';
            console.log(`⬅ ${typeName(pkt.type)}: ${payloadStr}`);
        }
    });

    ws.on('close', (code, reason) => {
        console.log(`❌ Disconnected: ${code} ${reason}`);
    });

    ws.on('error', (err) => {
        console.error('Error:', err.message);
    });

    return ws;
}

// --- Main ---
const args = process.argv.slice(2);
const cmd = args[0] || 'interactive';

if (cmd === 'interactive') {
    const readline = require('readline');
    const rl = readline.createInterface({ input: process.stdin, output: process.stdout });

    connect((ws) => {
        console.log('Commands: say <name> <text> | look <yaw> <pitch> | light <#RRGGBB> [ms] | avatar <json> | motion <json> | dance <json> | ping | quit');

        const prompt = () => rl.question('> ', (line) => {
            const parts = line.trim().split(/\s+/);
            const c = parts[0];

            if (c === 'quit' || c === 'exit') { ws.close(); process.exit(0); }
            else if (c === 'ping') { ws.send(buildPacket(TYPE.PING)); console.log('➡ PING'); }
            else if (c === 'say') { 
                const name = parts[1] || 'Copilot';
                const text = parts.slice(2).join(' ') || 'Hello!';
                ws.send(buildTextMessage(name, text)); 
                console.log(`➡ TEXT: [${name}] ${text}`);
            }
            else if (c === 'look') {
                ws.send(buildMotion(parts[1] || 0, parts[2] || 0));
                console.log(`➡ MOTION: look yaw=${parts[1]||0} pitch=${parts[2]||0}`);
            }
            else if (c === 'avatar') {
                ws.send(buildPacket(TYPE.AVATAR, parts.slice(1).join(' ')));
                console.log('➡ AVATAR');
            }
            else if (c === 'motion') {
                ws.send(buildPacket(TYPE.MOTION, parts.slice(1).join(' ')));
                console.log('➡ MOTION');
            }
            else if (c === 'dance') {
                ws.send(buildPacket(TYPE.DANCE, parts.slice(1).join(' ')));
                console.log('➡ DANCE');
            }
            else if (c === 'light') {
                try {
                    ws.send(buildLight(parts[1] || '#000000', parts[2] || 1000));
                    console.log(`➡ LIGHT: color=${parts[1] || '#000000'} durationMs=${parts[2] || 1000}`);
                } catch (err) { console.error('Error:', err.message); }
            }
            else { console.log('Unknown command. Try: say, look, light, avatar, motion, dance, ping, quit'); }

            prompt();
        });
        prompt();
    });
} else if (cmd === 'say') {
    const name = args[1] || 'Copilot';
    const text = args.slice(2).join(' ') || 'Hello from Copilot!';
    connect((ws) => {
        ws.send(buildTextMessage(name, text));
        console.log(`➡ Sent: [${name}] ${text}`);
        setTimeout(() => { ws.close(); process.exit(0); }, 1000);
    });
} else if (cmd === 'look') {
    connect((ws) => {
        ws.send(buildMotion(args[1] || 0, args[2] || 0));
        console.log(`➡ Look: yaw=${args[1]||0} pitch=${args[2]||0}`);
        setTimeout(() => { ws.close(); process.exit(0); }, 1000);
    });
} else if (cmd === 'light') {
    connect((ws) => {
        try {
            ws.send(buildLight(args[1] || '#000000', args[2] || 1000));
            console.log(`➡ Light: color=${args[1] || '#000000'} durationMs=${args[2] || 1000}`);
        } catch (err) {
            console.error('Error:', err.message);
            ws.close();
            process.exit(1);
        }
        setTimeout(() => { ws.close(); process.exit(0); }, 1000);
    });
} else if (cmd === 'ping') {
    connect((ws) => {
        ws.send(buildPacket(TYPE.PING));
        console.log('➡ PING');
        setTimeout(() => { ws.close(); process.exit(0); }, 2000);
    });
} else {
    console.log('Usage: node ws_control.js [say|look|light|ping|interactive] [args...]');
}
