/**
 * Writes: their wire form, and a local estimate of their effect.
 *
 * applyLocally mirrors what the server does with a write (see
 * internal/domain/write.go and the parsing in internal/adapters/httpapi/
 * wire.go). It is what lets a listener show a write immediately, before the
 * server has confirmed it.
 */
import type { WireDoc, WireMap, WireValue } from "./types.js";
import { TRANSFORM_KEYS, arrayContains, formatTimestamp, isPlainMap } from "./values.js";
import { idOf } from "./query.js";

export interface WirePrecondition {
  exists?: boolean;
  version?: number;
}

/** One entry of the "writes" array of POST /v1/commit. */
export interface WireWrite {
  op: "set" | "create" | "update" | "delete" | "verify";
  path: string;
  data?: WireMap;
  merge?: boolean;
  precondition?: WirePrecondition;
}

interface Transform {
  key: string;
  arg: WireValue;
}

/** Recognises a transform object such as {"$increment": 1}. */
function asTransform(v: WireValue): Transform | null {
  if (v === null || typeof v !== "object" || Array.isArray(v)) return null;
  const keys = Object.keys(v);
  if (keys.length !== 1 || !TRANSFORM_KEYS.has(keys[0]!)) return null;
  return { key: keys[0]!, arg: v[keys[0]!]! };
}

function getAt(data: WireMap, path: string[]): WireValue | undefined {
  let cur: WireValue | undefined = data;
  for (const name of path) {
    if (!isPlainMap(cur) || !Object.hasOwn(cur, name)) return undefined;
    cur = cur[name];
  }
  return cur;
}

function setAt(data: WireMap, path: string[], value: WireValue): void {
  let cur = data;
  for (const name of path.slice(0, -1)) {
    const next = cur[name];
    if (isPlainMap(next)) {
      cur = next;
    } else {
      const fresh: WireMap = {};
      cur[name] = fresh;
      cur = fresh;
    }
  }
  cur[path[path.length - 1]!] = value;
}

function deleteAt(data: WireMap, path: string[]): void {
  let cur = data;
  for (const name of path.slice(0, -1)) {
    const next = cur[name];
    if (!isPlainMap(next)) return;
    cur = next;
  }
  delete cur[path[path.length - 1]!];
}

/** Applies one transform (or plain value) to the field at path. */
function applyField(data: WireMap, path: string[], value: WireValue, now: Date): void {
  const t = asTransform(value);
  if (t === null) {
    setAt(data, path, structuredClone(value));
    return;
  }
  const prev = getAt(data, path);
  switch (t.key) {
    case "$delete":
      deleteAt(data, path);
      return;
    case "$serverTimestamp":
      // An estimate: the server will stamp its own commit time.
      setAt(data, path, { $timestamp: formatTimestamp(now) });
      return;
    case "$increment":
      setAt(data, path, (typeof prev === "number" ? prev : 0) + (t.arg as number));
      return;
    case "$arrayUnion": {
      const out = Array.isArray(prev) ? [...prev] : [];
      for (const e of t.arg as WireValue[]) if (!arrayContains(out, e)) out.push(e);
      setAt(data, path, out);
      return;
    }
    case "$arrayRemove": {
      const old = Array.isArray(prev) ? prev : [];
      setAt(data, path, old.filter((e) => !arrayContains(t.arg as WireValue[], e)));
      return;
    }
  }
}

/**
 * Walks write data, applying every leaf to `target`. With `flatten` (a
 * merge) nested maps are descended into so that sibling fields survive;
 * without it only transforms are lifted out and everything else is taken
 * as it stands.
 */
function applyTree(target: WireMap, tree: WireMap, prefix: string[], flatten: boolean, now: Date): void {
  for (const name of Object.keys(tree).sort()) {
    const child = tree[name]!;
    const path = [...prefix, name];
    if (asTransform(child) === null && isPlainMap(child) && (Object.keys(child).length > 0 || !flatten)) {
      if (!flatten) setAt(target, path, {});
      applyTree(target, child, path, flatten, now);
    } else {
      applyField(target, path, child, now);
    }
  }
}

/**
 * Returns the document a write would produce, or undefined if the result
 * is "no document". `existing` is not modified.
 */
export function applyLocally(existing: WireDoc | undefined, w: WireWrite, now: Date): WireDoc | undefined {
  let data: WireMap;
  switch (w.op) {
    case "delete":
      return undefined;
    case "verify":
      return existing; // a verify only checks; it changes nothing
    case "update":
      // The server rejects an update of a missing document, so locally it
      // has no effect either.
      if (!existing) return undefined;
      data = structuredClone(existing.data);
      for (const key of Object.keys(w.data ?? {}).sort()) {
        applyField(data, key.split("."), w.data![key]!, now);
      }
      break;
    default:
      if (w.op === "set" && w.merge) {
        data = existing ? structuredClone(existing.data) : {};
        applyTree(data, w.data ?? {}, [], true, now);
      } else {
        data = {};
        applyTree(data, w.data ?? {}, [], false, now);
      }
  }
  const stamp = formatTimestamp(now);
  return {
    path: w.path,
    id: idOf(w.path),
    data,
    createTime: existing?.createTime ?? stamp,
    updateTime: stamp,
    // The real version is only known once the server confirms the write.
    version: existing?.version ?? 0,
  };
}
