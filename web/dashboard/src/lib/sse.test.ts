import { describe, expect, it, vi } from "vitest";
import { consumeSSE } from "./sse";

function streamFromChunks(chunks: string[]): ReadableStream<Uint8Array> {
  const encoder = new TextEncoder();
  let i = 0;
  return new ReadableStream({
    pull(controller) {
      if (i >= chunks.length) {
        controller.close();
        return;
      }
      controller.enqueue(encoder.encode(chunks[i++]));
    },
  });
}

function responseFromChunks(chunks: string[]): Response {
  return new Response(streamFromChunks(chunks), {
    status: 200,
    headers: { "Content-Type": "text/event-stream" },
  });
}

describe("consumeSSE", () => {
  it("parses fragmented events across chunk boundaries", async () => {
    const events: Array<{ event: string; data: string }> = [];
    // Split mid-line and mid-event.
    await consumeSSE(
      responseFromChunks([
        "event: snap",
        'shot\ndata: {"a":1}\n\n',
        "event: chunk\ndata: line1\nda",
        "ta: line2\n\n",
      ]),
      (event, data) => {
        events.push({ event, data });
      },
    );

    expect(events).toEqual([
      { event: "snapshot", data: '{"a":1}' },
      { event: "chunk", data: "line1\nline2" },
    ]);
  });

  it("flushes trailing buffer after stream ends", async () => {
    const events: Array<{ event: string; data: string }> = [];
    await consumeSSE(
      responseFromChunks(['event: end\ndata: {"reason":"done"}\n']),
      (event, data) => {
        events.push({ event, data });
      },
    );
    expect(events).toEqual([{ event: "end", data: '{"reason":"done"}' }]);
  });

  it("cancels the reader on abort after an event", async () => {
    const cancel = vi.fn(async () => {});
    const encoder = new TextEncoder();
    let pullCount = 0;
    const body = new ReadableStream<Uint8Array>({
      pull(controller) {
        pullCount += 1;
        if (pullCount === 1) {
          controller.enqueue(encoder.encode("event: chunk\ndata: a\n\n"));
          return;
        }
        // hang until cancel
        return new Promise(() => {});
      },
      cancel,
    });

    const response = new Response(body, {
      status: 200,
      headers: { "Content-Type": "text/event-stream" },
    });

    const controller = new AbortController();
    const events: string[] = [];
    const done = consumeSSE(
      response,
      (event) => {
        events.push(event);
        controller.abort();
      },
      controller.signal,
    );

    await done;
    expect(events).toContain("chunk");
    expect(cancel).toHaveBeenCalled();
  });

  it("cancels immediately while blocked in reader.read()", async () => {
    const cancel = vi.fn(async () => {});
    const body = new ReadableStream<Uint8Array>({
      pull() {
        // Never enqueue — hang in read() until cancel unblocks.
        return new Promise(() => {});
      },
      cancel,
    });

    const response = new Response(body, {
      status: 200,
      headers: { "Content-Type": "text/event-stream" },
    });

    const controller = new AbortController();
    const done = consumeSSE(response, () => {}, controller.signal);

    // Let consumeSSE enter the hanging read().
    await new Promise((r) => setTimeout(r, 20));
    controller.abort();

    await expect(done).resolves.toBeUndefined();
    expect(cancel).toHaveBeenCalled();
  });
});
