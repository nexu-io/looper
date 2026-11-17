import { randomUUID } from 'node:crypto';
import { chmod, mkdir, unlink } from 'node:fs/promises';
import { createServer, request } from 'node:http';
import { dirname } from 'node:path';
import { createInterface } from 'node:readline';
import { startBridge } from './bot/bridge';
import type { ExternalTask, TurnEvent, HumanEvent } from './bot/external';
import type { AppConfig } from './config/schema';
import { getProjectByChatId, addProject, listProjects } from './project/registry';

interface Init {
  socket: string;
  callbackSocket: string;
  cwd: string;
  appId: string;
  appSecret: string;
  ownerOpenId: string;
  tenant?: 'feishu' | 'lark';
  concurrency?: number;
  externalEvents?: boolean;
}

// Credentials cross an inherited pipe, never argv, logs, or saved bridge config.
const input = createInterface({ input: process.stdin });
const init = await new Promise<Init>((resolve, reject) => {
  input.once('line', (line) => {
    try { resolve(JSON.parse(line) as Init); } catch { reject(new Error('invalid bridge initialization')); }
  });
  input.once('close', () => reject(new Error('host closed before initialization')));
});

const turns = new Map<string, string>();
async function report(event: TurnEvent): Promise<void> {
  const key = `${event.chatId}:${event.rootMessageId}`;
  if (event.phase === 'started' || !turns.has(key)) turns.set(key, randomUUID());
  const body = JSON.stringify({ ...event, turnId: turns.get(key) });
  await new Promise<void>((resolve, reject) => {
    const req = request({ socketPath: init.callbackSocket, path: '/bridge/events',
      method: 'POST', headers: { 'content-type': 'application/json' } }, (res) => {
      res.resume();
      res.on('end', () => res.statusCode === 200 ? resolve() : reject(new Error(`host rejected turn event: ${res.statusCode}`)));
    });
    req.setTimeout(10_000, () => req.destroy(new Error('host event timeout')));
    req.on('error', reject);
    req.end(body);
  });
  if (event.phase === 'finished' || event.phase === 'failed') turns.delete(key);
}

const projects = await listProjects();
const cfg: AppConfig = { accounts: { app: {
  id: init.appId, secret: '', tenant: init.tenant ?? 'feishu',
} }, preferences: {
  messageReply: 'card', showToolCalls: true, maxConcurrentRuns: init.concurrency ?? 4,
  pendingPolicy: 'steer', requireMentionInGroup: true,
  access: { ownerOpenId: init.ownerOpenId, allowedChats: projects.length ? projects.map((p) => p.chatId) : ['__no_tasks_yet__'] },
  cliBridge: { enabled: false },
} };
// startBridge receives the secret directly; agent children must not inherit it.
const bridge = await startBridge({ cfg, appSecret: init.appSecret, fallbackCwd: init.cwd,
  transport: init.externalEvents ? 'webhook' : 'websocket',
  externalHooks: { onTurn: report, onHuman: reportHuman } });
init.appSecret = '';

async function reportHuman(event: HumanEvent): Promise<void> {
  if (!Number.isFinite(event.at) || event.at <= 0) throw new Error('missing source message time');
  let messageUrl = '';
  try {
    const response = await bridge.channel.rawClient.im.v1.message.get({ path: { message_id: event.messageId } });
    const message = response.data?.items?.[0] as { message_position?: string } | undefined;
    if (message?.message_position != null && /^\d+$/.test(String(message.message_position))) {
      messageUrl = `https://applink.feishu.cn/client/chat/open?${new URLSearchParams({
        openChatId: event.chatId, position: String(message.message_position),
      })}`;
    }
  } catch { /* Keep the observed identity even when Feishu cannot provide a link. */ }
  const body = JSON.stringify({ ...event, at: new Date(event.at).toISOString(), messageUrl });
  await new Promise<void>((resolve, reject) => {
    const req = request({ socketPath: init.callbackSocket, path: '/bridge/human', method: 'POST',
      headers: { 'content-type': 'application/json' } }, (res) => {
      res.resume();
      res.on('end', () => res.statusCode === 200 ? resolve() : reject(new Error(`host rejected human metadata: ${res.statusCode}`)));
    });
    req.setTimeout(10_000, () => req.destroy(new Error('host observation timeout')));
    req.on('error', reject);
    req.end(body);
  });
}

async function readBody(req: import('node:http').IncomingMessage): Promise<unknown> {
  const chunks: Buffer[] = [];
  let size = 0;
  for await (const chunk of req) {
    size += chunk.length;
    if (size > 2 * 1024 * 1024) throw new Error('request too large');
    chunks.push(chunk);
  }
  return JSON.parse(Buffer.concat(chunks).toString('utf8'));
}

const server = createServer(async (req, res) => {
  res.setHeader('content-type', 'application/json');
  try {
    if (req.method === 'GET' && req.url === '/health') {
      res.end(JSON.stringify({ ready: true }));
      return;
    }
    if (req.method === 'POST' && req.url === '/tasks') {
      const task = await readBody(req) as ExternalTask;
      if (!task.chatId || !task.rootMessageId || !task.prompt || !task.cwd) throw new Error('incomplete task');
      if (!(await getProjectByChatId(task.chatId))) {
        await addProject({ name: task.chatId, chatId: task.chatId, cwd: init.cwd,
          blank: false, createdAt: Date.now(), noMention: false, mode: 'full' });
      }
      const chats = cfg.preferences!.access!.allowedChats!;
      if (!chats.includes(task.chatId)) chats.push(task.chatId);
      void bridge.submitTask(task).catch((err) => console.error('dispatch failed:', err instanceof Error ? err.message : 'unknown error'));
      res.end(JSON.stringify({ accepted: true }));
      return;
    }
    if (req.method === 'POST' && req.url === '/inbound' && init.externalEvents) {
      await bridge.dispatchEvent(await readBody(req));
      res.end(JSON.stringify({ accepted: true }));
      return;
    }
    if (req.method === 'POST' && req.url === '/shutdown') {
      res.end(JSON.stringify({ stopped: true }));
      void shutdown();
      return;
    }
    if (req.method === 'POST' && req.url === '/stop-task') {
      const body = await readBody(req) as { threadId: string };
      res.end(JSON.stringify({ stopped: bridge.stopTask(body.threadId) }));
      return;
    }
    res.statusCode = 404;
    res.end(JSON.stringify({ error: 'not found' }));
  } catch (err) {
    res.statusCode = 400;
    res.end(JSON.stringify({ error: err instanceof Error ? err.message : 'invalid request' }));
  }
});
await mkdir(dirname(init.socket), { recursive: true, mode: 0o700 });
await unlink(init.socket).catch((err: NodeJS.ErrnoException) => { if (err.code !== 'ENOENT') throw err; });
await new Promise<void>((resolve, reject) => {
  server.once('error', reject);
  server.listen(init.socket, () => resolve());
});
await chmod(init.socket, 0o600);

let stopping = false;
async function shutdown(): Promise<void> {
  if (stopping) return;
  stopping = true;
  await bridge.shutdown();
  server.close();
  input.close();
  await unlink(init.socket).catch(() => undefined);
  process.exit(0);
}
// The host's inherited pipe closes on a host crash too. No persisted-PID kill.
process.stdin.once('end', () => void shutdown());
process.once('SIGTERM', () => void shutdown());
process.once('SIGINT', () => void shutdown());

// EOF may have arrived while startBridge was still connecting.
if (process.stdin.readableEnded) void shutdown();
