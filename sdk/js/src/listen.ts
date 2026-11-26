/**
 * Realtime listeners.
 *
 * All listeners of a client share one stream (GET /v1/listen, Server-Sent
 * Events). The stream is opened when the first listener is added and closed
 * when the last one is removed. Each listener is a "target" on the server's
 * session; its results arrive on the shared stream tagged with its ID.
 *
 * What a listener shows is not simply what the server sent. It is
 *
 *     view = limit(sort(filter(serverDocs + pending local writes)))
 *
 * recomputed after every server event and every local write, and compared
 * with the previous view so the application is only called when something
 * it can see has changed. That one rule gives latency compensation (a local
 * write shows at once), rollback (a rejected write disappears again) and
 * quiet reconnects (a fresh snapshot that equals the old view is silent).
 */
import type { Auth } from "./auth.js";
import type { FetchLike, Http } from "./http.js";
import { applyLocally, type WireWrite } from "./mutations.js";
import { parentOf, queryToWire, runLocally, type QuerySpec } from "./query.js";
import { CandleDBError, type WireDoc } from "./types.js";

export type TargetSpec = { document: string } | { query: QuerySpec };

/** One document of a view, with whether a local write is still unconfirmed. */
export interface ViewEntry {
  doc: WireDoc;
  pending: boolean;
}

export interface ViewChange {
  type: "added" | "modified" | "removed";
  entry: ViewEntry;
}

export interface ViewUpdate {
  entries: ViewEntry[];
  changes: ViewChange[];
  /** True while any local write affecting this view awaits confirmation. */
  hasPendingWrites: boolean;
}

export interface ReconnectOptions {
  minDelayMs?: number;
  maxDelayMs?: number;
  /**
   * How long the stream may stay completely silent before it is assumed
   * dead and reopened. The server sends a keep-alive every 20 seconds, so
   * silence much longer than that means the connection is broken in a way
   * the network stack has not noticed. Default 60 seconds.
   */
  idleTimeoutMs?: number;
}

/** A local write that the server has not yet confirmed to every listener. */
export interface PendingMutation {
  writes: WireWrite[];
  time: Date;
  /** Set when the server accepted the commit. */
  version?: number;
}

interface ServerEvent {
  type: "ready" | "snapshot" | "change" | "error";
  session?: string;
  target?: number;
  version?: number;
  changes?: { kind: "added" | "modified" | "removed"; doc?: WireDoc; path?: string }[];
  code?: string;
  message?: string;
}

interface Target {
  id: number;
  spec: TargetSpec;
  onUpdate: (u: ViewUpdate) => void;
  onError: (err: CandleDBError) => void;
  /** The documents the server has told us are in the result. */
  serverDocs: Map<string, WireDoc>;
  /** The database version the server has brought this target up to. */
  version: number;
  /** False until the first snapshot arrives; nothing is shown before. */
  synced: boolean;
  /** Events held back while one of our own commits is in flight. */
  held: ServerEvent[];
  /** The session this target was registered on, if any. */
  session: string | null;
  last: Map<string, { entry: ViewEntry; signature: string }> | null;
  lastPending: boolean;
}

export class ListenManager {
  private readonly targets = new Map<number, Target>();
  private pending: PendingMutation[] = [];
  private nextTargetId = 1;

  private session: string | null = null;
  /** Incremented to invalidate a running connection loop. */
  private generation = 0;
  private running = false;
  private abort: AbortController | null = null;
  private wake: (() => void) | null = null;
  private readonly minDelay: number;
  private readonly maxDelay: number;
  private readonly idleTimeout: number;

  constructor(
    private readonly http: Http,
    private readonly fetchFn: FetchLike,
    private readonly auth: Auth,
    reconnect: ReconnectOptions = {},
  ) {
    this.minDelay = reconnect.minDelayMs ?? 500;
    this.maxDelay = reconnect.maxDelayMs ?? 30_000;
    this.idleTimeout = reconnect.idleTimeoutMs ?? 60_000;
  }

  // ---- listeners ----

  listen(spec: TargetSpec, onUpdate: (u: ViewUpdate) => void, onError: (err: CandleDBError) => void): () => void {
    const target: Target = {
      id: this.nextTargetId++,
      spec,
      onUpdate,
      onError,
      serverDocs: new Map(),
      version: -1,
      synced: false,
      held: [],
      session: null,
      last: null,
      lastPending: false,
    };
    this.targets.set(target.id, target);
    if (this.running) this.register(target);
    else this.start();
    return () => this.unlisten(target);
  }

  private unlisten(target: Target): void {
    if (!this.targets.delete(target.id)) return;
    if (this.session !== null && target.session === this.session) {
      this.http.request("DELETE", `/v1/listen/${this.session}/targets/${target.id}`).catch(() => {});
    }
    this.prunePending();
    if (this.targets.size === 0) this.stop();
  }

  /** Ends every listener and the stream. */
  close(): void {
    this.targets.clear();
    this.pending = [];
    this.stop();
  }

