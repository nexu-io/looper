import { mkdtempSync, readFileSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { afterAll, expect, it } from 'vitest';
import { BRIDGE_DEVELOPER_INSTRUCTIONS } from '../src/agent/bridge-instructions';
import { CodexAppServerBackend } from '../src/agent/codex-appserver/backend';
import { shutdownResidentClients } from '../src/agent/codex-appserver/client-pool';
import { writeNodeExecutable } from './helpers/node-executable';

const dir = mkdtempSync(join(tmpdir(), 'looper-resume-'));
const requests = join(dir, 'requests.jsonl');
const { bin } = writeNodeExecutable(dir, 'codex', `
const fs = require('node:fs');
require('node:readline').createInterface({ input: process.stdin }).on('line', line => {
  const msg = JSON.parse(line);
  if (typeof msg.id !== 'number') return;
  fs.appendFileSync(${JSON.stringify(requests)}, JSON.stringify({ ...msg, pid: process.pid }) + '\\n');
  const result = msg.method === 'thread/resume' ? { thread: { id: msg.params.threadId } } : {};
  process.stdout.write(JSON.stringify({ id: msg.id, result }) + '\\n');
});
`);

afterAll(async () => {
  await shutdownResidentClients();
  rmSync(dir, { recursive: true, force: true });
});

it('refreshes model-visible bridge guidance on the same resumed native session', async () => {
  const previous = process.env.CODEX_BIN;
  process.env.CODEX_BIN = bin;
  try {
    const thread = await new CodexAppServerBackend().resumeThread({ cwd: dir, sessionId: 'original-session' });
    try {
      expect(thread.sessionId).toBe('original-session');
      const all = readFileSync(requests, 'utf8').trim().split('\n').map(line => JSON.parse(line));
      const pid = all.find(call => call.method === 'thread/resume')?.pid;
      const calls = all.filter(call => call.pid === pid);
      const resume = calls.findIndex(call => call.method === 'thread/resume');
      expect(resume).toBeGreaterThanOrEqual(0);
      expect(calls[resume + 1]).toMatchObject({
        method: 'thread/inject_items',
        params: {
          threadId: 'original-session',
          items: [{ type: 'message', role: 'developer', content: [{ type: 'input_text', text: expect.stringContaining(BRIDGE_DEVELOPER_INSTRUCTIONS) }] }],
        },
      });
      expect(calls.some(call => call.method === 'thread/start' || call.method === 'turn/start')).toBe(false);
    } finally {
      await thread.close();
    }
  } finally {
    if (previous === undefined) delete process.env.CODEX_BIN;
    else process.env.CODEX_BIN = previous;
  }
});
