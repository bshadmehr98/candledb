import assert from "node:assert/strict";
import { after, before, describe, test } from "node:test";

import { createClient, increment, serverTimestamp } from "../dist/index.js";
import { ADMIN_KEY, freshCollection, recorder, startServer, waitFor } from "./helpers.js";

const ids = (snapshot) => snapshot.docs.map((d) => d.id);
const confirmed = (snapshot) => !snapshot.metadata.hasPendingWrites;

describe("realtime", () => {
  let server, db, other;
  before(async () => {
    server = await startServer();
    db = createClient({ url: server.url });
    other = createClient({ url: server.url }); // stands for another user's device
  });
  after(async () => {
    db.close();
    other.close();
    await server.stop();
  });

  test("a document listener follows create, update and delete", async () => {
    const ref = db.collection(freshCollection()).doc("d");
    const rec = recorder();
    const stop = ref.onSnapshot(rec.onNext, rec.onError);
    try {
      const first = await rec.until(() => true);
      assert.equal(first.exists, false);
      assert.equal(first.metadata.hasPendingWrites, false);

      await other.doc(ref.path).set({ n: 1 });
      let snap = await rec.until((s) => s.exists);
      assert.deepEqual(snap.data(), { n: 1 });
      assert.equal(snap.id, "d");

      await other.doc(ref.path).update({ n: 2 });
      snap = await rec.until((s) => s.get("n") === 2);
      assert.ok(snap.version > 0);

      await other.doc(ref.path).delete();
      await rec.until((s) => !s.exists);
      assert.deepEqual(rec.errors, []);
    } finally {
      stop();
    }
  });

  test("a query listener reports added, modified and removed documents in order", async () => {
    const col = db.collection(freshCollection());
    await other.doc(`${col.path}/b`).set({ rank: 2, active: true });
    await other.doc(`${col.path}/x`).set({ rank: 9, active: false });

    const rec = recorder();
    const stop = col.where("active", "==", true).orderBy("rank").onSnapshot(rec.onNext, rec.onError);
    try {
      const first = await rec.until(() => true);
      assert.deepEqual(ids(first), ["b"]);
      assert.deepEqual(first.docChanges().map((c) => [c.type, c.doc.id]), [["added", "b"]]);

      await other.doc(`${col.path}/a`).set({ rank: 1, active: true });
      let snap = await rec.until((s) => s.size === 2);
      assert.deepEqual(ids(snap), ["a", "b"]);
      assert.deepEqual(snap.docChanges().map((c) => [c.type, c.doc.id]), [["added", "a"]]);

      await other.doc(`${col.path}/a`).update({ rank: 5 });
      snap = await rec.until((s) => s.docs[0]?.id === "b" && s.size === 2);
      assert.deepEqual(ids(snap), ["b", "a"], "the result is re-sorted");
      assert.deepEqual(snap.docChanges().map((c) => [c.type, c.doc.id]), [["modified", "a"]]);

      // A document that starts matching enters; one that stops matching leaves.
      await other.doc(`${col.path}/x`).update({ active: true });
      snap = await rec.until((s) => s.size === 3);
      assert.deepEqual(ids(snap), ["b", "a", "x"]);

      await other.doc(`${col.path}/b`).update({ active: false });
      snap = await rec.until((s) => s.size === 2);
      assert.deepEqual(snap.docChanges().map((c) => [c.type, c.doc.id]), [["removed", "b"]]);

      await other.doc(`${col.path}/a`).delete();
      snap = await rec.until((s) => s.size === 1);
      assert.deepEqual(ids(snap), ["x"]);

      // A write that does not touch the result does not wake the listener.
      const count = rec.snapshots.length;
      await other.doc(`${col.path}/zzz`).set({ rank: 0, active: false });
      await other.doc(`${col.path}/x`).update({ rank: 10 });
      await rec.until((s) => s.docs[0]?.get("rank") === 10);
      assert.equal(rec.snapshots.length, count + 1);
      assert.deepEqual(rec.errors, []);
    } finally {
      stop();
    }
  });

  test("a limited query pulls in the next document when one leaves", async () => {
    const col = db.collection(freshCollection());
    const batch = other.batch();
    for (const [id, score] of [["a", 50], ["b", 40], ["c", 30], ["d", 20]]) {
      batch.set(other.doc(`${col.path}/${id}`), { score });
    }
    await batch.commit();

    const rec = recorder();
    const stop = col.orderBy("score", "desc").limit(2).onSnapshot(rec.onNext, rec.onError);
    try {
      assert.deepEqual(ids(await rec.until(() => true)), ["a", "b"]);

      await other.doc(`${col.path}/a`).delete();
      let snap = await rec.until((s) => !ids(s).includes("a") && s.size === 2);
      assert.deepEqual(ids(snap), ["b", "c"]);
      assert.deepEqual(
        snap.docChanges().map((c) => [c.type, c.doc.id]).sort(),
        [["added", "c"], ["removed", "a"]],
      );

      await other.doc(`${col.path}/d`).update({ score: 100 });
      snap = await rec.until((s) => s.docs[0]?.id === "d");
      assert.deepEqual(ids(snap), ["d", "b"]);
    } finally {
      stop();
    }
  });

  test("two clients see each other's writes", async () => {
    const col = freshCollection();
    const mine = recorder();
    const theirs = recorder();
    const stopMine = db.collection(col).onSnapshot(mine.onNext, mine.onError);
    const stopTheirs = other.collection(col).onSnapshot(theirs.onNext, theirs.onError);
    try {
      await Promise.all([mine.until(() => true), theirs.until(() => true)]);
      await db.doc(`${col}/from-me`).set({ by: "me" });
      await other.doc(`${col}/from-them`).set({ by: "them" });
      for (const rec of [mine, theirs]) {
        const snap = await rec.until((s) => s.size === 2 && confirmed(s));
        assert.deepEqual(ids(snap), ["from-me", "from-them"]);
      }
    } finally {
      stopMine();
      stopTheirs();
    }
  });

  test("a local write shows immediately as pending, then is confirmed", async () => {
    const ref = db.collection(freshCollection()).doc("d");
    await ref.set({ n: 1 });
    const rec = recorder();
    const stop = ref.onSnapshot(rec.onNext, rec.onError);
    try {
      await rec.until((s) => s.get("n") === 1);
      const before = rec.snapshots.length;

      const write = ref.update({ n: increment(1), at: serverTimestamp() });
      // The listener has already fired, before the request had any chance
      // to reach the server.
      assert.equal(rec.snapshots.length, before + 1);
      assert.equal(rec.last.get("n"), 2);
      assert.ok(rec.last.get("at") instanceof Date);
      assert.equal(rec.last.metadata.hasPendingWrites, true);

      await write;
      const done = await rec.until(confirmed);
      assert.equal(done.get("n"), 2);
      assert.ok(done.version > rec.snapshots[before].version);
      // The value never went backwards or counted the increment twice.
      assert.deepEqual([...new Set(rec.snapshots.slice(before).map((s) => s.get("n")))], [2]);
    } finally {
      stop();
    }
  });

  test("local writes show in query listeners, including creates and deletes", async () => {
    const col = db.collection(freshCollection());
    await col.doc("a").set({ n: 1, keep: true });
    const rec = recorder();
    const stop = col.where("keep", "==", true).orderBy("n").onSnapshot(rec.onNext, rec.onError);
    try {
      await rec.until((s) => s.size === 1);

      const create = col.doc("b").set({ n: 0, keep: true });
      assert.deepEqual(ids(rec.last), ["b", "a"]);
      assert.equal(rec.last.metadata.hasPendingWrites, true);
      assert.equal(rec.last.docs[0].metadata.hasPendingWrites, true);
      assert.equal(rec.last.docs[1].metadata.hasPendingWrites, false);
      await create;
      await rec.until((s) => confirmed(s) && s.size === 2);

      const hide = col.doc("a").update({ keep: false });
      assert.deepEqual(ids(rec.last), ["b"], "a document that stops matching leaves at once");
      await hide;

      const remove = col.doc("b").delete();
      assert.equal(rec.last.empty, true);
      assert.equal(rec.last.metadata.hasPendingWrites, true);
      await remove;
      const end = await rec.until(confirmed);
      assert.equal(end.empty, true);
    } finally {
      stop();
    }
  });

  test("rapid local increments are never shown out of step", async () => {
    const ref = db.collection(freshCollection()).doc("counter");
    await ref.set({ n: 0 });
    const rec = recorder();
    const stop = ref.onSnapshot(rec.onNext, rec.onError);
    try {
      await rec.until((s) => s.exists);
      const writes = [];
      for (let i = 0; i < 20; i++) writes.push(ref.update({ n: increment(1) }));
      assert.equal(rec.last.get("n"), 20);
      await Promise.all(writes);
      const done = await rec.until((s) => confirmed(s) && s.get("n") === 20);
      assert.equal(done.get("n"), 20);
      const values = rec.snapshots.map((s) => s.get("n"));
      assert.ok(Math.max(...values) === 20, `no snapshot overshoots: ${values}`);
      for (let i = 1; i < values.length; i++) {
        assert.ok(values[i] >= values[i - 1], `values never go backwards: ${values}`);
      }
    } finally {
      stop();
    }
  });

  test("unsubscribing stops delivery and the stream closes with the last listener", async () => {
    const ref = db.collection(freshCollection()).doc("d");
    const admin = createClient({ url: server.url, adminKey: ADMIN_KEY });
    const sessions = async () => {
      const res = await fetch(`${server.url}/metrics`, { headers: { Authorization: `Bearer ${ADMIN_KEY}` } });
      return Number(/candledb_listen_sessions (\d+)/.exec(await res.text())[1]);
    };
    const quiet = createClient({ url: server.url });
    // Earlier tests close their streams without waiting for the server to
    // notice, so the count is only meaningful once it has settled at zero.
    await waitFor(async () => (await sessions()) === 0, { message: "streams of earlier tests are still open" });

    const rec = recorder();
    const stop = quiet.doc(ref.path).onSnapshot(rec.onNext, rec.onError);
    try {
      await rec.until(() => true);
      assert.equal(await sessions(), 1);
    } finally {
      stop();
    }

    await waitFor(async () => (await sessions()) === 0, { message: "the stream was not closed" });
    const count = rec.snapshots.length;
    await admin.doc(ref.path).set({ n: 1 });
    await admin.doc(ref.path).set({ n: 2 });
    assert.equal(rec.snapshots.length, count);
  });

  test("listeners recover after the server is killed and restarted", async () => {
    const col = freshCollection();
    const client = createClient({ url: server.url, reconnect: { minDelayMs: 20, maxDelayMs: 100 } });
    const docRec = recorder();
    const queryRec = recorder();
    const stopDoc = client.doc(`${col}/d`).onSnapshot(docRec.onNext, docRec.onError);
    const stopQuery = client.collection(col).orderBy("n").onSnapshot(queryRec.onNext, queryRec.onError);
    try {
      await client.doc(`${col}/d`).set({ n: 1 });
      await docRec.until((s) => s.get("n") === 1 && confirmed(s));
      await queryRec.until((s) => s.size === 1 && confirmed(s));
      const docCount = docRec.snapshots.length;
      const queryCount = queryRec.snapshots.length;

      await server.kill();
      await assert.rejects(client.doc(`${col}/d`).get(), { code: "unavailable" });
      await server.start();

      // Data written before the crash is still there, and the fresh
      // snapshot after reconnecting equals what the listeners already
      // showed, so they stay silent until something really changes.
      await other.doc(`${col}/e`).set({ n: 2 });
      const snap = await queryRec.until((s) => s.size === 2, "the query listener did not recover");
      assert.deepEqual(ids(snap), ["d", "e"]);
      assert.deepEqual(snap.docChanges().map((c) => [c.type, c.doc.id]), [["added", "e"]]);
      assert.equal(queryRec.snapshots.length, queryCount + 1);

      await other.doc(`${col}/d`).update({ n: 5 });
      await docRec.until((s) => s.get("n") === 5, "the document listener did not recover");
      assert.equal(docRec.snapshots.length, docCount + 1);
      assert.deepEqual(docRec.errors, []);
      assert.deepEqual(queryRec.errors, []);
    } finally {
      stopDoc();
      stopQuery();
      client.close();
    }
  });
});
