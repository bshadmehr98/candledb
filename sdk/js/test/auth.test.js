import assert from "node:assert/strict";
import { after, before, describe, test } from "node:test";

import { CandleDBError, createClient } from "../dist/index.js";
import { ADMIN_KEY, freshCollection, recorder, startServer, waitFor } from "./helpers.js";

let counter = 0;
const email = () => `user${process.pid}n${++counter}@example.com`;
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

async function whoAmI(url, token) {
  const res = await fetch(`${url}/v1/auth/me`, { headers: { Authorization: `Bearer ${token}` } });
  return res.ok ? (await res.json()).id : null;
}

// The server limits sign-in attempts per client address. Each test client
// therefore presents its own address, the way separate users behind a
// reverse proxy would (the server runs with -trust-proxy).
let addresses = 0;
function fromOwnAddress(inner = fetch) {
  const address = `10.0.${Math.floor(++addresses / 250)}.${(addresses % 250) + 1}`;
  return (input, init = {}) => inner(input, { ...init, headers: { ...init.headers, "X-Forwarded-For": address } });
}

describe("auth", () => {
  let server;
  const newClient = (options = {}) =>
    createClient({ url: server.url, ...options, fetch: fromOwnAddress(options.fetch) });
  before(async () => {
    server = await startServer({ args: ["-trust-proxy"] });
  });
  after(async () => {
    await server.stop();
  });

  test("sign up, sign out, sign in", async () => {
    const client = newClient();
    const address = email();
    assert.equal(client.auth.currentUser, null);
    assert.equal(await client.auth.getAccessToken(), null);

    const user = await client.auth.signUp(address.toUpperCase(), "correct horse");
    assert.equal(user.email, address, "emails are normalised to lower case");
    assert.ok(user.id);
    assert.ok(user.createdAt instanceof Date);
    assert.deepEqual(client.auth.currentUser, user);
    const token = await client.auth.getAccessToken();
    assert.equal(await whoAmI(server.url, token), user.id);

    await client.auth.signOut();
    assert.equal(client.auth.currentUser, null);
    assert.equal(await client.auth.getAccessToken(), null);
    await client.auth.signOut(); // signing out twice is harmless

    const again = await client.auth.signIn(address, "correct horse");
    assert.equal(again.id, user.id);
  });

  test("repeated attempts from one address are rate limited", async () => {
    const client = newClient();
    let limited = null;
    for (let i = 0; i < 15 && !limited; i++) {
      try {
        await client.auth.signIn("nobody@example.com", "wrong password");
      } catch (err) {
        if (err.code === "resource_exhausted") limited = err;
        else assert.equal(err.code, "unauthenticated");
      }
    }
    assert.ok(limited, "the limiter never engaged");
    assert.equal(limited.status, 429);
    // Another address is unaffected.
    await assert.rejects(newClient().auth.signIn("nobody@example.com", "wrong password"), { code: "unauthenticated" });
  });

  test("bad credentials and duplicate accounts are refused", async () => {
    const client = newClient();
    const address = email();
    await client.auth.signUp(address, "correct horse");
    await client.auth.signOut();

    await assert.rejects(client.auth.signIn(address, "wrong password"), (err) => {
      assert.ok(err instanceof CandleDBError);
      assert.equal(err.code, "unauthenticated");
      assert.equal(err.status, 401);
      return true;
    });
    await assert.rejects(client.auth.signIn("nobody@example.com", "whatever12"), { code: "unauthenticated" });
    await assert.rejects(client.auth.signUp(address, "another pass"), { code: "already_exists" });
    await assert.rejects(client.auth.signUp(email(), "short"), { code: "invalid_argument" });
    await assert.rejects(client.auth.signUp("not-an-email", "long enough"), { code: "invalid_argument" });
    assert.equal(client.auth.currentUser, null);
  });

  test("onAuthStateChanged reports the initial state and every change", async () => {
    const client = newClient();
    const seen = [];
    const stop = client.auth.onAuthStateChanged((user) => seen.push(user?.email ?? null));
    await waitFor(() => seen.length === 1);
    assert.deepEqual(seen, [null]);

    const address = email();
    await client.auth.signUp(address, "correct horse");
    await client.auth.signOut();
    assert.deepEqual(seen, [null, address, null]);

    stop();
    await client.auth.signIn(address, "correct horse");
    assert.deepEqual(seen, [null, address, null], "no calls after unsubscribing");
  });

  test("a stored session is restored by a new client", async () => {
    const store = new Map();
    const storage = {
      getItem: (k) => store.get(k) ?? null,
      setItem: (k, v) => void store.set(k, v),
      removeItem: (k) => void store.delete(k),
    };
    const first = newClient({ storage });
    const user = await first.auth.signUp(email(), "correct horse");
    assert.equal(store.size, 1);

    // An asynchronous storage works the same way.
    const asyncStorage = {
      getItem: async (k) => store.get(k) ?? null,
      setItem: async (k, v) => void store.set(k, v),
      removeItem: async (k) => void store.delete(k),
    };
    const second = newClient({ storage: asyncStorage });
    const seen = [];
    second.auth.onAuthStateChanged((u) => seen.push(u?.id ?? null));
    await second.auth.ready;
    assert.equal(second.auth.currentUser.id, user.id);
    assert.equal(await whoAmI(server.url, await second.auth.getAccessToken()), user.id);
    await waitFor(() => seen.length === 1);
    assert.deepEqual(seen, [user.id]);

    await second.auth.signOut();
    assert.equal(store.size, 0);
  });

  test("a rejected token is refreshed and the request repeated", async () => {
    let sabotage = false;
    let refreshes = 0;
    const flaky = async (input, init) => {
      const url = String(input);
      if (url.endsWith("/v1/auth/refresh")) refreshes++;
      if (sabotage && url.includes("/v1/documents/") && init?.headers?.Authorization) {
        sabotage = false; // only the next request gets a broken token
        init = { ...init, headers: { ...init.headers, Authorization: "Bearer not.a.token" } };
      }
      return fetch(input, init);
    };
    const client = newClient({ fetch: flaky });
    const user = await client.auth.signUp(email(), "correct horse");
    const ref = client.collection(freshCollection()).doc("d");
    await ref.set({ n: 1 });

    sabotage = true;
    const snap = await ref.get();
    assert.equal(snap.get("n"), 1);
    assert.equal(refreshes, 1);
    assert.equal(client.auth.currentUser.id, user.id, "the user stays signed in");
  });

  test("an invalid refresh token signs the user out", async () => {
    const store = new Map();
    const storage = {
      getItem: (k) => store.get(k) ?? null,
      setItem: (k, v) => void store.set(k, v),
      removeItem: (k) => void store.delete(k),
    };
    const client = newClient({ storage });
    await client.auth.signUp(email(), "correct horse");

    // Corrupt the stored session: an expired access token and a refresh
    // token the server does not know.
    const [key] = store.keys();
    const session = JSON.parse(store.get(key));
    store.set(key, JSON.stringify({ ...session, refreshToken: "bogus", expiresAt: Date.now() - 1000 }));

    const restored = newClient({ storage });
    const seen = [];
    restored.auth.onAuthStateChanged((u) => seen.push(u?.id ?? null));
    assert.equal(await restored.auth.getAccessToken(), null);
    assert.equal(restored.auth.currentUser, null);
    await waitFor(() => seen.at(-1) === null && seen.length >= 1);
    assert.equal(store.size, 0);
  });

  test("two tabs sharing storage do not sign each other out", async () => {
    const store = new Map();
    const storage = {
      getItem: (k) => store.get(k) ?? null,
      setItem: (k, v) => void store.set(k, v),
      removeItem: (k) => void store.delete(k),
    };
    const first = newClient({ storage });
    const user = await first.auth.signUp(email(), "correct horse");

    // Both tabs start from the same stored session, whose access token has
    // run out, so both will want to refresh with the same refresh token.
    const [key] = store.keys();
    store.set(key, JSON.stringify({ ...JSON.parse(store.get(key)), expiresAt: Date.now() - 1000 }));
    const tabA = newClient({ storage });
    const tabB = newClient({ storage });
    await Promise.all([tabA.auth.ready, tabB.auth.ready]);

    // Tab A refreshes, which uses the refresh token up. Tab B must pick up
    // A's new tokens from storage instead of failing with the old one.
    const tokenA = await tabA.auth.getAccessToken();
    const tokenB = await tabB.auth.getAccessToken();
    assert.equal(await whoAmI(server.url, tokenA), user.id);
    assert.equal(await whoAmI(server.url, tokenB), user.id);
    assert.equal(tabB.auth.currentUser?.id, user.id, "tab B stays signed in");
    assert.equal(store.size, 1, "the session is still stored");
  });

  test("the admin key authenticates server-side clients", async () => {
    const admin = newClient({ adminKey: ADMIN_KEY });
    const res = await fetch(`${server.url}/metrics`, {
      headers: { Authorization: `Bearer ${await admin.auth.bearer()}` },
    });
    assert.equal(res.status, 200);
    const ref = admin.collection(freshCollection()).doc("d");
    await ref.set({ by: "admin" });
    assert.equal((await ref.get()).get("by"), "admin");
  });
});

