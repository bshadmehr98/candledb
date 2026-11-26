/**
 * The public face of the SDK: the client, document and collection
 * references, queries, snapshots, batches and transactions.
 */
import { Auth, type AuthStorage } from "./auth.js";
import { Http, encodePath, type FetchLike } from "./http.js";
import { ListenManager, type PendingMutation, type ReconnectOptions, type ViewEntry, type ViewUpdate } from "./listen.js";
import type { WirePrecondition, WireWrite } from "./mutations.js";
import { queryToWire, type CursorSpec, type OrderDirection, type QuerySpec, type WhereOp } from "./query.js";
import { CandleDBError, type DocumentData, type Unsubscribe, type WireDoc } from "./types.js";
import { decodeValue, encodeMap, encodeValue, getField } from "./values.js";

export interface ClientOptions {
  /** Base URL of the server, e.g. "http://localhost:8090". */
  url: string;
  /** A fetch implementation; defaults to the global one. */
  fetch?: FetchLike;
  /** Where to keep the signed-in session; defaults to memory only. */
  storage?: AuthStorage;
  /** The server's admin key. Bypasses security rules; never ship it to browsers. */
  adminKey?: string;
  /** Timing of realtime reconnection attempts. */
  reconnect?: ReconnectOptions;
}

export interface SnapshotMetadata {
  /** True while the snapshot includes local writes the server has not confirmed. */
  readonly hasPendingWrites: boolean;
}

export interface SetOptions {
  /** Merge the given fields into the document instead of replacing it. */
  merge?: boolean;
}

export interface TransactionOptions {
  /** How often the transaction function may run before giving up (default 5). */
  maxAttempts?: number;
}

const ID_ALPHABET = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789";

/** A random 20-character document ID, generated on the client. */
function newId(): string {
  // 62 * 4 = 248: bytes at or above that are discarded so that every
  // character stays equally likely.
  let id = "";
  while (id.length < 20) {
    const bytes = crypto.getRandomValues(new Uint8Array(32));
    for (const b of bytes) {
      if (b < 248 && id.length < 20) id += ID_ALPHABET[b % 62];
    }
  }
  return id;
}

function checkPath(path: string, kind: "document" | "collection"): string {
  const segments = path.split("/");
  const wantEven = kind === "document";
  if (path === "" || segments.some((s) => s === "") || (segments.length % 2 === 0) !== wantEven) {
    throw new CandleDBError("invalid_argument", `"${path}" is not a valid ${kind} path`);
  }
  return path;
}

/** Creates a client for the candledb server at `options.url`. */
export function createClient(options: ClientOptions): CandleDBClient {
  return new CandleDBClient(options);
}

export class CandleDBClient {
  readonly auth: Auth;
  /** @internal */ readonly _http: Http;
  /** @internal */ readonly _listen: ListenManager;

  constructor(options: ClientOptions) {
    const baseUrl = options.url.replace(/\/+$/, "");
    // Calling fetch through a wrapper keeps its `this` binding intact in
    // browsers, where a detached window.fetch throws.
    const fetchFn: FetchLike = options.fetch ?? ((input, init) => fetch(input, init));
    this.auth = new Auth(baseUrl, fetchFn, options.storage ?? null, options.adminKey);
    this._http = new Http(baseUrl, fetchFn, this.auth);
    this._listen = new ListenManager(this._http, fetchFn, this.auth, options.reconnect);
    this.auth._onIdentityChange = () => this._listen.identityChanged();
  }

  /** A reference to the document at `path` ("users/alice"). */
  doc(path: string): DocumentReference {
    return new DocumentReference(this, checkPath(path, "document"));
  }

  /** A reference to the collection at `path` ("users", "users/alice/posts"). */
  collection(path: string): CollectionReference {
    return new CollectionReference(this, checkPath(path, "collection"));
  }

  /** Starts a batch of writes that is committed atomically. */
  batch(): WriteBatch {
    return new WriteBatch(this);
  }

  /**
   * Runs `fn` as a transaction: everything it reads through `tx.get` is
   * guaranteed unchanged when its writes are committed. If another client
   * changed one of those documents in the meantime, the commit is refused
   * and `fn` runs again from the start, so it must not have side effects
   * of its own.
   *
   * The guarantee covers every document read through `tx.get`, whether or
   * not the transaction goes on to write it.
   */
  async runTransaction<T>(fn: (tx: Transaction) => Promise<T>, options: TransactionOptions = {}): Promise<T> {
    const maxAttempts = options.maxAttempts ?? 5;
    for (let attempt = 1; ; attempt++) {
      const tx = new Transaction(this);
      const result = await fn(tx);
      try {
        await tx._commit();
        return result;
      } catch (err) {
        const conflict =
          err instanceof CandleDBError && (err.code === "failed_precondition" || err.code === "already_exists");
        if (!conflict || attempt >= maxAttempts) throw err;
        // A short random pause lets the competing writers spread out.
        await new Promise((resolve) => setTimeout(resolve, Math.random() * 20 * attempt));
      }
    }
  }

