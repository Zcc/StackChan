#!/usr/bin/env node
import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import WsWebSocket from 'ws';

const __filename = fileURLToPath(import.meta.url);
const __dirname = path.dirname(__filename);
const repoRoot = path.resolve(__dirname, '../../..');
loadEnv(process.env.HOMEAGENT_ENV_FILE || path.join(repoRoot, 'tools/.env.local'));

const bridgeURL = trimTrailingSlash(process.env.HOMEAGENT_BRIDGE_URL || process.env.STACKCHAN_BRIDGE_URL || 'http://127.0.0.1:8790');
const bridgeToken = process.env.HOMEAGENT_BRIDGE_TOKEN || process.env.STACKCHAN_BRIDGE_TOKEN || process.env.BRIDGE_TOKEN || '';
const retryCount = parseInt(process.env.HOMEAGENT_SKILL_RETRIES || '5', 10);
const retryDelayMs = parseInt(process.env.HOMEAGENT_SKILL_RETRY_DELAY_MS || '750', 10);

async function main() {
  const args = process.argv.slice(2);
  const command = args.shift() || 'help';

  switch (command) {
    case 'status':
      return printJSON(await requestJSON('GET', '/status'));
    case 'say':
      return say(args);
    case 'look':
      return look(args);
    case 'light':
      return light(args);
    case 'light-off':
      return light(['#000000', '--duration', '500']);
    case 'marquee':
      return marquee(args);
    case 'photo':
      return photo('/snapshot', args);
    case 'latest-photo':
      return photo('/snapshot/latest', args, 'GET');
    case 'camera-start':
      return printJSON(await requestJSON('POST', '/camera/start'));
    case 'camera-stop':
      return printJSON(await requestJSON('POST', '/camera/stop'));
    case 'mic-listen':
      return process.exitCode = await micListen(args);
    case 'help':
    case '--help':
    case '-h':
      return help();
    default:
      throw new Error(`unknown command: ${command}`);
  }
}

async function say(args) {
  const name = readOption(args, '--name') || 'HomeAgent';
  let content = readOption(args, '--content');
  if (!content && args.includes('--stdin')) {
    args.splice(args.indexOf('--stdin'), 1);
    content = fs.readFileSync(0, 'utf8');
  }
  if (!content) content = args.join(' ');
  content = content.trim();
  if (!content) throw new Error('say requires message text (positional, --content, or --stdin)');
  return printJSON(await requestJSON('POST', '/say', { name, content }));
}

async function look(args) {
  const yaw = numberOption(args, '--yaw', args[0] || 0);
  const pitch = numberOption(args, '--pitch', args[1] || 0);
  const speed = numberOption(args, '--speed', args[2] || 500);
  return printJSON(await requestJSON('POST', '/look', { yaw, pitch, speed }));
}

async function light(args) {
  const color = normalizeHexColor(args[0] || '#000000');
  const durationMs = numberOption(args, '--duration', readOption(args, '--durationMs') || args[1] || 1000);
  return printJSON(await requestJSON('POST', '/light', { color, durationMs }));
}

async function marquee(args) {
  const color = parseHexRgb(readOption(args, '--color') || '#FF5000');
  const bgRaw = readOption(args, '--bg');
  const bg = bgRaw ? parseHexRgb(bgRaw) : { r: 0, g: 0, b: 0 };
  const count = Math.max(1, Math.floor(numberOption(args, '--count', readOption(args, '--leds') || 12)));
  const speedMs = Math.max(10, Math.floor(numberOption(args, '--speed-ms', readOption(args, '--speed') || 80)));
  const cycles = Math.max(1, Math.floor(numberOption(args, '--cycles', 3)));
  const tail = Math.max(0, Math.floor(numberOption(args, '--tail', 0)));
  const reverse = args.includes('--reverse');
  if (reverse) args.splice(args.indexOf('--reverse'), 1);
  const bounce = args.includes('--bounce');
  if (bounce) args.splice(args.indexOf('--bounce'), 1);
  const keepOn = args.includes('--keep-on');
  if (keepOn) args.splice(args.indexOf('--keep-on'), 1);

  const positions = [];
  for (let c = 0; c < cycles; c++) {
    if (bounce) {
      for (let i = 0; i < count; i++) positions.push(reverse ? count - 1 - i : i);
      for (let i = count - 2; i > 0; i--) positions.push(reverse ? count - 1 - i : i);
    } else {
      for (let i = 0; i < count; i++) positions.push(reverse ? count - 1 - i : i);
    }
  }

  for (const head of positions) {
    const leds = [];
    for (let i = 0; i < count; i++) {
      let r = bg.r, g = bg.g, b = bg.b;
      const dist = Math.abs(i - head);
      if (i === head) {
        ({ r, g, b } = color);
      } else if (tail > 0 && dist <= tail) {
        const factor = 1 - dist / (tail + 1);
        r = Math.round(bg.r + (color.r - bg.r) * factor);
        g = Math.round(bg.g + (color.g - bg.g) * factor);
        b = Math.round(bg.b + (color.b - bg.b) * factor);
      }
      leds.push({ i, r, g, b });
    }
    await requestJSON('POST', '/rgb', { leds });
    await delay(speedMs);
  }

  if (!keepOn) {
    const offLeds = [];
    for (let i = 0; i < count; i++) offLeds.push({ i, r: 0, g: 0, b: 0 });
    await requestJSON('POST', '/rgb', { leds: offLeds });
  }

  return printJSON({ ok: true, frames: positions.length, count, cycles, speedMs });
}