describe("auth with short-lived tokens", () => {
  let server;
  before(async () => {
    server = await startServer({ args: ["-access-ttl", "2s"] });
  });
  after(async () => {
    await server.stop();
  });

  test("the access token is renewed before it expires", async () => {
    const client = createClient({ url: server.url });
    const user = await client.auth.signUp(email(), "correct horse");
    const first = await client.auth.getAccessToken();
    assert.equal(await client.auth.getAccessToken(), first, "a fresh token is reused");

    await sleep(2100); // let the first token expire
    assert.equal(await whoAmI(server.url, first), null, "the old token no longer works");
    const second = await client.auth.getAccessToken();
    assert.notEqual(second, first);
    assert.equal(await whoAmI(server.url, second), user.id);

    // Many concurrent requests share one refresh (refresh tokens are single-use).
    await sleep(2100);
    const tokens = await Promise.all(Array.from({ length: 5 }, () => client.auth.getAccessToken()));
    assert.equal(new Set(tokens).size, 1);
    assert.equal(await whoAmI(server.url, tokens[0]), user.id);
  });

  test("a listener survives the expiry of the token its stream was opened with", async () => {
    const client = createClient({ url: server.url, reconnect: { minDelayMs: 20, maxDelayMs: 100 } });
    const admin = createClient({ url: server.url, adminKey: ADMIN_KEY });
    await client.auth.signUp(email(), "correct horse");
    const ref = client.collection(freshCollection()).doc("d");
    const rec = recorder();
    const stop = ref.onSnapshot(rec.onNext, rec.onError);
    try {
      await rec.until(() => true);
      await sleep(2500); // the server ends the stream when the token expires
      await admin.doc(ref.path).set({ n: 1 });
      await rec.until((s) => s.get("n") === 1, "the listener did not resume after token expiry");
      await sleep(2500);
      await admin.doc(ref.path).set({ n: 2 });
      await rec.until((s) => s.get("n") === 2, "the listener did not resume a second time");
      assert.deepEqual(rec.errors, []);
    } finally {
      stop();
      client.close();
    }
  });
});