  /** Reopens the stream under the current identity (after sign-in or sign-out). */
  identityChanged(): void {
    if (!this.running) return;
    this.stop();
    this.start();
  }

  // ---- local writes ----

  /** Shows a local write in every affected listener straight away. */
  addPending(m: PendingMutation): void {
    if (this.targets.size === 0) return;
    this.pending.push(m);
    this.refresh(m);
  }

  /** The server accepted the write at this version. */
  confirmPending(m: PendingMutation, version: number): void {
    m.version = version;
    this.settle(m);
  }

  /** The write failed: listeners fall back to what the server has. */
  rejectPending(m: PendingMutation): void {
    this.pending = this.pending.filter((p) => p !== m);
    this.settle(m);
  }

  /** After a commit resolved either way: release held events, then redraw. */
  private settle(m: PendingMutation): void {
    for (const target of [...this.targets.values()]) {
      if (!this.affects(target, m)) continue;
      if (!this.hasUnanswered(target)) {
        const held = target.held;
        target.held = [];
        for (const ev of held) this.apply(target, ev);
      }
      this.render(target);
    }
    this.prunePending();
  }

  private refresh(m: PendingMutation): void {
    for (const target of [...this.targets.values()]) {
      if (this.affects(target, m)) this.render(target);
    }
  }

  private touches(target: Target, path: string): boolean {
    return "document" in target.spec ? target.spec.document === path : target.spec.query.collection === parentOf(path);
  }

  private affects(target: Target, m: PendingMutation): boolean {
    return m.writes.some((w) => this.touches(target, w.path));
  }

  /** Whether a commit touching this target is still waiting for its HTTP response. */
  private hasUnanswered(target: Target): boolean {
    return this.pending.some((m) => m.version === undefined && this.affects(target, m));
  }

  /**
   * Forgets mutations no listener needs any more: those the server
   * confirmed and every affected listener has caught up with.
   */
  private prunePending(): void {
    this.pending = this.pending.filter((m) => {
      if (m.version === undefined) return true;
      for (const target of this.targets.values()) {
        if (this.affects(target, m) && target.version < m.version) return true;
      }
      return false;
    });
  }

  // ---- server events ----

  private handle(ev: ServerEvent): void {
    const target = ev.target === undefined ? undefined : this.targets.get(ev.target);
    if (!target) return; // a listener that was removed in the meantime
    // While one of our own commits is in flight we cannot tell whether an
    // incoming event already includes it. Applying the event now and the
    // local write on top could count the write twice (think increment), so
    // the event waits for the commit's response.
    if (this.hasUnanswered(target)) {
      target.held.push(ev);
      return;
    }
    this.apply(target, ev);
    this.render(target);
    this.prunePending();
  }

  private apply(target: Target, ev: ServerEvent): void {
    if (ev.type === "snapshot") {
      target.serverDocs = new Map();
      target.synced = true;
    }
    for (const change of ev.changes ?? []) {
      // Changes are idempotent: "added" and "modified" both mean "this is
      // the document now", and removing an unknown document is a no-op.
      if (change.kind === "removed") {
        if (change.path) target.serverDocs.delete(change.path);
      } else if (change.doc) {
        target.serverDocs.set(change.doc.path, change.doc);
      }
    }
    if (ev.version !== undefined) target.version = Math.max(target.version, ev.version);
  }

  /** Recomputes a target's view and notifies the application if it changed. */
  private render(target: Target): void {
    if (!target.synced) return;

    const docs = new Map(target.serverDocs);
    const pendingPaths = new Set<string>();
    for (const m of this.pending) {
      // Confirmed and already reflected in what the server sent us.
      if (m.version !== undefined && target.version >= m.version) continue;
      for (const w of m.writes) {
        if (!this.touches(target, w.path)) continue;
        const before = docs.get(w.path);
        const after = applyLocally(before, w, m.time);
        if (!before && !after) continue;
        if (after) docs.set(w.path, after);
        else docs.delete(w.path);
        pendingPaths.add(w.path);
      }
    }

    const visible =
      "document" in target.spec
        ? [docs.get(target.spec.document)].filter((d): d is WireDoc => d !== undefined)
        : runLocally(target.spec.query, docs.values());

    const next = new Map<string, { entry: ViewEntry; signature: string }>();
    const entries: ViewEntry[] = [];
    const changes: ViewChange[] = [];
    for (const doc of visible) {
      const entry = { doc, pending: pendingPaths.has(doc.path) };
      const signature = `${doc.version}|${entry.pending ? 1 : 0}|${JSON.stringify(doc.data)}`;
      entries.push(entry);
      next.set(doc.path, { entry, signature });
      const before = target.last?.get(doc.path);
      if (!before) changes.push({ type: "added", entry });
      else if (before.signature !== signature) changes.push({ type: "modified", entry });
    }
    for (const [path, before] of target.last ?? []) {
      if (!next.has(path)) changes.push({ type: "removed", entry: before.entry });
    }

    const hasPendingWrites = pendingPaths.size > 0;
    const first = target.last === null;
    const changed = first || changes.length > 0 || hasPendingWrites !== target.lastPending;
    target.last = next;
    target.lastPending = hasPendingWrites;
    if (!changed) return;
    try {
      target.onUpdate({ entries, changes, hasPendingWrites });
    } catch (err) {
      // An exception in application code must not break the stream.
      console.error("candledb: error in snapshot listener", err);
    }
  }