  /** Stops every listener and closes the realtime stream. */
  close(): void {
    this._listen.close();
  }

  /** @internal Sends writes, showing them in local listeners meanwhile. */
  async _commit(writes: WireWrite[]): Promise<void> {
    if (writes.length === 0) return;
    const pending: PendingMutation = { writes, time: new Date() };
    this._listen.addPending(pending);
    try {
      const res = await this._http.request<{ version: number }>("POST", "/v1/commit", { writes });
      this._listen.confirmPending(pending, res.version);
    } catch (err) {
      this._listen.rejectPending(pending);
      throw err;
    }
  }

  /** @internal */
  async _get(path: string): Promise<DocumentSnapshot> {
    const ref = new DocumentReference(this, path);
    try {
      const doc = await this._http.request<WireDoc>("GET", `/v1/documents/${encodePath(path)}`);
      return new DocumentSnapshot(ref, doc, false);
    } catch (err) {
      if (err instanceof CandleDBError && err.code === "not_found") return new DocumentSnapshot(ref, null, false);
      throw err;
    }
  }
}

// ---- snapshots ----

export class DocumentSnapshot<T extends DocumentData = DocumentData> {
  readonly metadata: SnapshotMetadata;

  /** @internal */
  constructor(
    readonly ref: DocumentReference,
    /** @internal */ readonly _doc: WireDoc | null,
    pending: boolean,
  ) {
    this.metadata = { hasPendingWrites: pending };
  }

  get exists(): boolean {
    return this._doc !== null;
  }
  get id(): string {
    return this.ref.id;
  }
  get path(): string {
    return this.ref.path;
  }
  /**
   * The version of the last server commit that changed the document, or 0
   * if it does not exist. While a local write is pending this is still the
   * version of the confirmed state underneath.
   */
  get version(): number {
    return this._doc?.version ?? 0;
  }
  get createTime(): Date | null {
    return this._doc ? new Date(this._doc.createTime) : null;
  }
  get updateTime(): Date | null {
    return this._doc ? new Date(this._doc.updateTime) : null;
  }

  /** The document's fields, or undefined if it does not exist. */
  data(): T | undefined {
    return this._doc ? (decodeValue(this._doc.data) as T) : undefined;
  }

  /** One field by dotted path ("address.city"), or undefined. */
  get(field: string): any {
    if (!this._doc) return undefined;
    const v = getField(this._doc.data, field);
    return v === undefined ? undefined : decodeValue(v);
  }
}

export interface DocumentChange<T extends DocumentData = DocumentData> {
  type: "added" | "modified" | "removed";
  doc: DocumentSnapshot<T>;
}

export class QuerySnapshot<T extends DocumentData = DocumentData> {
  readonly metadata: SnapshotMetadata;

  /** @internal */
  constructor(
    readonly docs: DocumentSnapshot<T>[],
    private readonly changes: DocumentChange<T>[],
    pending: boolean,
  ) {
    this.metadata = { hasPendingWrites: pending };
  }

  get size(): number {
    return this.docs.length;
  }
  get empty(): boolean {
    return this.docs.length === 0;
  }

  /** What changed since the previous snapshot (everything is "added" in the first). */
  docChanges(): DocumentChange<T>[] {
    return this.changes;
  }

  forEach(fn: (doc: DocumentSnapshot<T>) => void): void {
    this.docs.forEach(fn);
  }
}

// ---- references ----

export class DocumentReference<T extends DocumentData = DocumentData> {
  /** @internal */
  constructor(
    /** @internal */ readonly _client: CandleDBClient,
    readonly path: string,
  ) {}

  get id(): string {
    return this.path.slice(this.path.lastIndexOf("/") + 1);
  }

  /** The collection this document belongs to. */
  get parent(): CollectionReference<T> {
    return new CollectionReference<T>(this._client, this.path.slice(0, this.path.lastIndexOf("/")));
  }

  /** A subcollection of this document. */
  collection(name: string): CollectionReference {
    return this._client.collection(`${this.path}/${name}`);
  }

