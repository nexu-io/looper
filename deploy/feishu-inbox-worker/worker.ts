// Restored from Looper c53c6bf; keeps its non-destructive inbox contract.
interface Row {
  id: number; received_at: string; kind: string; chat_id: string;
  root_id: string; thread_id: string; sender_open_id: string;
  text: string; value_json: string; raw_json: string | null;
}
interface Statement {
  bind(...values: unknown[]): Statement;
  run(): Promise<unknown>;
  all(): Promise<{ results: Row[] }>;
}
export interface Env {
  DB: { prepare(sql: string): Statement };
  FEISHU_APP_ID: string;
  FEISHU_VERIFY_TOKEN: string;
  FEISHU_ENCRYPT_KEY?: string;
  POLL_TOKEN: string;
  PLANE_BASE_URL?: string;
  PRESENCE_TOKEN?: string;
  PATH_PREFIX?: string;
}

// Feishu → looper event inbox (Cloudflare Worker).
//
// A dumb, durable inbox that lets a SHARED Feishu app deliver inbound events
// (a human's reply typed in a HITL ask thread, or a card-action button click) to
// MANY local looper daemons behind NAT — without each teammate running their own
// app or tunnel. Feishu's servers POST here; each looper POLLS here and keeps
// only the events whose thread it owns (matched against its local feishu_threads).
//
// Endpoints:
//   POST /            Feishu event/card callback. Verifies the app Verification
//                     Token, answers the url_verification handshake, and stores
//                     im.message.receive_v1 / card.action.trigger events.
//   GET  /events?since=<id>&limit=<n>
//                     Returns stored events with id > since (default 0), oldest
//                     first. Requires Authorization: Bearer <POLL_TOKEN>.
//
// Bindings (wrangler.toml):
//   DB              D1 database (schema in schema.sql)
// Secrets (wrangler secret put ...):
//   FEISHU_VERIFY_TOKEN   the shared app's Event Subscription Verification Token
//   POLL_TOKEN            shared secret loopers present to read /events
//
// Encrypted callbacks are supported when FEISHU_ENCRYPT_KEY is configured.

const MAX_LIMIT = 200;

export default {
  async fetch(request: Request, env: Env): Promise<Response> {
    const url = new URL(request.url);
    const prefix = env.PATH_PREFIX || "";
    if (prefix && !url.pathname.startsWith(prefix + "/")) return new Response("not found", { status: 404 });
    const path = url.pathname.slice(prefix.length);
    if (request.method === "POST" && path === "/") {
      return handleFeishuCallback(request, env);
    }
    if (request.method === "GET" && path === "/events") {
      return handlePoll(request, env, url);
    }
    if (request.method === "GET" && path === "/healthz") {
      return json({ ok: true });
    }
    if (request.method === "POST" && path === "/devices/heartbeat" && !url.search) {
      return handleHeartbeat(request, env);
    }
    if (request.method === "GET" && /^\/devices\/[0-9a-f-]{36}$/.test(path)) {
      return handlePresence(request, env, path.slice("/devices/".length));
    }
    return new Response("not found", { status: 404 });
  },
};

async function handleHeartbeat(request: Request, env: Env): Promise<Response> {
  if (!request.headers.get("Looper-Signature")) return json({ error: "unauthorized" }, 401);
  let origin: URL;
  try {
    origin = new URL(env.PLANE_BASE_URL || "");
    const local = origin.protocol === "http:" && ["127.0.0.1", "localhost"].includes(origin.hostname);
    if ((!local && origin.protocol !== "https:") || origin.username || origin.password || origin.search || origin.hash || origin.pathname !== "/") throw new Error("invalid origin");
  } catch {
    return json({ error: "plane_not_configured" }, 503);
  }
  const body = await request.text();
  if (body.length > 1024) return json({ error: "request_too_large" }, 413);
  let result: Response;
  try {
    result = await fetch(new URL("/api/looper/devices/heartbeat/", origin), {
      method: "POST", body, redirect: "manual", signal: AbortSignal.timeout(8000),
      headers: { "Content-Type": "application/json", "Looper-Signature": request.headers.get("Looper-Signature")! },
    });
    if (!result.ok) return json({ error: "device_not_verified" }, result.status < 500 ? 401 : 503);
    const identity: any = await result.json();
    if (!/^[0-9a-f-]{36}$/.test(identity.device_id) || typeof identity.node_id !== "string" || !identity.node_id) throw new Error("invalid identity");
    const now = new Date().toISOString();
    await env.DB.prepare(`INSERT INTO devices (device_id, node_id, last_heartbeat_at) VALUES (?, ?, ?)
      ON CONFLICT(device_id) DO UPDATE SET node_id=excluded.node_id,
      last_heartbeat_at=MAX(devices.last_heartbeat_at, excluded.last_heartbeat_at)`)
      .bind(identity.device_id, identity.node_id, now).run();
    return json({ ok: true });
  } catch (error) {
    console.warn("Device heartbeat could not be recorded", error instanceof Error ? error.message : "unknown error");
    return json({ error: "heartbeat_unavailable" }, 503);
  }
}