  // ---- the stream ----

  private start(): void {
    this.running = true;
    void this.run(++this.generation);
  }

  private stop(): void {
    this.running = false;
    this.generation++;
    this.session = null;
    this.abort?.abort();
    this.wake?.();
    for (const target of this.targets.values()) target.session = null;
  }

  /** Tells the server about a target, once there is a session to add it to. */
  private register(target: Target): void {
    const session = this.session;
    if (session === null) return; // done for all targets when the stream is ready
    target.session = session;
    const body = { id: target.id, ...("document" in target.spec ? target.spec : { query: queryToWire(target.spec.query) }) };
    this.http.request("POST", `/v1/listen/${session}/targets`, body).catch((err: CandleDBError) => {
      if (!this.targets.has(target.id) || this.session !== session) return;
      if (err.status === 0 || err.status === 404 || err.status === 410) {
        this.abort?.abort(); // the session is gone: reconnect and start over
        return;
      }
      // The server refused this listener (bad query, no permission).
      this.targets.delete(target.id);
      target.onError(err);
      if (this.targets.size === 0) this.stop();
    });
  }

  /**
   * The connection loop: connect, read events until the stream ends, wait,
   * repeat. It runs until stop() bumps the generation.
   */
  private async run(generation: number): Promise<void> {
    let delay = this.minDelay;
    let justRenewed = false;
    while (generation === this.generation) {
      const abort = new AbortController();
      this.abort = abort;
      let renew = false;
      // The watchdog aborts the request if nothing at all, not even a
      // keep-alive, arrives for too long.
      let watchdog: ReturnType<typeof setTimeout> | undefined;
      const alive = () => {
        clearTimeout(watchdog);
        watchdog = setTimeout(() => abort.abort(), this.idleTimeout);
      };
      try {
        alive();
        const headers: Record<string, string> = { Accept: "text/event-stream" };
        const bearer = await this.auth.bearer();
        if (bearer) headers["Authorization"] = `Bearer ${bearer}`;
        const res = await this.fetchFn(`${this.http.baseUrl}/v1/listen`, { headers, signal: abort.signal });
        if (res.status === 401) {
          renew = true;
        } else if (res.ok && res.body) {
          for await (const ev of readEvents(res.body, alive)) {
            if (generation !== this.generation) return;
            if (ev.type === "ready" && ev.session) {
              delay = this.minDelay;
              this.session = ev.session;
              for (const target of this.targets.values()) {
                target.held = [];
                this.register(target);
              }
            } else if (ev.type === "error") {
              renew = ev.code === "unauthenticated";
              break;
            } else {
              this.handle(ev);
            }
          }
        }
      } catch {
        // Network failure or abort: fall through to reconnect.
      } finally {
        // Also runs on the early return above, so the timer can never
        // outlive the connection it watches.
        clearTimeout(watchdog);
      }
      abort.abort();
      if (generation !== this.generation) return;
      this.session = null;

      // With a fresh token, reconnect at once. Doing so only once in a row
      // keeps a server that rejects even fresh tokens from being hammered.
      if (renew && !justRenewed && (await this.auth.renew())) {
        justRenewed = true;
        continue;
      }
      justRenewed = false;
      if (generation !== this.generation) return;

      // Exponential backoff with jitter, so that many clients cut off at the
      // same moment do not all come back at the same moment.
      await new Promise<void>((resolve) => {
        const timer = setTimeout(resolve, delay * (0.5 + Math.random() * 0.5));
        this.wake = () => {
          clearTimeout(timer);
          resolve();
        };
      });
      this.wake = null;
      delay = Math.min(delay * 2, this.maxDelay);
    }
  }
}

/**
 * Parses a Server-Sent Events body into the JSON of its "data:" messages.
 * `onBytes` is called whenever anything arrives, keep-alives included.
 */
async function* readEvents(body: ReadableStream<Uint8Array>, onBytes: () => void): AsyncGenerator<ServerEvent> {
  const reader = body.getReader();
  const decoder = new TextDecoder();
  let buffer = "";
  try {
    for (;;) {
      const { done, value } = await reader.read();
      if (done) return;
      onBytes();
      buffer += decoder.decode(value, { stream: true });
      // Messages are separated by a blank line.
      for (let end = buffer.indexOf("\n\n"); end !== -1; end = buffer.indexOf("\n\n")) {
        const block = buffer.slice(0, end);
        buffer = buffer.slice(end + 2);
        const data = block
          .split("\n")
          .filter((line) => line.startsWith("data:")) // lines starting with ":" are keep-alives
          .map((line) => line.slice(5).trimStart())
          .join("\n");
        if (data) yield JSON.parse(data) as ServerEvent;
      }
    }
  } finally {
    reader.cancel().catch(() => {});
  }
}
