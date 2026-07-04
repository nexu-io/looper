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
// Encryption note: turn the app's Event Encrypt Key OFF — this inbox reads plain
// JSON callbacks. (An encrypted `{"encrypt":"…"}` body is rejected.)

const MAX_LIMIT = 200;

export default {
  async fetch(request, env) {
    const url = new URL(request.url);
    if (request.method === "POST" && url.pathname === "/") {
      return handleFeishuCallback(request, env);
    }
    if (request.method === "GET" && url.pathname === "/events") {
      return handlePoll(request, env, url);
    }
    if (request.method === "GET" && url.pathname === "/healthz") {
      return json({ ok: true });
    }
    return new Response("not found", { status: 404 });
  },
};

async function handleFeishuCallback(request, env) {
  let body;
  try {
    body = await request.json();
  } catch {
    return json({ code: 1, msg: "invalid json" }, 400);
  }

  if (body && body.encrypt) {
    // Encrypt Key is on — we can't read it. Fail loudly so setup is corrected.
    return json({ code: 1, msg: "event encryption must be disabled" }, 400);
  }

  // The Verification Token proves the request came from Feishu. v1 handshake +
  // card actions carry it at the top level; v2 events carry it in header.token.
  const presented = (body.token || (body.header && body.header.token) || "").trim();
  const expected = (env.FEISHU_VERIFY_TOKEN || "").trim();
  const tokenOK = expected !== "" && timingSafeEqual(presented, expected);

  // url_verification handshake.
  if (body.type === "url_verification") {
    if (expected !== "" && !tokenOK) return json({ code: 1, msg: "token mismatch" }, 401);
    return json({ challenge: body.challenge });
  }
  if (!tokenOK) return json({ code: 1, msg: "token mismatch" }, 401);

  const evt = normalizeEvent(body);
  if (!evt) return json({ code: 0, msg: "ignored" }); // not an event we relay

  await env.DB.prepare(
    `INSERT INTO events (received_at, kind, chat_id, root_id, thread_id, sender_open_id, text, value_json)
     VALUES (?, ?, ?, ?, ?, ?, ?, ?)`
  )
    .bind(
      new Date().toISOString(),
      evt.kind,
      evt.chatId,
      evt.rootId,
      evt.threadId,
      evt.senderOpenId,
      evt.text,
      evt.valueJson
    )
    .run();

  // card.action.trigger expects a card-action response; a toast gives the clicker
  // immediate feedback. Plain events just get an ack.
  if (evt.kind === "card_action") {
    // Just acknowledge with a toast — leave the ask card's full content intact
    // (question + research + options) for later review. Looper patches that card
    // in place on delivery to mark the chosen option, keeping everything else.
    let answer = "";
    try {
      answer = (JSON.parse(evt.valueJson || "{}").answer || "").trim();
    } catch {}
    return json({ toast: { type: "success", content: answer ? "已选:" + answer : "已收到" } });
  }
  return json({ code: 0, msg: "ok" });
}

// normalizeEvent maps a Feishu callback into the inbox row shape, or null when it
// is not something loopers consume (message replies + card actions only).
function normalizeEvent(body) {
  const eventType = body.header && body.header.event_type;
  if (eventType === "im.message.receive_v1") {
    const m = (body.event && body.event.message) || {};
    if (m.message_type !== "text") return null;
    let text = "";
    try {
      text = (JSON.parse(m.content || "{}").text || "").trim();
    } catch {}
    if (!text) return null;
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
  // Card action button click, legacy shape (card.action.trigger_v1): the older one
  // carries open_chat_id/open_message_id at the top level; some variants nest them
  // under `context`. Accept both so chat/root metadata survives.
  if (body.action && body.action.value) {
    const ctx = body.context || {};
    return {
      kind: "card_action",
      chatId: (body.open_chat_id || ctx.open_chat_id || "").trim(),
      rootId: (body.open_message_id || ctx.open_message_id || "").trim(),
      threadId: "",
      senderOpenId: (body.open_id || (body.operator && body.operator.open_id) || "").trim(),
      text: "",
      valueJson: JSON.stringify(body.action.value),
    };
  }
  return null;
}

async function handlePoll(request, env, url) {
  const auth = request.headers.get("Authorization") || "";
  const token = auth.replace(/^Bearer\s+/i, "").trim();
  const expected = (env.POLL_TOKEN || "").trim();
  if (expected === "" || !timingSafeEqual(token, expected)) {
    return json({ ok: false, error: "unauthorized" }, 401);
  }
  const since = parseInt(url.searchParams.get("since") || "0", 10) || 0;
  let limit = parseInt(url.searchParams.get("limit") || "100", 10) || 100;
  if (limit > MAX_LIMIT) limit = MAX_LIMIT;

  const rows = await env.DB.prepare(
    `SELECT id, received_at, kind, chat_id, root_id, thread_id, sender_open_id, text, value_json
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
  }));
  const cursor = events.length ? events[events.length - 1].id : since;
  return json({ ok: true, events, cursor });
}

function json(obj, status = 200) {
  return new Response(JSON.stringify(obj), {
    status,
    headers: { "content-type": "application/json; charset=utf-8" },
  });
}

function safeParse(s) {
  try {
    return JSON.parse(s);
  } catch {
    return null;
  }
}

// timingSafeEqual compares two short strings without early-exit on mismatch.
function timingSafeEqual(a, b) {
  if (a.length !== b.length) return false;
  let diff = 0;
  for (let i = 0; i < a.length; i++) diff |= a.charCodeAt(i) ^ b.charCodeAt(i);
  return diff === 0;
}
