import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { NormalizedMessage } from '@larksuiteoapi/node-sdk';
import type { AgentEvent, AgentInput, TurnOptions } from '../src/agent/types';

const fake = vi.hoisted(() => ({
  record: undefined as any,
  backend: { capabilities: { steer: true }, id: 'codex', listModels: vi.fn(async () => []), resumeThread: vi.fn(), startThread: vi.fn() },
  final: vi.fn(async () => true),
  createCard: vi.fn(async () => 'card'),
  send: vi.fn(async () => ({})),
  log: { info: vi.fn(), warn: vi.fn(), fail: vi.fn() },
}));
vi.mock('../src/core/logger', () => ({ log: fake.log, withTrace: (_ctx: unknown, fn: () => unknown) => fn() }));
vi.mock('../src/agent', async (original) => ({ ...await original<object>(), createBackend: () => fake.backend }));
vi.mock('../src/project/registry', async (original) => ({
  ...await original<object>(),
  getProjectByChatId: async () => ({ name: 'test', chatId: 'chat', cwd: '/test', defaultModel: 'chosen-model', defaultEffort: 'xhigh' }),
}));
vi.mock('../src/bot/session-store', async (original) => ({
  ...await original<object>(),
  getSession: async () => fake.record,
  patchSession: async (_key: string, patch: object) => { Object.assign(fake.record, patch); },
  upsertSession: async (record: object) => { fake.record = record; },
}));
vi.mock('../src/bot/session-title-coordinator', () => ({
  SessionTitleCoordinator: class { startRecovery() {} async register() {} async shutdown() {} },
}));
vi.mock('../src/card/run-card-stream', () => ({
  RunCardStream: class {
    create = fake.createCard;
    getCardId() { return 'card_entity'; }
    async updateElement() { return true; }
    streamCoalesced() {}
    async drain() {}
    setImageWorker() {}
    async settleImages() { return new Map(); }
    updateCard = fake.final;
    finalizeCard = fake.final;
    stats() { return { pushCount: 0, cardPushes: 0, elPushes: 0, totalRttMs: 0, maxRttMs: 0 }; }
  },
}));
import { createOrchestrator } from '../src/bot/handle-message';
import type { AppConfig } from '../src/config/schema';

function deferred<T>() {
  let resolve!: (v: T) => void;
  let reject!: (err: Error) => void;
  const promise = new Promise<T>((res, rej) => { resolve = res; reject = rej; });
  return { promise, resolve, reject };
}
function thread() {
  const turns: ReturnType<typeof deferred<void>>[] = [];
  const consumed: AgentInput[] = [];
  const options: (TurnOptions | undefined)[] = [];
  const t = {
    sessionId: 'host', isAlive: () => true,
    close: vi.fn(async () => { for (const turn of turns) turn.resolve(); }),
    abort: vi.fn(async () => undefined),
    steer: vi.fn(async (_input: AgentInput, _id: string): Promise<void> => undefined),
    runStreamed(input: AgentInput, turn?: TurnOptions) {
      consumed.push(input);
      options.push(turn);
      const end = deferred<void>();
      const id = `turn-${turns.push(end)}`;
      return {
        turnId: () => id, // deliberately keep backend ID stale during final-card I/O
        events: (async function* (): AsyncGenerator<AgentEvent> {
          yield { type: 'turn_started', turnId: id };
          await end.promise;
          yield { type: 'done', turnId: id };
        })(),
      };
    },
  };
  return { t, turns, consumed, options };
}
let orchestrator: ReturnType<typeof createOrchestrator>;
const events = vi.fn(async (_event: unknown) => undefined);
let seq = 0;
function message(text: string): NormalizedMessage {
  return { messageId: `msg-${++seq}`, chatId: 'chat', chatType: 'group', threadId: 'topic',
    content: text, senderId: 'owner', senderName: 'Owner', mentionedBot: true, createTime: Date.now(), rawContentType: 'text', resources: [], mentions: [], mentionAll: false };
}
function setup(showModel: 'off' | 'running' | 'always' = 'always') {
  const cfg: AppConfig = { accounts: { app: { id: 'app', secret: 'test', tenant: 'feishu' } }, preferences: { showModel, access: { ownerOpenId: 'owner' }, completionReminder: { mode: 'manual' } } };
  const channel = { send: fake.send, rawClient: { im: { v1: { message: { get: async () => ({ data: { items: [{ thread_id: 'topic' }] } }) }, messageReaction: {
    create: async () => ({ data: {} }), delete: async () => ({}),
  } } } } };
  orchestrator = createOrchestrator(channel as never, cfg, '/test', undefined, {onTurn: events});
  return orchestrator;
}
beforeEach(() => {
  vi.clearAllMocks();
  fake.record = undefined;
  fake.backend.listModels.mockResolvedValue([{ id: 'chosen-model', displayName: 'Chosen', description: '', supportedEfforts: ['medium', 'xhigh'], defaultEffort: 'medium', isDefault: true, hidden: false }] as never);
  fake.backend.capabilities.steer = true;
  fake.final.mockReset().mockResolvedValue(true);
  fake.createCard.mockReset().mockResolvedValue('card');
  fake.send.mockReset().mockResolvedValue({});
  fake.backend.resumeThread.mockReset();
  fake.backend.startThread.mockReset();
});
afterEach(async () => {
  await orchestrator?.shutdown();
});
const until = (check: () => void) => vi.waitFor(check);

describe('Looper native dispatch seam', () => {
  it('adopts the real topic before completion, queues a follow-up, and keeps one native session', async () => {
    const run = thread();
    fake.backend.startThread.mockResolvedValue(run.t);
    const o = setup();
    const task = {chatId:'chat',rootMessageId:'root',cwd:'/neutral-workspace',title:'Cross-repo investigation',prompt:'Read both repos',requesterOpenId:'owner'};
    const first = o.submitTask(task);
    await until(() => expect(events).toHaveBeenCalledWith(expect.objectContaining({phase:'bound',threadId:'topic',sessionId:'host'})));
    expect(fake.record).toMatchObject({threadId:'topic',cwd:'/neutral-workspace'});
    await o.submitTask({...task,threadId:'topic',prompt:'Also inspect the second repository'});
    run.turns[0]!.resolve();
    await until(() => expect(run.consumed).toHaveLength(2));
    expect(run.consumed[1]!.text).toBe('Also inspect the second repository');
    run.turns[1]!.resolve();
    await first;
    expect(fake.backend.startThread).toHaveBeenCalledTimes(1);
    expect(events.mock.calls.map(([e]) => (e as {phase:string}).phase)).toEqual(['started','bound','finished','started','finished']);
    const next = o.submitTask({...task,threadId:'topic',prompt:'Continue here'});
    await until(() => expect(run.consumed).toHaveLength(3));
    run.turns[2]!.resolve();
    await next;
    expect(fake.backend.startThread).toHaveBeenCalledTimes(1);
  });
});
