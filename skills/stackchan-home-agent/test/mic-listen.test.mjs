import { test } from 'node:test';
import assert from 'node:assert/strict';
import { createServer } from 'node:http';
import { mkdir, readFile, rm } from 'node:fs/promises';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { WebSocketServer } from 'ws';
import { runMicListen } from '../bin/stackchan-home-agent.js';

const testDir = dirname(fileURLToPath(import.meta.url));

function listen(server) {
  return new Promise((resolve) => server.listen(0, () => resolve(server.address().port)));
}

function close(server) {
  return new Promise((resolve, reject) => server.close((err) => (err ? reject(err) : resolve())));
}

test('mic-listen writes payload to file and stops on session end', async () => {
  let started = false;
  let stopped = false;
  let startBody = '';
  let startAuth = '';
  let wsAuth = '';
  const out = join(testDir, 'output', `mic-${Date.now()}.opus`);

  const http = createServer((req, res) => {
    if (req.url === '/mic/start' && req.method === 'POST') {
      started = true;
      startAuth = req.headers.authorization ?? '';
      req.setEncoding('utf8');
      req.on('data', (chunk) => {
        startBody += chunk;
      });
      req.on('end', () => {
        res.setHeader('content-type', 'application/json');
        res.end(JSON.stringify({ stream_id: 'x', duration_ms: 1000 }));
      });
    } else if (req.url === '/mic/stop' && req.method === 'POST') {
      stopped = true;
      res.setHeader('content-type', 'application/json');
      res.end('{}');
    } else {
      res.statusCode = 404;
      res.end();
    }
  });
  const wss = new WebSocketServer({ noServer: true });
  http.on('upgrade', (req, sock, head) => {
    if (req.url !== '/mic/ws') {
      sock.destroy();
      return;
    }
    wsAuth = req.headers.authorization ?? '';
    wss.handleUpgrade(req, sock, head, (ws) => {
      ws.send(Buffer.from([1, 2, 3, 4, 5, 6, 7, 8, 0, 0, 0, 0, 0, 0, 0, 0, 0xaa, 0xbb]));
      setTimeout(() => ws.close(), 25);
    });
  });

  await mkdir(dirname(out), { recursive: true });
  const port = await listen(http);
  try {
    const code = await runMicListen({
      bridge: `http://127.0.0.1:${port}`,
      token: 't',
      out,
      durationMs: 1000,
    });

    assert.equal(code, 0);
    assert.equal(started, true);
    assert.equal(stopped, true);
    assert.equal(startAuth, 'Bearer t');
    assert.equal(wsAuth, 'Bearer t');
    assert.deepEqual(JSON.parse(startBody), { duration_ms: 1000 });
    assert.deepEqual([...(await readFile(out))], [0xaa, 0xbb]);
  } finally {
    wss.close();
    await close(http);
    await rm(dirname(out), { recursive: true, force: true });
  }
});

test('mic-listen returns 2 and does not open websocket when start fails', async () => {
  let upgraded = false;
  const http = createServer((req, res) => {
    if (req.url === '/mic/start' && req.method === 'POST') {
      res.statusCode = 409;
      res.end('busy');
    } else if (req.url === '/mic/stop' && req.method === 'POST') {
      assert.fail('mic-listen must not stop a stream that never started');
    } else {
      res.statusCode = 404;
      res.end();
    }
  });
  http.on('upgrade', (req, sock) => {
    upgraded = true;
    sock.destroy();
  });

  const port = await listen(http);
  try {
    const code = await runMicListen({ bridge: `http://127.0.0.1:${port}`, token: '', durationMs: 1000 });
    assert.equal(code, 2);
    assert.equal(upgraded, false);
  } finally {
    await close(http);
  }
});