  get(): Promise<DocumentSnapshot<T>> {
    return this._client._get(this.path) as Promise<DocumentSnapshot<T>>;
  }

  /** Writes the document, replacing it unless `merge` is set. */
  async set(data: T, options: SetOptions = {}): Promise<void> {
    await this._client._commit([writeSet(this.path, data, options)]);
  }

  /**
   * Changes fields of an existing document. Keys may be dotted paths:
   * `update({"address.city": "Paris"})`. Fails with "not_found" if the
   * document does not exist.
   */
  async update(data: DocumentData): Promise<void> {
    await this._client._commit([{ op: "update", path: this.path, data: encodeMap(data) }]);
  }

  async delete(): Promise<void> {
    await this._client._commit([{ op: "delete", path: this.path }]);
  }

  /**
   * Calls `onNext` with the document now and again after every change,
   * until the returned function is called.
   */
  onSnapshot(onNext: (snapshot: DocumentSnapshot<T>) => void, onError?: (err: CandleDBError) => void): Unsubscribe {
    return this._client._listen.listen(
      { document: this.path },
      (u) => onNext(new DocumentSnapshot<T>(this, u.entries[0]?.doc ?? null, u.hasPendingWrites)),
      onError ?? defaultOnError,
    );
  }
}

function defaultOnError(err: CandleDBError): void {
  console.error("candledb: listener failed", err);
}

function writeSet(path: string, data: DocumentData, options: SetOptions): WireWrite {
  const w: WireWrite = { op: "set", path, data: encodeMap(data) };
  if (options.merge) w.merge = true;
  return w;
}

/**
 * A query over one collection. Queries are immutable: every builder method
 * returns a new Query and leaves the original unchanged.
 */
export class Query<T extends DocumentData = DocumentData> {
  /** @internal */
  constructor(
    /** @internal */ readonly _client: CandleDBClient,
    /** @internal */ readonly _spec: QuerySpec,
  ) {}

  private with(change: Partial<QuerySpec>): Query<T> {
    return new Query<T>(this._client, { ...this._spec, ...change });
  }

  where(field: string, op: WhereOp, value: unknown): Query<T> {
    return this.with({ where: [...this._spec.where, { field, op, value: encodeValue(value) }] });
  }

  orderBy(field: string, direction: OrderDirection = "asc"): Query<T> {
    return this.with({ orderBy: [...this._spec.orderBy, { field, direction }] });
  }

  limit(n: number): Query<T> {
    if (!Number.isInteger(n) || n < 1) throw new CandleDBError("invalid_argument", "limit must be a positive integer");
    return this.with({ limit: n });
  }

  /**
   * Cursors position the query within its ordered results. Pass one value
   * per orderBy field, optionally followed by a document ID; or pass a
   * DocumentSnapshot to continue exactly from that document.
   */
  startAt(...values: unknown[]): Query<T> {
    return this.with({ startAt: this.cursor(values, true) });
  }
  startAfter(...values: unknown[]): Query<T> {
    return this.with({ startAt: this.cursor(values, false) });
  }
  endAt(...values: unknown[]): Query<T> {
    return this.with({ endAt: this.cursor(values, true) });
  }
  endBefore(...values: unknown[]): Query<T> {
    return this.with({ endAt: this.cursor(values, false) });
  }

  private cursor(values: unknown[], inclusive: boolean): CursorSpec {
    const first = values[0];
    if (values.length === 1 && first instanceof DocumentSnapshot) {
      const doc = first._doc;
      if (!doc) throw new CandleDBError("invalid_argument", "cannot use a missing document as a cursor");
      const fields = this._spec.orderBy.map((o) => {
        const v = getField(doc.data, o.field);
        if (v === undefined) {
          throw new CandleDBError("invalid_argument", `cursor document has no field "${o.field}"`);
        }
        return v;
      });
      return { values: [...fields, doc.id], inclusive };
    }
    if (values.length === 0) throw new CandleDBError("invalid_argument", "a cursor needs at least one value");
    return { values: values.map(encodeValue), inclusive };
  }

  async get(): Promise<QuerySnapshot<T>> {
    const res = await this._client._http.request<{ documents: WireDoc[] }>("POST", "/v1/query", queryToWire(this._spec));
    const docs = res.documents.map((d) => new DocumentSnapshot<T>(new DocumentReference<T>(this._client, d.path), d, false));
    return new QuerySnapshot<T>(
      docs,
      docs.map((doc) => ({ type: "added", doc })),
      false,
    );
  }

