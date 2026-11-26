import assert from "node:assert/strict";
import { after, before, describe, test } from "node:test";

import {
  CandleDBError,
  arrayRemove,
  arrayUnion,
  createClient,
  deleteField,
  increment,
  serverTimestamp,
} from "../dist/index.js";
import { freshCollection, startServer } from "./helpers.js";

describe("documents", () => {
  let server, db;
  before(async () => {
    server = await startServer();
    db = createClient({ url: server.url });
  });
  after(async () => {
    db.close();
    await server.stop();
  });

  test("set, get, update, delete", async () => {
    const ref = db.collection(freshCollection()).doc("alice");
    assert.equal((await ref.get()).exists, false);

    await ref.set({ name: "Alice", age: 30, tags: ["a"], address: { city: "Paris" } });
    let snap = await ref.get();
    assert.equal(snap.exists, true);
    assert.equal(snap.id, "alice");
    assert.equal(snap.path, ref.path);
    assert.deepEqual(snap.data(), { name: "Alice", age: 30, tags: ["a"], address: { city: "Paris" } });
    assert.equal(snap.get("address.city"), "Paris");
    assert.equal(snap.get("missing"), undefined);
    assert.ok(snap.version > 0);
    assert.ok(snap.createTime instanceof Date);
    const firstVersion = snap.version;

    await ref.set({ name: "Alicia" });
    snap = await ref.get();
    assert.deepEqual(snap.data(), { name: "Alicia" }, "set replaces the document");
    assert.ok(snap.version > firstVersion);

    await ref.delete();
    snap = await ref.get();
    assert.equal(snap.exists, false);
    assert.equal(snap.data(), undefined);
    assert.equal(snap.version, 0);
    await ref.delete(); // deleting a missing document succeeds
  });

  test("set with merge keeps other fields, including nested ones", async () => {
    const ref = db.collection(freshCollection()).doc("d");
    await ref.set({ a: 1, nested: { x: 1, y: 2 } });
    await ref.set({ b: 2, nested: { y: 20, z: 30 } }, { merge: true });
    assert.deepEqual((await ref.get()).data(), { a: 1, b: 2, nested: { x: 1, y: 20, z: 30 } });

    const fresh = db.collection(freshCollection()).doc("d");
    await fresh.set({ only: true }, { merge: true });
    assert.deepEqual((await fresh.get()).data(), { only: true }, "merge creates a missing document");
  });

  test("update with dotted paths", async () => {
    const ref = db.collection(freshCollection()).doc("d");
    await ref.set({ name: "Bob", address: { city: "Paris", zip: "75001" } });
    await ref.update({ "address.city": "Lyon", age: 41 });
    assert.deepEqual((await ref.get()).data(), { name: "Bob", age: 41, address: { city: "Lyon", zip: "75001" } });

    await ref.update({ address: { country: "FR" } });
    assert.deepEqual((await ref.get()).get("address"), { country: "FR" }, "a map value replaces the field");
  });

  test("update of a missing document fails with not_found", async () => {
    const ref = db.collection(freshCollection()).doc("ghost");
    await assert.rejects(ref.update({ a: 1 }), (err) => {
      assert.ok(err instanceof CandleDBError);
      assert.equal(err.code, "not_found");
      assert.equal(err.status, 404);
      return true;
    });
  });

  test("add generates an ID and refuses nothing", async () => {
    const col = db.collection(freshCollection());
    const a = await col.add({ n: 1 });
    const b = await col.add({ n: 2 });
    assert.notEqual(a.id, b.id);
    assert.equal(a.id.length, 20);
    assert.equal((await a.get()).get("n"), 1);
    assert.equal(a.parent.path, col.path);
  });

  test("subcollections", async () => {
    const user = db.collection(freshCollection()).doc("u1");
    const post = user.collection("posts").doc("p1");
    await post.set({ title: "Hello" });
    assert.equal(post.path, `${user.path}/posts/p1`);
    assert.equal((await post.get()).get("title"), "Hello");
    assert.equal((await user.get()).exists, false, "a subcollection does not create its parent");
  });

  test("sentinels: increment, arrays, server timestamp, deleteField", async () => {
    const ref = db.collection(freshCollection()).doc("d");
    const before = Date.now();
    await ref.set({ count: increment(5), tags: arrayUnion("a", "b"), created: serverTimestamp(), temp: 1 });
    let data = (await ref.get()).data();
    assert.equal(data.count, 5);
    assert.deepEqual(data.tags, ["a", "b"]);
    assert.ok(data.created instanceof Date);
    assert.ok(Math.abs(data.created.getTime() - before) < 60_000);

    await ref.update({
      count: increment(-2),
      tags: arrayUnion("b", "c"),
      temp: deleteField(),
      "stats.views": increment(1),
    });
    data = (await ref.get()).data();
    assert.equal(data.count, 3);
    assert.deepEqual(data.tags, ["a", "b", "c"]);
    assert.equal("temp" in data, false);
    assert.deepEqual(data.stats, { views: 1 });

    await ref.update({ tags: arrayRemove("a", "c"), count: increment(0.5) });
    data = (await ref.get()).data();
    assert.deepEqual(data.tags, ["b"]);
    assert.equal(data.count, 3.5);

    await ref.set({ gone: deleteField(), kept: 1 }, { merge: true });
    assert.equal((await ref.get()).get("kept"), 1);
  });

  test("values round trip, including dates and nesting", async () => {
    const ref = db.collection(freshCollection()).doc("d");
    const when = new Date("2024-02-29T12:34:56.789Z");
    const value = {
      nothing: null,
      yes: true,
      int: 42,
      big: 9007199254740991,
      float: 1.5,
      negative: -0.25,
      text: "héllo 🌍",
      when,
      list: [1, "two", null, [3], { four: 4 }, when],
      map: { deep: { deeper: { deepest: "x" } }, empty: {}, none: [] },
    };
    await ref.set(value);
    assert.deepEqual((await ref.get()).data(), value);
  });

  test("invalid data is refused", async () => {
    const ref = db.collection(freshCollection()).doc("d");
    await assert.rejects(ref.set({ "bad.name": 1 }), { code: "invalid_argument" });
    await assert.rejects(ref.set({ $reserved: 1 }), { code: "invalid_argument" });
    assert.throws(() => db.doc("just-a-collection"), { code: "invalid_argument" });
    assert.throws(() => db.collection("a/b"), { code: "invalid_argument" });
    await assert.rejects(ref.set({ n: Number.NaN }), { code: "invalid_argument" });
  });

  test("a batch is atomic", async () => {
    const col = db.collection(freshCollection());
    await col.doc("existing").set({ n: 1 });

    const ok = db.batch();
    ok.set(col.doc("a"), { n: 1 }).create(col.doc("b"), { n: 2 }).update(col.doc("existing"), { n: increment(1) });
    ok.delete(col.doc("nothing"));
    await ok.commit();
    assert.equal((await col.doc("a").get()).get("n"), 1);
    assert.equal((await col.doc("b").get()).get("n"), 2);
    assert.equal((await col.doc("existing").get()).get("n"), 2);
    await assert.rejects(ok.commit(), { code: "failed_precondition" });

    const bad = db.batch();
    bad.set(col.doc("c"), { n: 3 });
    bad.update(col.doc("missing"), { n: 1 }); // fails: the document does not exist
    await assert.rejects(bad.commit(), { code: "not_found" });
    assert.equal((await col.doc("c").get()).exists, false, "no write of a failed batch is applied");

    const dup = db.batch().create(col.doc("a"), { n: 9 }).set(col.doc("d"), { n: 4 });
    await assert.rejects(dup.commit(), { code: "already_exists", status: 409 });
    assert.equal((await col.doc("d").get()).exists, false);
  });

  test("a transaction reads then writes", async () => {
    const col = db.collection(freshCollection());
    const from = col.doc("from");
    const to = col.doc("to");
    await from.set({ balance: 100 });

    const moved = await db.runTransaction(async (tx) => {
      const a = await tx.get(from);
      const b = await tx.get(to);
      const amount = 30;
      tx.update(from, { balance: a.get("balance") - amount });
      tx.set(to, { balance: (b.exists ? b.get("balance") : 0) + amount });
      return amount;
    });
    assert.equal(moved, 30);
    assert.equal((await from.get()).get("balance"), 70);
    assert.equal((await to.get()).get("balance"), 30);

    await assert.rejects(
      db.runTransaction(async (tx) => {
        tx.set(to, { balance: 0 });
        await tx.get(from);
      }),
      { code: "failed_precondition" },
    );
  });

  test("a transaction is retried when a document it read has changed", async () => {
    const ref = db.collection(freshCollection()).doc("d");
    await ref.set({ n: 1 });
    let runs = 0;
    await db.runTransaction(async (tx) => {
      runs++;
      const snap = await tx.get(ref);
      if (runs === 1) await ref.update({ n: 100 }); // someone else gets in first
      tx.update(ref, { n: snap.get("n") + 1 });
    });
    assert.equal(runs, 2);
    assert.equal((await ref.get()).get("n"), 101);

    runs = 0;
    await assert.rejects(
      db.runTransaction(
        async (tx) => {
          runs++;
          const snap = await tx.get(ref);
          await ref.update({ n: increment(1) }); // always interferes
          tx.update(ref, { n: snap.get("n") });
        },
        { maxAttempts: 3 },
      ),
      { code: "failed_precondition", status: 412 },
    );
    assert.equal(runs, 3);
  });

  test("a transaction is retried when a document it only read has changed", async () => {
    const col = db.collection(freshCollection());
    const price = col.doc("price");
    const order = col.doc("order");
    await price.set({ amount: 10 });

    let runs = 0;
    await db.runTransaction(async (tx) => {
      runs++;
      const snap = await tx.get(price); // read, never written
      if (runs === 1) await price.set({ amount: 15 }); // the price changes underneath us
      tx.set(order, { total: snap.get("amount") * 2 });
    });
    assert.equal(runs, 2);
    assert.equal((await order.get()).get("total"), 30, "the order must use the price that was current at commit");
    assert.equal((await price.get()).get("amount"), 15);
  });

  test("concurrent transactions from several clients never lose an update", async () => {
    const path = `${freshCollection()}/counter`;
    await db.doc(path).set({ n: 0 });
    const clients = Array.from({ length: 4 }, () => createClient({ url: server.url }));
    const perClient = 5;

    await Promise.all(
      clients.flatMap((client) =>
        Array.from({ length: perClient }, () =>
          client.runTransaction(
            async (tx) => {
              const snap = await tx.get(client.doc(path));
              tx.set(client.doc(path), { n: snap.get("n") + 1 });
            },
            { maxAttempts: 200 },
          ),
        ),
      ),
    );
    assert.equal((await db.doc(path).get()).get("n"), clients.length * perClient);
  });

  test("an unreachable server yields an 'unavailable' error", async () => {
    const offline = createClient({ url: "http://127.0.0.1:1" });
    await assert.rejects(offline.doc("a/b").get(), (err) => {
      assert.ok(err instanceof CandleDBError);
      assert.equal(err.code, "unavailable");
      assert.equal(err.status, 0);
      return true;
    });
  });
});
