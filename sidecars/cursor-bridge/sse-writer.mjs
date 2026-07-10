export class SseWriterError extends Error {
  constructor(code, message) {
    super(message);
    this.name = "SseWriterError";
    this.code = code;
  }
}

function rejectedReceipt(error) {
  const handedToNode = Promise.reject(error);
  handedToNode.catch(() => {});
  return { handedToNode, ok: false, queued: false };
}

export class SseWriter {
  constructor(res, { maxQueueBytes, onFailure = null } = {}) {
    this.res = res;
    this.maxQueueBytes = maxQueueBytes;
    this.onFailure = onFailure;
    this.queue = [];
    this.queueBytes = 0;
    this.blocked = false;
    this.failed = null;
    this.endRequested = false;
    this.ended = false;
    this._drain = () => this.drain();
    this._close = () => {
      if (!this.ended) this.fail(new SseWriterError("transport_closed", "response closed before its terminal receipt"));
    };
    if (typeof res.on === "function") {
      res.on("drain", this._drain);
      res.on("close", this._close);
    }
  }

  isHealthy() { return !this.failed && !this.ended; }

  write(payload) {
    if (!this.isHealthy()) return rejectedReceipt(this.failed || new SseWriterError("writer_closed", "response writer is closed"));
    if (this.blocked || this.queue.length) return this.enqueue(payload);
    return this.writeNow(payload);
  }

  writeNow(payload) {
    let accepted;
    try { accepted = this.res.write(payload); }
    catch (error) {
      const wrapped = new SseWriterError("write_failed", `response write failed: ${error.message}`);
      this.fail(wrapped);
      return rejectedReceipt(wrapped);
    }
    if (accepted === false) this.blocked = true;
    return { handedToNode: Promise.resolve(), ok: true, queued: false };
  }

  enqueue(payload) {
    const bytes = Buffer.byteLength(payload);
    if (this.queueBytes + bytes > this.maxQueueBytes) {
      const error = new SseWriterError("backpressure_overflow", `SSE output queue exceeded ${this.maxQueueBytes} bytes`);
      this.fail(error);
      return rejectedReceipt(error);
    }
    let resolve;
    let reject;
    const handedToNode = new Promise((res, rej) => { resolve = res; reject = rej; });
    this.queue.push({ bytes, payload, reject, resolve });
    this.queueBytes += bytes;
    return { handedToNode, ok: true, queued: true };
  }

  drain() {
    if (!this.isHealthy()) return;
    this.blocked = false;
    while (this.queue.length && !this.blocked) {
      const entry = this.queue[0];
      let accepted;
      try { accepted = this.res.write(entry.payload); }
      catch (error) {
        this.fail(new SseWriterError("write_failed", `response drain write failed: ${error.message}`));
        return;
      }
      this.queue.shift();
      this.queueBytes -= entry.bytes;
      entry.resolve();
      if (accepted === false) this.blocked = true;
    }
  }

  async endAfter(payload = null) {
    if (this.endRequested) return this.endPromise;
    this.endRequested = true;
    const receipt = payload == null
      ? { handedToNode: Promise.resolve(), ok: true, queued: false }
      : this.write(payload);
    this.endPromise = receipt.handedToNode
      .catch(() => {})
      .then(() => {
        if (!this.ended) {
          this.ended = true;
          try { this.res.end(); } catch {}
          this.detach();
        }
      });
    return this.endPromise;
  }

  fail(error) {
    if (this.failed || this.ended) return false;
    this.failed = error instanceof Error ? error : new SseWriterError("transport_failed", String(error));
    for (const entry of this.queue) entry.reject(this.failed);
    this.queue = [];
    this.queueBytes = 0;
    this.detach();
    if (this.onFailure) {
      try { this.onFailure(this.failed); } catch {}
    }
    return true;
  }

  detach() {
    if (typeof this.res.off === "function") {
      try { this.res.off("drain", this._drain); } catch {}
      try { this.res.off("close", this._close); } catch {}
    }
  }
}
