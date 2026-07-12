import assert from "node:assert/strict";
import test from "node:test";
import {
  CAPABILITY_STREAM_RESUME_V1,
  InMemoryStateStore,
  StreamJournal,
  eventId,
  formatSseFrame,
  hasStreamResumeCapability,
  parseResumeCursor,
  payloadDigest,
} from "./stream-journal.mjs";

function seedJournal() {
  const store = new InMemoryStateStore();
  store.ensureInvocation("inv1_stream_test", "accepted");
  return new StreamJournal(store);
}

test("hasStreamResumeCapability parses capability header lists", () => {
  assert.equal(hasStreamResumeCapability("stream-resume-v1"), true);
  assert.equal(hasStreamResumeCapability("invocation-id-v1, stream-resume-v1"), true);
  assert.equal(hasStreamResumeCapability("invocation-id-v1"), false);
  assert.equal(CAPABILITY_STREAM_RESUME_V1, "stream-resume-v1");
});

test("formatSseFrame emits id only after a positive sequence", () => {
  assert.equal(formatSseFrame({ type: "text", delta: "a" }), "data: {\"type\":\"text\",\"delta\":\"a\"}\n\n");
  assert.equal(
    formatSseFrame({ type: "text", delta: "a" }, { invocationId: "inv1_x", sequence: 3 }),
    "id: inv1_x:3\ndata: {\"type\":\"text\",\"delta\":\"a\"}\n\n",
  );
});

test("parseResumeCursor reads Last-Event-ID shaped cursors", () => {
  assert.deepEqual(parseResumeCursor("inv1_abc:7"), { invocationId: "inv1_abc", sequence: 7 });
  assert.equal(parseResumeCursor("bad"), null);
});

test("appendBeforeExpose assigns monotonic sequences and digests", async () => {
  const journal = seedJournal();
  const first = await journal.appendBeforeExpose("inv1_stream_test", "text", { type: "text", delta: "hi" });
  const second = await journal.appendBeforeExpose("inv1_stream_test", "reasoning", { type: "reasoning", delta: "think" });
  assert.equal(first.sequence, 1);
  assert.equal(second.sequence, 2);
  assert.equal(first.event_id, eventId("inv1_stream_test", 1));
  assert.equal(first.payload_digest, payloadDigest({ type: "text", delta: "hi" }));
  assert.match(first.sse_frame, /^id: inv1_stream_test:1\ndata: /);
});

test("journal commit completes before expose helper returns", async () => {
  const store = new InMemoryStateStore();
  store.ensureInvocation("inv1_order", "accepted");
  const order = [];
  const wrapped = {
    async appendJournal(event) {
      order.push("append-start");
      const resp = await store.appendJournal(event);
      order.push("append-committed");
      return resp;
    },
    readJournal: (...args) => store.readJournal(...args),
  };
  const journal = new StreamJournal(wrapped);
  const committed = await journal.appendBeforeExpose("inv1_order", "text", { type: "text", delta: "x" });
  order.push("expose-allowed");
  assert.deepEqual(order, ["append-start", "append-committed", "expose-allowed"]);
  assert.equal(committed.sequence, 1);
});

test("readAfter returns events strictly after the resume cursor", async () => {
  const journal = seedJournal();
  await journal.appendBeforeExpose("inv1_stream_test", "text", { type: "text", delta: "a" });
  await journal.appendBeforeExpose("inv1_stream_test", "text", { type: "text", delta: "b" });
  await journal.appendBeforeExpose("inv1_stream_test", "turn_end", { type: "turn_end", stop_reason: "end_turn" });
  const after1 = await journal.readAfter("inv1_stream_test", 1);
  assert.equal(after1.length, 2);
  assert.equal(after1[0].sequence, 2);
  assert.equal(after1[1].type, "turn_end");
  const after3 = await journal.readAfter("inv1_stream_test", 3);
  assert.equal(after3.length, 0);
});

test("appendBeforeExpose fails closed without an invocation", async () => {
  const journal = seedJournal();
  await assert.rejects(
    () => journal.appendBeforeExpose("", "text", { type: "text", delta: "x" }),
    /invocation_id required/,
  );
});

test("serialized appends preserve order under concurrency", async () => {
  const journal = seedJournal();
  const started = [];
  const store = journal.store;
  const slow = {
    async appendJournal(event) {
      started.push(event.type);
      await new Promise((r) => setTimeout(r, event.type === "text" ? 20 : 0));
      return store.appendJournal(event);
    },
    readJournal: (...args) => store.readJournal(...args),
  };
  const j = new StreamJournal(slow);
  const [a, b] = await Promise.all([
    j.appendBeforeExpose("inv1_stream_test", "text", { type: "text", delta: "1" }),
    j.appendBeforeExpose("inv1_stream_test", "reasoning", { type: "reasoning", delta: "2" }),
  ]);
  assert.equal(a.sequence, 1);
  assert.equal(b.sequence, 2);
  assert.deepEqual(started, ["text", "reasoning"]);
});