function parseHexRgb(value) {
  const normalized = normalizeHexColor(value).slice(1);
  return {
    r: parseInt(normalized.slice(0, 2), 16),
    g: parseInt(normalized.slice(2, 4), 16),
    b: parseInt(normalized.slice(4, 6), 16),
  };
}

async function photo(endpoint, args, method = 'POST') {
  const out = readOption(args, '--out') || defaultSnapshotPath(endpoint.includes('latest') ? 'latest' : 'snapshot');
  const data = await requestBuffer(method, endpoint);
  fs.writeFileSync(out, data);
  return printJSON({ ok: true, path: out, bytes: data.length, mimeType: 'image/jpeg' });
}

async function micListen(args) {
  const out = readOption(args, '--out') || '';
  const durationArg = readOption(args, '--duration-ms') || readOption(args, '--durationMs') || '';
  const durationMs = durationArg ? numberOptionValue(durationArg, '--duration-ms') : undefined;
  return runMicListen({ bridge: bridgeURL, token: bridgeToken, out, durationMs });
}

export async function runMicListen({ bridge = bridgeURL, token = bridgeToken, out = '', durationMs } = {}) {
  const base = trimTrailingSlash(bridge);
  const headers = token ? { Authorization: `Bearer ${token}` } : {};
  const body = {};
  if (durationMs !== undefined) body.duration_ms = durationMs;

  const startRes = await fetch(`${base}/mic/start`, {
    method: 'POST',
    headers: { ...headers, 'Content-Type': 'application/json' },
    body: JSON.stringify(body),
  });
  if (!startRes.ok) return 2;

  const file = out ? fs.createWriteStream(out) : null;
  let ws;
  const sigintHandler = () => {
    closeWebSocket(ws);
  };
  process.once('SIGINT', sigintHandler);

  try {
    ws = createMicWebSocket(`${httpToWs(base)}/mic/ws`, headers);
    await consumeMicWebSocket(ws, (data) => {
      if (data.length < 16) return;
      const payload = data.subarray(16);
      if (file) file.write(payload);
    });
  } finally {
    process.removeListener('SIGINT', sigintHandler);
    if (file) await new Promise((resolve, reject) => file.end((err) => (err ? reject(err) : resolve())));
    await fetch(`${base}/mic/stop`, { method: 'POST', headers }).catch(() => {});
  }

  return 0;
}

function createMicWebSocket(url, headers) {
  if (Object.keys(headers).length === 0 && typeof globalThis.WebSocket === 'function') {
    return new globalThis.WebSocket(url);
  }
  return new WsWebSocket(url, { headers });
}

function consumeMicWebSocket(ws, onBinary) {
  if (typeof ws.on === 'function') {
    return new Promise((resolve, reject) => {
      ws.on('message', (data, isBinary) => {
        if (!isBinary) return;
        onBinary(Buffer.from(data));
      });
      ws.on('close', resolve);
      ws.on('error', reject);
    });
  }

  return new Promise((resolve, reject) => {
    ws.binaryType = 'arraybuffer';
    ws.addEventListener('message', (event) => {
      if (typeof event.data === 'string') return;
      onBinary(Buffer.from(event.data));
    });
    ws.addEventListener('close', resolve, { once: true });
    ws.addEventListener('error', reject, { once: true });
  });
}

function closeWebSocket(ws) {
  if (!ws) return;
  if (typeof ws.close === 'function') ws.close();
}

function httpToWs(value) {
  return value.replace(/^http:/, 'ws:').replace(/^https:/, 'wss:');
}

