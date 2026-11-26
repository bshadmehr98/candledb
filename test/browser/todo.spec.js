// Drives the todo example (examples/todo/index.html) in real browsers,
// against a real candledb server. These are the only tests in which the
// client runs where it is meant to run: a browser, with its own fetch,
// streams, CORS checks and localStorage.
import { test, expect, signUp, signIn, addTodo, todo, PASSWORD } from "./fixtures.js";

test("sign up, add, complete and delete a todo", async ({ page, app }) => {
  await signUp(page, app);
  await expect(page.locator("#empty")).toBeVisible();

  await addTodo(page, "Buy milk");
  await expect(page.locator("#empty")).toBeHidden();

  await page.getByLabel("Done: Buy milk").check();
  await expect(todo(page, "Buy milk")).toHaveClass(/done/);

  await todo(page, "Buy milk").getByRole("button", { name: "Delete" }).click();
  await expect(todo(page, "Buy milk")).toHaveCount(0);
  await expect(page.locator("#empty")).toBeVisible();
  await expect(page.locator("#error")).toBeEmpty();
});

test("todos are listed in the order they were added", async ({ page, app }) => {
  await signUp(page, app);
  for (const title of ["first", "second", "third"]) await addTodo(page, title);
  await expect(page.locator("#list li span")).toHaveText(["first", "second", "third"]);
});

test("the session and the data survive a page reload", async ({ page, app }) => {
  const email = await signUp(page, app);
  await addTodo(page, "Still here");

  await page.reload();
  // No sign-in form: the client restored the session from localStorage.
  await expect(page.locator("#who")).toHaveText(email);
  await expect(todo(page, "Still here")).toBeVisible();
});

test("signing out and in again", async ({ page, app }) => {
  const email = await signUp(page, app);
  await addTodo(page, "Remember me");

  await page.getByRole("link", { name: "sign out" }).click();
  await expect(page.locator("#signed-out")).toBeVisible();
  await expect(page.locator("#signed-in")).toBeHidden();

  await signIn(page, app, email);
  await expect(todo(page, "Remember me")).toBeVisible();
});

test("a wrong password is reported and does not sign in", async ({ page, app }) => {
  const email = await signUp(page, app);
  await page.getByRole("link", { name: "sign out" }).click();

  await page.getByPlaceholder("email").fill(email);
  await page.getByPlaceholder("password").fill("not the password");
  await page.getByRole("button", { name: "Sign in" }).click();

  await expect(page.locator("#error")).not.toBeEmpty();
  await expect(page.locator("#signed-out")).toBeVisible();
});

test("a change in one window appears in another", async ({ browser, app }) => {
  // Two contexts are two separate browser profiles: nothing is shared
  // between them except the server.
  const first = await (await browser.newContext()).newPage();
  const second = await (await browser.newContext()).newPage();
  const email = await signUp(first, app);
  await signIn(second, app, email);

  await addTodo(first, "Seen everywhere");
  await expect(todo(second, "Seen everywhere")).toBeVisible();

  await second.getByLabel("Done: Seen everywhere").check();
  await expect(todo(first, "Seen everywhere")).toHaveClass(/done/);

  await first.getByRole("button", { name: "Delete" }).click();
  await expect(todo(second, "Seen everywhere")).toHaveCount(0);
});

test("one user cannot see another user's todos", async ({ browser, app }) => {
  const alice = await (await browser.newContext()).newPage();
  const bob = await (await browser.newContext()).newPage();
  await signUp(alice, app);
  await signUp(bob, app);

  await addTodo(alice, "Alice's secret");
  await addTodo(bob, "Bob's own");

  await expect(bob.locator("#list li")).toHaveCount(1);
  await expect(todo(bob, "Alice's secret")).toHaveCount(0);

  // The page only ever asks for its own list. Ask the server for Alice's
  // list directly, to check that it is the rules that keep Bob out and not
  // the page's good manners.
  const session = (page) => page.evaluate(() => JSON.parse(localStorage.getItem("candledb:session")));
  const aliceSession = await session(alice);
  const bobSession = await session(bob);
  const queryAlicesTodos = async (token) => {
    const response = await bob.request.post(`${app.database.url}/v1/query`, {
      headers: { Authorization: `Bearer ${token}` },
      data: { collection: `users/${aliceSession.user.id}/todos` },
    });
    expect(response.status()).toBe(200);
    return (await response.json()).documents;
  };
  // With Alice's token the request works, which proves it is well formed.
  expect(await queryAlicesTodos(aliceSession.accessToken)).toHaveLength(1);
  // With Bob's token the same request finds nothing.
  expect(await queryAlicesTodos(bobSession.accessToken)).toHaveLength(0);
});

test("a write the rules refuse is rolled back and reported", async ({ page, app }) => {
  await signUp(page, app);
  await addTodo(page, "Short enough");

  // The page limits titles to 200 characters, and so do the rules. Remove
  // the page's limit, as anyone can with the browser's developer tools.
  await page.locator("#title").evaluate((input) => input.removeAttribute("maxlength"));
  await page.getByPlaceholder("What needs doing?").fill("x".repeat(201));
  await page.getByRole("button", { name: "Add" }).click();

  await expect(page.locator("#error")).not.toBeEmpty();
  // The optimistic copy is gone again; only the valid todo remains.
  await expect(page.locator("#list li")).toHaveCount(1);
  await expect(todo(page, "Short enough")).toBeVisible();
});

test("titles are shown as text, never as HTML", async ({ page, app }) => {
  await signUp(page, app);
  const title = '<img src=x onerror="window.hacked = true">';
  await addTodo(page, title);

  await expect(page.locator("#list li span")).toHaveText(title);
  await expect(page.locator("#list img")).toHaveCount(0);
  expect(await page.evaluate(() => window.hacked)).toBeUndefined();
});

test("the list recovers when the server restarts", async ({ page, app }) => {
  await signUp(page, app);
  await addTodo(page, "Before the crash");

  await app.database.kill(); // as a crash would: no goodbye to the browser
  await app.database.start();

  // The same page, not reloaded: the client reconnects by itself.
  await addTodo(page, "After the restart");
  await expect(page.locator("#list li span")).toHaveText(["Before the crash", "After the restart"]);
});
