// Fixtures are what Playwright calls the things a test needs before it can
// start. This file adds one: `app`, a running candledb server with the todo
// example's rules, plus a static file server for the example's page.
import { test as base, expect } from "@playwright/test";
import { createServer } from "node:http";
import { readFile } from "node:fs/promises";
import { dirname, extname, join, normalize, resolve, sep } from "node:path";
import { fileURLToPath } from "node:url";
import { startServer } from "../../sdk/js/test/helpers.js";

const repoRoot = resolve(dirname(fileURLToPath(import.meta.url)), "../..");

const contentTypes = {
  ".html": "text/html; charset=utf-8",
  ".js": "text/javascript; charset=utf-8",
  ".map": "application/json",
};

/** Serves the repository's files, as "python3 -m http.server" would. */
function startStaticServer() {
  const server = createServer(async (req, res) => {
    try {
      let path = decodeURIComponent(new URL(req.url, "http://x").pathname);
      if (path.endsWith("/")) path += "index.html";
      const file = normalize(join(repoRoot, path));
      if (!file.startsWith(repoRoot + sep)) throw new Error("outside the repository");
      const body = await readFile(file);
      res.writeHead(200, { "Content-Type": contentTypes[extname(file)] ?? "application/octet-stream" });
      res.end(body);
    } catch {
      res.writeHead(404).end("not found");
    }
  });
  return new Promise((ready) => {
    server.listen(0, "127.0.0.1", () => {
      ready({
        url: `http://127.0.0.1:${server.address().port}`,
        // close() waits for open connections to end, and a browser keeps
        // its connections open for reuse; close them from this side.
        stop: () =>
          new Promise((done) => {
            server.close(done);
            server.closeAllConnections();
          }),
      });
    });
  });
}

let counter = 0;

export const test = base.extend({
  // { scope: "worker" } starts the servers once per worker process instead
  // of once per test. Tests stay independent because each one signs up its
  // own user, and the rules give every user a private list.
  app: [
    async ({}, use) => {
      const rules = await readFile(join(repoRoot, "examples/todo/candledb.rules"), "utf8");
      // The app signs up a new user in every test, all from one address.
      const database = await startServer({ rules, args: ["-auth-rate", "1000", "-auth-burst", "1000"] });
      const files = await startStaticServer();
      await use({
        database,
        /** The address of the example page, pointed at this test's server. */
        pageUrl: `${files.url}/examples/todo/?server=${encodeURIComponent(database.url)}`,
        /** A new email address on every call. */
        newEmail: () => `user${Date.now()}-${counter++}@example.com`,
      });
      await files.stop();
      await database.stop();
    },
    { scope: "worker" },
  ],
});

export { expect };

export const PASSWORD = "correct horse battery";

/** Opens the example and creates an account. Returns the email used. */
export async function signUp(page, app, email = app.newEmail()) {
  await page.goto(app.pageUrl);
  await page.getByPlaceholder("email").fill(email);
  await page.getByPlaceholder("password").fill(PASSWORD);
  await page.getByRole("button", { name: "Create account" }).click();
  await expect(page.locator("#who")).toHaveText(email);
  return email;
}

/** Opens the example and signs in to an existing account. */
export async function signIn(page, app, email) {
  await page.goto(app.pageUrl);
  await page.getByPlaceholder("email").fill(email);
  await page.getByPlaceholder("password").fill(PASSWORD);
  await page.getByRole("button", { name: "Sign in" }).click();
  await expect(page.locator("#who")).toHaveText(email);
}

/** Adds a todo and waits until the server has confirmed it. */
export async function addTodo(page, title) {
  await page.getByPlaceholder("What needs doing?").fill(title);
  await page.getByRole("button", { name: "Add" }).click();
  await expect(todo(page, title)).toBeVisible();
  // "pending" marks an optimistic write the server has not confirmed yet.
  await expect(todo(page, title)).not.toHaveClass(/pending/);
}

/** The list item of the todo with this title. */
export function todo(page, title) {
  return page.locator("#list li").filter({ hasText: title });
}