async function requestJSON(method, endpoint, body) {
  return withRelayRetry(async () => {
    const res = await request(method, endpoint, body ? JSON.stringify(body) : undefined, body ? 'application/json' : undefined);
    const text = await res.text();
    if (!res.ok) throw httpError(method, endpoint, res.status, text);
    try {
      return text ? JSON.parse(text) : { ok: true };
    } catch {
      return { ok: true, text };
    }
  });
}

async function requestBuffer(method, endpoint) {
  return withRelayRetry(async () => {
    const res = await request(method, endpoint);
    const arrayBuffer = await res.arrayBuffer();
    const data = Buffer.from(arrayBuffer);
    if (!res.ok) throw httpError(method, endpoint, res.status, data.toString('utf8'));
    return data;
  });
}

async function request(method, endpoint, body, contentType) {
  if (typeof fetch !== 'function') {
    throw new Error('global fetch is unavailable; use Node.js 18 or newer');
  }
  const headers = {};
  if (bridgeToken) headers.Authorization = `Bearer ${bridgeToken}`;
  if (contentType) headers['Content-Type'] = contentType;
  return fetch(`${bridgeURL}${endpoint}`, { method, headers, body });
}

async function withRelayRetry(fn) {
  let lastErr;
  const attempts = Number.isFinite(retryCount) && retryCount > 0 ? retryCount : 1;
  for (let attempt = 1; attempt <= attempts; attempt++) {
    try {
      return await fn();
    } catch (err) {
      lastErr = err;
      if (!isTransientBridgeError(err) || attempt === attempts) break;
      await delay(retryDelayMs);
    }
  }
  throw lastErr;
}

function httpError(method, endpoint, status, text) {
  const err = new Error(`${method} ${endpoint} failed: HTTP ${status}: ${text}`);
  err.status = status;
  err.body = text;
  return err;
}

function isTransientBridgeError(err) {
  return err && (err.status === 503 || /bridge is not connected to relay|fetch failed|ECONNREFUSED/.test(err.message));
}

function delay(ms) {
  return new Promise((resolve) => setTimeout(resolve, Number.isFinite(ms) && ms > 0 ? ms : 750));
}

function readOption(args, name) {
  const index = args.indexOf(name);
  if (index === -1) return '';
  const value = args[index + 1] || '';
  args.splice(index, 2);
  return value;
}

function numberOption(args, name, fallback) {
  return numberOptionValue(readOption(args, name) || fallback, name);
}

function numberOptionValue(value, name) {
  const parsed = Number(value);
  if (!Number.isFinite(parsed)) throw new Error(`${name} must be a number`);
  return parsed;
}

function normalizeHexColor(value) {
  if (/^[0-9a-fA-F]{6}$/.test(value)) return `#${value}`;
  if (/^#[0-9a-fA-F]{6}$/.test(value)) return value;
  throw new Error('color must be #RRGGBB');
}

function defaultSnapshotPath(kind) {
  const stamp = new Date().toISOString().replace(/[-:]/g, '').replace(/\..+/, '').replace('T', '_');
  return path.join('/tmp', `stackchan_${kind}_${stamp}.jpg`);
}

function loadEnv(file) {
  if (!fs.existsSync(file)) return;
  for (const line of fs.readFileSync(file, 'utf8').split(/\r?\n/)) {
    const trimmed = line.trim();
    if (!trimmed || trimmed.startsWith('#')) continue;
    const eq = trimmed.indexOf('=');
    if (eq <= 0) continue;
    const key = trimmed.slice(0, eq).trim();
    const value = trimmed.slice(eq + 1).trim().replace(/^(["'])(.*)\1$/, '$2');
    if (!process.env[key]) process.env[key] = value;
  }
}

function trimTrailingSlash(value) {
  return value.replace(/\/+$/, '');
}

function printJSON(value) {
  process.stdout.write(`${JSON.stringify(value, null, 2)}\n`);
}

function help() {
  process.stdout.write(`Usage: stackchan-home-agent <command> [args]\n\nCommands:\n  status\n  say <text> [--name Name]\n  look --yaw <n> --pitch <n> [--speed <n>]\n  light <#RRGGBB> [--duration <ms>]\n  light-off\n  marquee [--color #RRGGBB] [--bg #RRGGBB] [--count 12] [--speed-ms 80] [--cycles 3] [--tail 0] [--reverse] [--bounce] [--keep-on]\n  photo [--out path]\n  latest-photo [--out path]\n  camera-start\n  camera-stop\n  mic-listen [--out path] [--duration-ms ms]\n`);
}

if (process.argv[1] && path.resolve(process.argv[1]) === __filename) {
  main().catch((err) => {
    process.stderr.write(`${JSON.stringify({ ok: false, error: err.message })}\n`);
    process.exit(1);
  });
}
