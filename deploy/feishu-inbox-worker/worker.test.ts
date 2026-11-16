import assert from 'node:assert/strict';
import { DatabaseSync } from 'node:sqlite';
import { readFileSync } from 'node:fs';
import { test } from 'node:test';
import worker, { type Env } from './worker.ts';

function setup() {
  const db = new DatabaseSync(':memory:');
  db.exec(readFileSync(new URL('./schema.sql', import.meta.url), 'utf8'));
  db.exec(readFileSync(new URL('./migrations/0001_raw_events.sql', import.meta.url), 'utf8'));
  const env: Env = { FEISHU_APP_ID: 'app', FEISHU_VERIFY_TOKEN: 'verify', POLL_TOKEN: 'poll', DB: {
    prepare(sql) { return { bind(...values) { return {
      async run() { return db.prepare(sql).run(...values as any[]); },
      async all() { return { results: db.prepare(sql).all(...values as any[]) as any[] }; },
      bind() { throw new Error('already bound'); },
    }; }, run: async () => undefined, all: async () => ({ results: [] }) }; },
  } };
  function callback(body: unknown) {
    return worker.fetch(new Request('https://test/', { method: 'POST', body: JSON.stringify(body) }), env);
  }
  function read(since = 0, token = 'poll') {
    return worker.fetch(new Request(`https://test/events?since=${since}`, { headers: { Authorization: `Bearer ${token}` } }), env);
  }
  return { env, callback, read, db };
}
function event(id: string, messageType = 'text') {
  return { header: { token: 'verify', app_id: 'app', event_id: id, event_type: 'im.message.receive_v1' },
    event: { sender: { sender_id: { open_id: 'person' } }, message: { message_id: `om_${id}`, chat_id: 'chat',
      thread_id: 'topic', root_id: 'root', message_type: messageType,
      content: JSON.stringify(messageType === 'text' ? { text: '@bot reply' } : { image_key: 'image' }),
      mentions: [{ key: '@bot', id: { open_id: 'bot' }, name: 'Looper' }] } } };
}

test('preserves SDK fields and media, deduplicates callbacks, allows independent cursors', async () => {
  const s = setup();
  try {
    assert.equal((await s.read(0, 'wrong')).status, 401);
    assert.equal((await s.callback({ type: 'url_verification', token: 'wrong', challenge: 'x' })).status, 401);
    assert.deepEqual(await (await s.callback({ type: 'url_verification', token: 'verify', challenge: 'x' })).json(), { challenge: 'x' });
    assert.equal((await s.callback(event('one'))).status, 200);
    assert.equal((await s.callback(event('one'))).status, 200);
    assert.equal((await s.callback(event('two', 'image'))).status, 200);
    const a: any = await (await s.read()).json();
    assert.equal(a.events.length, 2);
    assert.equal(a.events[0].raw.event.message.message_id, 'om_one');
    assert.deepEqual(a.events[0].raw.event.message.mentions, event('one').event.message.mentions);
    assert.equal(a.events[1].raw.event.message.message_type, 'image');
    assert.equal(a.events[0].raw.header.token, undefined);
    assert.deepEqual(await (await s.read()).json(), a);
    assert.equal(((await (await s.read(a.cursor)).json()) as any).events.length, 0);
  } finally { s.db.close(); }
});

test('decrypts callbacks before verification and rejects another app', async () => {
  const s = setup();
  try {
    s.env.FEISHU_ENCRYPT_KEY = 'fixture-encryption';
    const digest = await crypto.subtle.digest('SHA-256', new TextEncoder().encode(s.env.FEISHU_ENCRYPT_KEY));
    const key = await crypto.subtle.importKey('raw', digest, 'AES-CBC', false, ['encrypt']);
    const iv = crypto.getRandomValues(new Uint8Array(16));
    const encrypted = await crypto.subtle.encrypt({ name: 'AES-CBC', iv }, key, new TextEncoder().encode(JSON.stringify(event('encrypted'))));
    const bytes = new Uint8Array(16 + encrypted.byteLength); bytes.set(iv); bytes.set(new Uint8Array(encrypted), 16);
    assert.equal((await s.callback({ encrypt: Buffer.from(bytes).toString('base64') })).status, 200);
    assert.equal(((await (await s.read()).json()) as any).events.length, 1);
    const other = event('other'); other.header.app_id = 'other-app';
    assert.equal((await s.callback(other)).status, 403);
  } finally { s.db.close(); }
});
