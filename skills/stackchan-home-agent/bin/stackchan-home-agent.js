#!/usr/bin/env node
const fs = require('fs');
const path = require('path');

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
    case 'photo':
      return photo('/snapshot', args);
    case 'latest-photo':
      return photo('/snapshot/latest', args, 'GET');
    case 'camera-start':
      return printJSON(await requestJSON('POST', '/camera/start'));
    case 'camera-stop':
      return printJSON(await requestJSON('POST', '/camera/stop'));
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
  const content = args.join(' ').trim();
  if (!content) throw new Error('say requires message text');
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

async function photo(endpoint, args, method = 'POST') {
  const out = readOption(args, '--out') || defaultSnapshotPath(endpoint.includes('latest') ? 'latest' : 'snapshot');
  const data = await requestBuffer(method, endpoint);
  fs.writeFileSync(out, data);
  return printJSON({ ok: true, path: out, bytes: data.length, mimeType: 'image/jpeg' });
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
  const value = readOption(args, name) || fallback;
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
  process.stdout.write(`Usage: stackchan-home-agent <command> [args]\n\nCommands:\n  status\n  say <text> [--name Name]\n  look --yaw <n> --pitch <n> [--speed <n>]\n  light <#RRGGBB> [--duration <ms>]\n  light-off\n  photo [--out path]\n  latest-photo [--out path]\n  camera-start\n  camera-stop\n`);
}

main().catch((err) => {
  process.stderr.write(`${JSON.stringify({ ok: false, error: err.message })}\n`);
  process.exit(1);
});