  /**
   * Calls `onNext` with the query's results now and again after every
   * change to them, until the returned function is called.
   */
  onSnapshot(onNext: (snapshot: QuerySnapshot<T>) => void, onError?: (err: CandleDBError) => void): Unsubscribe {
    const snap = (e: ViewEntry) => new DocumentSnapshot<T>(new DocumentReference<T>(this._client, e.doc.path), e.doc, e.pending);
    return this._client._listen.listen(
      { query: this._spec },
      (u: ViewUpdate) =>
        onNext(
          new QuerySnapshot<T>(
            u.entries.map(snap),
            u.changes.map((c) => ({ type: c.type, doc: snap(c.entry) })),
            u.hasPendingWrites,
          ),
        ),
      onError ?? defaultOnError,
    );
  }
}

export class CollectionReference<T extends DocumentData = DocumentData> extends Query<T> {
  /** @internal */
  constructor(
    client: CandleDBClient,
    readonly path: string,
  ) {
    super(client, { collection: path, where: [], orderBy: [], limit: 0 });
  }

  get id(): string {
    return this.path.slice(this.path.lastIndexOf("/") + 1);
  }

  /** A document in this collection; without an ID, a new random one. */
  doc(id?: string): DocumentReference<T> {
    return new DocumentReference<T>(this._client, checkPath(`${this.path}/${id ?? newId()}`, "document"));
  }

  /** Creates a document with a generated ID and returns its reference. */
  async add(data: T): Promise<DocumentReference<T>> {
    const ref = this.doc();
    await this._client._commit([{ op: "create", path: ref.path, data: encodeMap(data) }]);
    return ref;
  }
}

// ---- batches and transactions ----

/** Collects writes and sends them as one atomic commit. */
export class WriteBatch {
  /** @internal */ protected readonly _writes: WireWrite[] = [];
  private committed = false;

  /** @internal */
  constructor(/** @internal */ protected readonly _client: CandleDBClient) {}

  set(ref: DocumentReference, data: DocumentData, options: SetOptions = {}): this {
    this._writes.push(writeSet(ref.path, data, options));
    return this;
  }

  /** Like set, but the commit fails if the document already exists. */
  create(ref: DocumentReference, data: DocumentData): this {
    this._writes.push({ op: "create", path: ref.path, data: encodeMap(data) });
    return this;
  }

  update(ref: DocumentReference, data: DocumentData): this {
    this._writes.push({ op: "update", path: ref.path, data: encodeMap(data) });
    return this;
  }

  delete(ref: DocumentReference): this {
    this._writes.push({ op: "delete", path: ref.path });
    return this;
  }

  /** Applies all writes, or none of them if any is refused. */
  async commit(): Promise<void> {
    if (this.committed) throw new CandleDBError("failed_precondition", "this batch has already been committed");
    this.committed = true;
    await this._client._commit(this._writes);
  }
}

/** The handle a transaction function reads and writes through. */
export class Transaction extends WriteBatch {
  /** What each document looked like when the transaction read it. */
  private readonly reads = new Map<string, WirePrecondition>();

  /** Reads a document and remembers its version for the commit check. */
  async get<T extends DocumentData>(ref: DocumentReference<T>): Promise<DocumentSnapshot<T>> {
    if (this._writes.length > 0) {
      throw new CandleDBError("failed_precondition", "a transaction must do all its reads before its writes");
    }
    const snapshot = await ref.get();
    this.reads.set(ref.path, snapshot.exists ? { version: snapshot.version } : { exists: false });
    return snapshot;
  }

  /** @internal */
  async _commit(): Promise<void> {
    // The precondition goes on the first write to each document: that is
    // the one checked against the stored state. Later writes in the batch
    // see the result of the earlier ones.
    const guarded = new Set<string>();
    for (const w of this._writes) {
      const pre = this.reads.get(w.path);
      if (pre && !guarded.has(w.path)) {
        w.precondition = pre;
        guarded.add(w.path);
      }
    }
    // A transaction that writes nothing has nothing to protect: its reads
    // are its result.
    if (this._writes.length === 0) return;
    // Documents that were read but not written get a "verify" write, which
    // changes nothing and only makes the commit fail if they have changed.
    // They go first so they are checked against the stored state.
    const verifies: WireWrite[] = [];
    for (const [path, precondition] of this.reads) {
      if (!guarded.has(path)) verifies.push({ op: "verify", path, precondition });
    }
    this._writes.unshift(...verifies);
    await this.commit();
  }
}
