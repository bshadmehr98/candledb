// Shared test helpers: starting a real candledb server and waiting for
// conditions without fixed sleeps.
import { spawn, execFileSync } from "node:child_process";
import { existsSync, mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { createServer } from "node:net";
import { tmpdir } from "node:os";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

const repoRoot = resolve(dirname(fileURLToPath(import.meta.url)), "../../..");

export const ADMIN_KEY = "test-admin-key-0123456789abcdef";

/** Path of the server binary; built on demand if it is missing. */
function serverBinary() {
  if (process.env.CANDLEDB_BIN) return process.env.CANDLEDB_BIN;
  const bin = join(repoRoot, "bin", "candledb");
  if (!existsSync(bin)) {
    execFileSync("go", ["build", "-o", bin, "./cmd/candledb"], { cwd: repoRoot, stdio: "inherit" });
  }
  return bin;
}

function freePort() {
  return new Promise((resolvePort, reject) => {
    const srv = createServer();
    srv.on("error", reject);
    srv.listen(0, "127.0.0.1", () => {
      const { port } = srv.address();
      srv.close(() => resolvePort(port));
    });
  });
}

/**
 * Starts a server on a free port with its own temporary data directory.
 * `rules` is the text of a rules file; without it the server runs with
 * -dev-open-rules. Returns { url, stop, kill, start }.
 */
export async function startServer({ rules, args = [] } = {}) {
  const dataDir = mkdtempSync(join(tmpdir(), "candledb-sdk-test-"));
  const port = await freePort();
  const url = `http://127.0.0.1:${port}`;
  const flags = ["serve", "-addr", `127.0.0.1:${port}`, "-data-dir", dataDir, "-admin-key", ADMIN_KEY, "-log-level", "warn"];
  if (rules === undefined) {
    flags.push("-dev-open-rules");
  } else {
    const file = join(dataDir, "test.rules");
    writeFileSync(file, rules);
    flags.push("-rules", file);
  }
  flags.push(...args);

  let child = null;
  let log = "";

  async function start() {
    child = spawn(serverBinary(), flags, { stdio: ["ignore", "ignore", "pipe"] });
    child.stderr.on("data", (chunk) => (log += chunk));
    const exited = new Promise((r) => child.once("exit", r));
    child.exited = exited;
    await waitFor(
      async () => {
        if (child.exitCode !== null) throw new Error(`server exited early:\n${log}`);
        try {
          return (await fetch(`${url}/healthz`)).ok;
        } catch {
          return false;
        }
      },
      { timeout: 10_000, message: "server did not become healthy" },
    );
  }

  /** Stops the process abruptly, as a crash would. */
  async function kill() {
    if (!child || child.exitCode !== null) return;
    child.kill("SIGKILL");
    await child.exited;
  }

  async function stop() {
    if (child && child.exitCode === null && child.signalCode === null) {
      child.kill("SIGTERM");
      // Graceful shutdown waits up to five seconds for connections that
      // were opened but never used (an aborted request can leave one in
      // the HTTP client's pool). Tests need not wait that long.
      const timer = setTimeout(() => child.kill("SIGKILL"), 500);
      await child.exited;
      clearTimeout(timer);
    }
    rmSync(dataDir, { recursive: true, force: true });
  }

  await start();
  return { url, start, kill, stop, log: () => log };
}

/**
 * Polls `check` until it returns a truthy value, which is then returned.
 * Fails with a descriptive error when the timeout passes first.
 */
export async function waitFor(check, { timeout = 5000, message = "condition was not met in time" } = {}) {
  const deadline = Date.now() + timeout;
  for (;;) {
    const value = await check();
    if (value) return value;
    if (Date.now() > deadline) throw new Error(message);
    await new Promise((r) => setTimeout(r, 10));
  }
}

/**
 * Collects the snapshots a listener delivers, so a test can wait for a
 * particular one and inspect the whole sequence.
 */
export function recorder() {
  const snapshots = [];
  const errors = [];
  return {
    snapshots,
    errors,
    onNext: (s) => snapshots.push(s),
    onError: (e) => errors.push(e),
    get last() {
      return snapshots[snapshots.length - 1];
    },
    /** Waits until the latest snapshot satisfies `pred` and returns it. */
    until(pred, message) {
      return waitFor(
        () => {
          const last = snapshots[snapshots.length - 1];
          return last !== undefined && pred(last) ? last : false;
        },
        { message: message ?? "listener never reached the expected state" },
      );
    },
  };
}

let unique = 0;
/** A collection name that no other test uses. */
export function freshCollection(prefix = "c") {
  return `${prefix}${process.pid}x${++unique}`;
}