async function handlePresence(request: Request, env: Env, id: string): Promise<Response> {
  const token = (request.headers.get("Authorization") || "").replace(/^Bearer\s+/i, "").trim();
  if (!env.PRESENCE_TOKEN || !timingSafeEqual(token, env.PRESENCE_TOKEN)) return json({ error: "unauthorized" }, 401);
  const rows = await env.DB.prepare("SELECT device_id, node_id, last_heartbeat_at FROM devices WHERE device_id=?").bind(id).all();
  return json(rows.results?.[0] || { device_id: id, node_id: null, last_heartbeat_at: null });
}

async function handleFeishuCallback(request: Request, env: Env): Promise<Response> {
  let body: any;
  try {
    body = await request.json();
    if (body.encrypt && env.FEISHU_ENCRYPT_KEY) body = await decryptEvent(body.encrypt, env.FEISHU_ENCRYPT_KEY);
    if (!body || typeof body !== "object") throw new Error("invalid event");
  } catch {
    return json({ code: 1, msg: "invalid json" }, 400);
  }

  if (body && body.encrypt) {
    // Fail explicitly rather than acknowledging an unreadable event.
    return json({ code: 1, msg: "configure FEISHU_ENCRYPT_KEY for encrypted callbacks" }, 400);
  }

  // The Verification Token proves the request came from Feishu. v1 handshake +
  // card actions carry it at the top level; v2 events carry it in header.token.
  const presented = (body.token || (body.header && body.header.token) || "").trim();
  const expected = (env.FEISHU_VERIFY_TOKEN || "").trim();
  const tokenOK = expected !== "" && timingSafeEqual(presented, expected);

  // url_verification handshake.
  if (body.type === "url_verification") {
    if (!tokenOK) return json({ code: 1, msg: "token mismatch" }, 401);
    return json({ challenge: body.challenge });
  }
  if (!tokenOK) return json({ code: 1, msg: "token mismatch" }, 401);

  if (body.header?.app_id !== env.FEISHU_APP_ID) return json({ code: 1, msg: "app mismatch" }, 403);
  const evt = normalizeEvent(body);
  if (!evt) return json({ code: 0, msg: "ignored" }); // not an event we relay

  // Verification material stays at the receiver, not in every local inbox copy.
  const raw = structuredClone(body);
  delete raw.token;
  if (raw.header) delete raw.header.token;
  await env.DB.prepare(
    `INSERT OR IGNORE INTO events (received_at, kind, chat_id, root_id, thread_id, sender_open_id, text, value_json, event_id, raw_json)
     VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
  )
    .bind(
      new Date().toISOString(),
      evt.kind,
      evt.chatId,
      evt.rootId,
      evt.threadId,
      evt.senderOpenId,
      evt.text,
      evt.valueJson,
      body.header?.event_id || null,
      JSON.stringify(raw)
    )
    .run();

  // Acknowledge receipt; the owning local instance may still be offline.
  if (evt.kind === "card_action") {
    return json({ toast: { type: "info", content: "已收到，等待本地处理" } });
  }
  return json({ code: 0, msg: "ok" });
}

// normalizeEvent maps a Feishu callback into the inbox row shape, or null when it
// is not something loopers consume (message replies + card actions only).
function normalizeEvent(body: any) {
  const eventType = body.header && body.header.event_type;
  if (eventType === "im.message.reaction.created_v1" || eventType === "im.message.reaction.deleted_v1") {
    const ev = body.event || {};
    if (!ev.message_id) return null;
    return { kind: "reaction", chatId: "", rootId: ev.message_id, threadId: "",
      senderOpenId: ev.user_id?.open_id || "", text: "", valueJson: "" };
  }
  if (eventType === "im.message.receive_v1") {
    const m = (body.event && body.event.message) || {};

    let text = "";
    try {
      text = (JSON.parse(m.content || "{}").text || "").trim();
    } catch {}
    if (!m.message_id) return null;
    const sender = (body.event && body.event.sender && body.event.sender.sender_id) || {};
    return {
      kind: "message",
      chatId: m.chat_id || "",
      rootId: (m.root_id || m.thread_id || "").trim(),
      threadId: (m.thread_id || "").trim(),
      senderOpenId: sender.open_id || "",
      text,
      valueJson: "",
    };
  }
  // Card action button click, v2 shape (card.action.trigger): everything is nested
  // under `event` — action.value + context.open_message_id + operator.open_id.
  if (eventType === "card.action.trigger") {
    const ev = body.event || {};
    const action = ev.action || {};
    if (action.value === undefined || action.value === null) return null;
    const ctx = ev.context || {};
    const operator = ev.operator || {};
    return {
      kind: "card_action",
      chatId: (ctx.open_chat_id || "").trim(),
      rootId: (ctx.open_message_id || "").trim(),
      threadId: "",
      senderOpenId: (operator.open_id || operator.union_id || "").trim(),
      text: "",
      valueJson: JSON.stringify(action.value),
    };
  }
  return null;
}

async function handlePoll(request: Request, env: Env, url: URL): Promise<Response> {
  const auth = request.headers.get("Authorization") || "";
  const token = auth.replace(/^Bearer\s+/i, "").trim();
  const expected = (env.POLL_TOKEN || "").trim();
  if (expected === "" || !timingSafeEqual(token, expected)) {
    return json({ ok: false, error: "unauthorized" }, 401);
  }
  const since = parseInt(url.searchParams.get("since") || "0", 10) || 0;
  let limit = parseInt(url.searchParams.get("limit") || "100", 10) || 100;
  limit = Math.max(1, Math.min(limit, MAX_LIMIT));

  const rows = await env.DB.prepare(
    `SELECT id, received_at, kind, chat_id, root_id, thread_id, sender_open_id, text, value_json, raw_json
     FROM events WHERE id > ? ORDER BY id ASC LIMIT ?`
  )
    .bind(since, limit)
    .all();

  const events = (rows.results || []).map((r) => ({
    id: r.id,
    receivedAt: r.received_at,
    kind: r.kind,
    chatId: r.chat_id,
    rootId: r.root_id,
    threadId: r.thread_id,
    senderOpenId: r.sender_open_id,
    text: r.text,
    value: r.value_json ? safeParse(r.value_json) : null,
    raw: r.raw_json ? safeParse(r.raw_json) : null,
  }));
  const cursor = events.length ? events[events.length - 1].id : since;
  return json({ ok: true, events, cursor });
}

function json(obj: unknown, status = 200): Response {
  return new Response(JSON.stringify(obj), {
    status,
    headers: { "content-type": "application/json; charset=utf-8" },
  });
}

function safeParse(s: string): unknown {
  try {
    return JSON.parse(s);
  } catch {
    return null;
  }
}

// timingSafeEqual compares two short strings without early-exit on mismatch.
function timingSafeEqual(a: string, b: string): boolean {
  if (a.length !== b.length) return false;
  let diff = 0;
  for (let i = 0; i < a.length; i++) diff |= a.charCodeAt(i) ^ b.charCodeAt(i);
  return diff === 0;
}

async function decryptEvent(encoded: string, secret: string): Promise<unknown> {
  const bytes = Uint8Array.from(atob(encoded), (c) => c.charCodeAt(0));
  const digest = await crypto.subtle.digest('SHA-256', new TextEncoder().encode(secret));
  const key = await crypto.subtle.importKey('raw', digest, 'AES-CBC', false, ['decrypt']);
  const plain = await crypto.subtle.decrypt({ name: 'AES-CBC', iv: bytes.slice(0, 16) }, key, bytes.slice(16));
  return JSON.parse(new TextDecoder().decode(plain));
}
