import assert from "node:assert/strict";
import { EventEmitter } from "node:events";
import test from "node:test";
import { SseWriter } from "./sse-writer.mjs";

class FakeResponse extends EventEmitter {
  constructor(returns = []) {
    super();
    this.returns = returns;
    this.writes = [];
    this.ended = false;
  }
  write(payload) {
    this.writes.push(payload);
    return this.returns.length ? this.returns.shift() : true;
  }
  end() { this.ended = true; }
}

test("false from res.write is an accepted receipt and only later frames queue", async () => {
  const res = new FakeResponse([false, true]);
  const writer = new SseWriter(res, { maxQueueBytes: 1024 });
  const first = writer.write("one");
  const second = writer.write("two");
  assert.equal(first.queued, false);
  assert.equal(second.queued, true);
  await first.handedToNode;
  assert.deepEqual(res.writes, ["one"]);
  res.emit("drain");
  await second.handedToNode;
  assert.deepEqual(res.writes, ["one", "two"]);
});

test("terminal payload drains before response end", async () => {
  const res = new FakeResponse([false, true, true]);
  const writer = new SseWriter(res, { maxQueueBytes: 1024 });
  writer.write("tool_call");
  const terminal = writer.write("turn_end");
  const done = writer.endAfter("[DONE]");
  assert.equal(res.ended, false);
  res.emit("drain");
  await terminal.handedToNode;
  await done;
  assert.deepEqual(res.writes, ["tool_call", "turn_end", "[DONE]"]);
  assert.equal(res.ended, true);
});

test("disconnect rejects queued receipts and reports one failure", async () => {
  const res = new FakeResponse([false]);
  const failures = [];
  const writer = new SseWriter(res, { maxQueueBytes: 1024, onFailure: (error) => failures.push(error.code) });
  writer.write("accepted");
  const queued = writer.write("queued");
  res.emit("close");
  await assert.rejects(queued.handedToNode, /closed before its terminal receipt/);
  res.emit("close");
  assert.deepEqual(failures, ["transport_closed"]);
});

test("disconnect after end is requested still rejects an unhanded terminal frame", async () => {
  const res = new FakeResponse([false]);
  const failures = [];
  const writer = new SseWriter(res, { maxQueueBytes: 1024, onFailure: (error) => failures.push(error.code) });
  writer.write("tool_call");
  const ending = writer.endAfter("turn_end-and-done");
  res.emit("close");
  await ending;
  assert.deepEqual(failures, ["transport_closed"]);
  assert.equal(res.ended, true);
  assert.deepEqual(res.writes, ["tool_call"]);
});

test("queue overflow rejects every queued frame and never ends as success", async () => {
  const res = new FakeResponse([false]);
  const failures = [];
  const writer = new SseWriter(res, { maxQueueBytes: 3, onFailure: (error) => failures.push(error.code) });
  writer.write("x");
  const queued = writer.write("abc");
  const overflow = writer.write("d");
  await assert.rejects(queued.handedToNode, /exceeded/);
  await assert.rejects(overflow.handedToNode, /exceeded/);
  assert.equal(res.ended, false);
  assert.deepEqual(failures, ["backpressure_overflow"]);
});
