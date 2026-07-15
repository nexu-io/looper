/**
 * Minimal SSE parser over a fetch Response body.
 * Supports event/data lines, multi-line data, and fragmented chunks.
 * Not EventSource — caller supplies Authorization via fetch.
 */

export type SSEEventHandler = (event: string, data: string) => void;

/**
 * Consume an SSE stream from response.body until done, aborted, or error.
 * Default event name is "message" when only data: lines are present.
 *
 * On abort: calls reader.cancel() immediately via AbortSignal listener
 * (unblocks a hanging read()).
 * After stream done: final TextDecoder flush and process remaining buffer.
 */
export async function consumeSSE(
  response: Response,
  onEvent: SSEEventHandler,
  signal?: AbortSignal,
): Promise<void> {
  const body = response.body;
  if (!body) {
    throw new Error("SSE response has no body");
  }

  const reader = body.getReader();
  const decoder = new TextDecoder();
  let buffer = "";
  let eventName = "message";
  let dataLines: string[] = [];
  let cancelled = false;

  const flush = () => {
    if (dataLines.length === 0) {
      eventName = "message";
      return;
    }
    const data = dataLines.join("\n");
    dataLines = [];
    const name = eventName || "message";
    eventName = "message";
    onEvent(name, data);
  };

  const processLine = (line: string) => {
    if (line === "") {
      flush();
      return;
    }
    if (line.startsWith(":")) {
      // comment / heartbeat
      return;
    }
    const colon = line.indexOf(":");
    let field: string;
    let value: string;
    if (colon === -1) {
      field = line;
      value = "";
    } else {
      field = line.slice(0, colon);
      value = line.slice(colon + 1);
      if (value.startsWith(" ")) {
        value = value.slice(1);
      }
    }
    if (field === "event") {
      eventName = value;
    } else if (field === "data") {
      dataLines.push(value);
    }
    // id / retry ignored for v1
  };

  const processBufferLines = () => {
    while (true) {
      const nl = buffer.indexOf("\n");
      if (nl === -1) {
        break;
      }
      let line = buffer.slice(0, nl);
      buffer = buffer.slice(nl + 1);
      if (line.endsWith("\r")) {
        line = line.slice(0, -1);
      }
      processLine(line);
    }
  };

  const processTrailingBuffer = () => {
    if (buffer.length > 0) {
      let line = buffer;
      buffer = "";
      if (line.endsWith("\r")) {
        line = line.slice(0, -1);
      }
      processLine(line);
    }
    flush();
  };

  const cancelReader = () => {
    if (cancelled) return;
    cancelled = true;
    // Fire-and-forget: must not await while holding the abort path that
    // unblocks a hanging reader.read().
    void reader.cancel().catch(() => {
      // ignore
    });
  };

  const onAbort = () => {
    cancelReader();
  };

  if (signal) {
    if (signal.aborted) {
      cancelReader();
      try {
        reader.releaseLock();
      } catch {
        // ignore
      }
      return;
    }
    signal.addEventListener("abort", onAbort, { once: true });
  }

  try {
    while (true) {
      if (signal?.aborted) {
        cancelReader();
        return;
      }

      let readResult: ReadableStreamReadResult<Uint8Array>;
      try {
        readResult = await reader.read();
      } catch (err) {
        if (signal?.aborted) {
          cancelReader();
          return;
        }
        throw err;
      }

      const { done, value } = readResult;
      if (done) {
        // Final decoder flush (no stream:true) then process remainder.
        buffer += decoder.decode();
        processBufferLines();
        processTrailingBuffer();
        break;
      }

      buffer += decoder.decode(value, { stream: true });
      processBufferLines();
    }
  } finally {
    if (signal) {
      signal.removeEventListener("abort", onAbort);
    }
    if (signal?.aborted) {
      cancelReader();
    }
    try {
      reader.releaseLock();
    } catch {
      // ignore if already cancelled/released
    }
  }
}
