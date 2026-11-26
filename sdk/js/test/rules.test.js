import assert from "node:assert/strict";
import { after, before, describe, test } from "node:test";

import { CandleDBError, createClient } from "../dist/index.js";
import { ADMIN_KEY, recorder, startServer, waitFor } from "./helpers.js";

const RULES = `
function signedIn() { return request.auth != null; }

match /notes/{id} {
  allow read: if resource == null
              || resource.data.public == true
              || (signedIn() && resource.data.owner == request.auth.uid);
  allow create: if signedIn() && request.resource.data.owner == request.auth.uid;
  allow update, delete: if signedIn() && resource.data.owner == request.auth.uid;
}

match /board/{id} {
  allow read;
}

match /secrets/{id} {
  allow read: if false;
}
`;

const ids = (snapshot) => snapshot.docs.map((d) => d.id);
let counter = 0;

describe("security rules", () => {
  let server, admin, alice, bob, anon, aliceId, bobId;
  before(async () => {
    server = await startServer({ rules: RULES });
    admin = createClient({ url: server.url, adminKey: ADMIN_KEY });
    alice = createClient({ url: server.url });
    bob = createClient({ url: server.url });
    anon = createClient({ url: server.url });
    aliceId = (await alice.auth.signUp(`alice${process.pid}@example.com`, "correct horse")).id;
    bobId = (await bob.auth.signUp(`bob${process.pid}@example.com`, "correct horse")).id;
  });
  after(async () => {
    for (const c of [admin, alice, bob, anon]) c.close();
    await server.stop();
  });

  test("writes are checked against the rules", async () => {
    const id = `w${++counter}`;
    await assert.rejects(anon.doc(`notes/${id}`).set({ owner: "nobody" }), (err) => {
      assert.ok(err instanceof CandleDBError);
      assert.equal(err.code, "permission_denied");
      assert.equal(err.status, 403);
      return true;
    });
    await assert.rejects(alice.doc(`notes/${id}`).set({ owner: bobId }), { code: "permission_denied" });

    await alice.doc(`notes/${id}`).set({ owner: aliceId, text: "mine" });
    await alice.doc(`notes/${id}`).update({ text: "still mine" });
    await assert.rejects(bob.doc(`notes/${id}`).update({ text: "stolen" }), { code: "permission_denied" });
    await assert.rejects(bob.doc(`notes/${id}`).delete(), { code: "permission_denied" });
    await assert.rejects(alice.doc(`elsewhere/${id}`).set({ a: 1 }), { code: "permission_denied" }, "no rule means no access");

    assert.equal((await admin.doc(`notes/${id}`).get()).get("text"), "still mine");
    await alice.doc(`notes/${id}`).delete();
  });

  test("reads are checked against the rules", async () => {
    const id = `r${++counter}`;
    await alice.doc(`notes/${id}`).set({ owner: aliceId, text: "private" });
    assert.equal((await alice.doc(`notes/${id}`).get()).get("text"), "private");
    await assert.rejects(bob.doc(`notes/${id}`).get(), { code: "permission_denied" });
    await assert.rejects(anon.doc(`notes/${id}`).get(), { code: "permission_denied" });

    await alice.doc(`notes/${id}`).update({ public: true });
    assert.equal((await anon.doc(`notes/${id}`).get()).get("text"), "private");

    await admin.doc("secrets/s1").set({ code: 42 });
    await assert.rejects(alice.doc("secrets/s1").get(), { code: "permission_denied" });
    // The same answer whether or not the document exists, so nothing leaks.
    await assert.rejects(alice.doc("secrets/does-not-exist").get(), { code: "permission_denied" });
    assert.equal((await admin.doc("secrets/s1").get()).get("code"), 42, "the admin key bypasses rules");
  });

  test("a query returns only the documents the caller may read", async () => {
    const tag = `q${++counter}`;
    await alice.doc(`notes/${tag}-a1`).set({ owner: aliceId, tag, n: 1 });
    await alice.doc(`notes/${tag}-a2`).set({ owner: aliceId, tag, n: 2, public: true });
    await bob.doc(`notes/${tag}-b1`).set({ owner: bobId, tag, n: 3 });
    await bob.doc(`notes/${tag}-b2`).set({ owner: bobId, tag, n: 4 });

    const query = (client) => client.collection("notes").where("tag", "==", tag).orderBy("n");
    assert.deepEqual(ids(await query(alice).get()), [`${tag}-a1`, `${tag}-a2`]);
    assert.deepEqual(ids(await query(bob).get()), [`${tag}-a2`, `${tag}-b1`, `${tag}-b2`]);
    assert.deepEqual(ids(await query(anon).get()), [`${tag}-a2`]);
    assert.equal((await query(admin).get()).size, 4);
    // The limit is filled with readable documents, not cut short by hidden ones.
    assert.deepEqual(ids(await query(bob).orderBy("owner").limit(2).get()).length, 2);
    assert.deepEqual(ids(await query(bob).limit(2).get()), [`${tag}-a2`, `${tag}-b1`]);
    assert.equal((await alice.collection("secrets").get()).empty, true);
  });

  test("a query listener only ever shows readable documents", async () => {
    const tag = `l${++counter}`;
    const rec = recorder();
    const stop = bob.collection("notes").where("tag", "==", tag).onSnapshot(rec.onNext, rec.onError);
    try {
      await rec.until(() => true);
      await alice.doc(`notes/${tag}-private`).set({ owner: aliceId, tag });
      await alice.doc(`notes/${tag}-public`).set({ owner: aliceId, tag, public: true });
      let snap = await rec.until((s) => s.size === 1);
      assert.deepEqual(ids(snap), [`${tag}-public`]);

      // Becoming unreadable looks like a removal; becoming readable like an addition.
      await alice.doc(`notes/${tag}-public`).update({ public: false });
      await rec.until((s) => s.empty);
      await alice.doc(`notes/${tag}-private`).update({ public: true });
      snap = await rec.until((s) => s.size === 1);
      assert.deepEqual(ids(snap), [`${tag}-private`]);
      assert.deepEqual(rec.errors, []);
    } finally {
      stop();
    }
  });

  test("listening to a forbidden document reports an error", async () => {
    await admin.doc("secrets/s2").set({ code: 1 });
    const rec = recorder();
    const stop = alice.doc("secrets/s2").onSnapshot(rec.onNext, rec.onError);
    try {
      await waitFor(() => rec.errors.length === 1, { message: "the listener error never arrived" });
      assert.ok(rec.errors[0] instanceof CandleDBError);
      assert.equal(rec.errors[0].code, "permission_denied");
      assert.equal(rec.snapshots.length, 0);
    } finally {
      stop();
    }
  });

  test("a rejected local write is rolled back in listeners", async () => {
    await admin.doc("board/notice").set({ text: "official" });
    const docRec = recorder();
    const queryRec = recorder();
    const stopDoc = alice.doc("board/notice").onSnapshot(docRec.onNext, docRec.onError);
    const stopQuery = alice.collection("board").onSnapshot(queryRec.onNext, queryRec.onError);
    try {
      await docRec.until((s) => s.get("text") === "official");
      await queryRec.until((s) => s.size === 1);

      // Readable but not writable: the write shows locally, then is undone.
      const write = alice.doc("board/notice").update({ text: "graffiti" });
      assert.equal(docRec.last.get("text"), "graffiti");
      assert.equal(docRec.last.metadata.hasPendingWrites, true);
      assert.equal(queryRec.last.docs[0].get("text"), "graffiti");

      await assert.rejects(write, { code: "permission_denied" });
      assert.equal(docRec.last.get("text"), "official");
      assert.equal(docRec.last.metadata.hasPendingWrites, false);
      assert.equal(queryRec.last.docs[0].get("text"), "official");
      assert.equal(queryRec.last.metadata.hasPendingWrites, false);

      const create = alice.doc("board/spam").set({ text: "buy now" });
      assert.deepEqual(ids(queryRec.last), ["notice", "spam"]);
      await assert.rejects(create, { code: "permission_denied" });
      assert.deepEqual(ids(queryRec.last), ["notice"]);
      assert.deepEqual(queryRec.last.docChanges().map((c) => [c.type, c.doc.id]), [["removed", "spam"]]);
    } finally {
      stopDoc();
      stopQuery();
    }
  });

  test("signing in and out changes what a listener may see", async () => {
    const client = createClient({ url: server.url, reconnect: { minDelayMs: 20, maxDelayMs: 100 } });
    const tag = `s${++counter}`;
    await alice.doc(`notes/${tag}-own`).set({ owner: aliceId, tag });
    await alice.doc(`notes/${tag}-pub`).set({ owner: aliceId, tag, public: true });
    const rec = recorder();
    const stop = client.collection("notes").where("tag", "==", tag).onSnapshot(rec.onNext, rec.onError);
    try {
      assert.deepEqual(ids(await rec.until(() => true)), [`${tag}-pub`]);
      await client.auth.signIn(`alice${process.pid}@example.com`, "correct horse");
      assert.deepEqual(ids(await rec.until((s) => s.size === 2)), [`${tag}-own`, `${tag}-pub`]);
      await client.auth.signOut();
      assert.deepEqual(ids(await rec.until((s) => s.size === 1)), [`${tag}-pub`]);
      assert.deepEqual(rec.errors, []);
    } finally {
      stop();
      client.close();
    }
  });
});
