import assert from "node:assert/strict";
import { after, before, describe, test } from "node:test";

import { createClient } from "../dist/index.js";
import { freshCollection, startServer } from "./helpers.js";

describe("queries", () => {
  let server, db, people;

  const ids = (snapshot) => snapshot.docs.map((d) => d.id);

  before(async () => {
    server = await startServer();
    db = createClient({ url: server.url });
    people = db.collection(freshCollection("people"));
    const batch = db.batch();
    batch.set(people.doc("ann"), { name: "Ann", age: 30, city: "Paris", tags: ["a", "b"], score: 1.5 });
    batch.set(people.doc("bob"), { name: "Bob", age: 25, city: "Lyon", tags: ["b"], score: 2 });
    batch.set(people.doc("cat"), { name: "Cat", age: 35, city: "Paris", tags: ["c"], score: 3 });
    batch.set(people.doc("dan"), { name: "Dan", age: 30, city: "Nice", tags: [], score: null });
    batch.set(people.doc("eve"), { name: "Eve", city: "Paris", age: "unknown" });
    await batch.commit();
  });
  after(async () => {
    db.close();
    await server.stop();
  });

  test("a collection without filters returns everything in ID order", async () => {
    const snap = await people.get();
    assert.deepEqual(ids(snap), ["ann", "bob", "cat", "dan", "eve"]);
    assert.equal(snap.size, 5);
    assert.equal(snap.empty, false);
    assert.deepEqual(snap.docChanges().map((c) => c.type), Array(5).fill("added"));
    assert.equal((await db.collection(freshCollection()).get()).empty, true);
  });

  test("comparison operators", async () => {
    assert.deepEqual(ids(await people.where("city", "==", "Paris").get()), ["ann", "cat", "eve"]);
    assert.deepEqual(ids(await people.where("city", "!=", "Paris").get()), ["bob", "dan"]);
    assert.deepEqual(ids(await people.where("age", "<", 30).get()), ["bob"]);
    assert.deepEqual(ids(await people.where("age", "<=", 30).get()), ["ann", "bob", "dan"]);
    assert.deepEqual(ids(await people.where("age", ">", 30).get()), ["cat"]);
    assert.deepEqual(ids(await people.where("age", ">=", 30).get()), ["ann", "cat", "dan"]);
    // An ordering comparison only matches values of the same type.
    assert.deepEqual(ids(await people.where("age", ">", "a").get()), ["eve"]);
    assert.deepEqual(ids(await people.where("score", "==", null).get()), ["dan"]);
    assert.deepEqual(ids(await people.where("score", "==", 2.0).get()), ["bob"]);
  });

  test("list operators", async () => {
    assert.deepEqual(ids(await people.where("city", "in", ["Lyon", "Nice"]).get()), ["bob", "dan"]);
    assert.deepEqual(ids(await people.where("city", "not-in", ["Lyon", "Nice"]).get()), ["ann", "cat", "eve"]);
    assert.deepEqual(ids(await people.where("tags", "array-contains", "b").get()), ["ann", "bob"]);
    assert.deepEqual(ids(await people.where("tags", "array-contains-any", ["a", "c", "b"]).get()), ["ann", "bob", "cat"]);
  });

  test("several filters combine", async () => {
    const q = people.where("city", "==", "Paris").where("age", ">=", 30).where("tags", "array-contains", "c");
    assert.deepEqual(ids(await q.get()), ["cat"]);
    assert.deepEqual(ids(await people.where("age", ">", 25).where("age", "<", 35).get()), ["ann", "dan"]);
  });

  test("ordering, with ties broken by ID and mixed types sorted by kind", async () => {
    // eve's age is a string, which sorts after every number.
    assert.deepEqual(ids(await people.orderBy("age").get()), ["bob", "ann", "dan", "cat", "eve"]);
    assert.deepEqual(ids(await people.orderBy("age", "desc").get()), ["eve", "cat", "dan", "ann", "bob"]);
    assert.deepEqual(ids(await people.orderBy("city").orderBy("age", "desc").get()), ["bob", "dan", "eve", "cat", "ann"]);
    // Documents without the ordered field are left out.
    assert.deepEqual(ids(await people.orderBy("score").get()), ["dan", "ann", "bob", "cat"]);
    assert.deepEqual(ids(await people.where("city", "==", "Paris").orderBy("name", "desc").get()), ["eve", "cat", "ann"]);
  });

  test("limit", async () => {
    assert.deepEqual(ids(await people.limit(2).get()), ["ann", "bob"]);
    assert.deepEqual(ids(await people.orderBy("age", "desc").limit(3).get()), ["eve", "cat", "dan"]);
    assert.deepEqual(ids(await people.where("city", "==", "Paris").limit(1).get()), ["ann"]);
    assert.throws(() => people.limit(0), { code: "invalid_argument" });
  });

  test("cursors by value", async () => {
    const byAge = people.where("age", "<", 100).orderBy("age");
    assert.deepEqual(ids(await byAge.startAt(30).get()), ["ann", "dan", "cat"]);
    assert.deepEqual(ids(await byAge.startAfter(30).get()), ["cat"]);
    assert.deepEqual(ids(await byAge.endAt(30).get()), ["bob", "ann", "dan"]);
    assert.deepEqual(ids(await byAge.endBefore(30).get()), ["bob"]);
    assert.deepEqual(ids(await byAge.startAt(30, "dan").get()), ["dan", "cat"]);
    assert.deepEqual(ids(await byAge.startAfter(26).endBefore(35).get()), ["ann", "dan"]);
    assert.deepEqual(ids(await people.startAfter("bob").get()), ["cat", "dan", "eve"]);
    assert.deepEqual(ids(await people.orderBy("age", "desc").startAfter(35).endAt(30).get()), ["dan", "ann"]);
  });

  test("paginating with a document snapshot as cursor visits everything once", async () => {
    const col = db.collection(freshCollection("pages"));
    const batch = db.batch();
    for (let i = 0; i < 23; i++) batch.set(col.doc(`d${String(i).padStart(2, "0")}`), { group: i % 3, i });
    await batch.commit();

    for (const base of [col.orderBy("group"), col.orderBy("group", "desc"), col, col.where("group", "==", 1)]) {
      const seen = [];
      let page = await base.limit(5).get();
      while (!page.empty) {
        seen.push(...ids(page));
        page = await base.startAfter(page.docs[page.docs.length - 1]).limit(5).get();
      }
      const all = ids(await base.get());
      assert.deepEqual(seen, all);
      assert.equal(new Set(seen).size, seen.length);
    }
  });

  test("strings order by code point, not by UTF-16 unit", async () => {
    const col = db.collection(freshCollection("unicode"));
    // U+FF5E sorts after U+1F600 in UTF-16 but before it by code point.
    const batch = db.batch();
    batch.set(col.doc("a"), { s: "\u{1F600}" });
    batch.set(col.doc("b"), { s: "～" });
    batch.set(col.doc("c"), { s: "z" });
    await batch.commit();
    assert.deepEqual(ids(await col.orderBy("s").get()), ["c", "b", "a"]);
  });

  test("timestamps can be filtered and ordered", async () => {
    const col = db.collection(freshCollection("times"));
    const batch = db.batch();
    batch.set(col.doc("old"), { at: new Date("2020-01-01T00:00:00Z") });
    batch.set(col.doc("mid"), { at: new Date("2022-06-15T12:00:00Z") });
    batch.set(col.doc("new"), { at: new Date("2024-12-31T23:59:59Z") });
    await batch.commit();
    assert.deepEqual(ids(await col.where("at", ">", new Date("2021-01-01T00:00:00Z")).orderBy("at", "desc").get()), ["new", "mid"]);
    assert.deepEqual(ids(await col.where("at", "==", new Date("2022-06-15T12:00:00Z")).get()), ["mid"]);
  });

  test("nested fields can be queried", async () => {
    const col = db.collection(freshCollection("nested"));
    await col.doc("a").set({ address: { city: "Paris" } });
    await col.doc("b").set({ address: { city: "Lyon" } });
    assert.deepEqual(ids(await col.where("address.city", "==", "Lyon").get()), ["b"]);
    assert.deepEqual(ids(await col.orderBy("address.city").get()), ["b", "a"]);
  });

  test("an invalid query is refused", async () => {
    await assert.rejects(people.where("age", "~~", 1).get(), { code: "invalid_argument", status: 400 });
    await assert.rejects(people.where("city", "in", []).get(), { code: "invalid_argument" });
  });
});
