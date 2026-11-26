/**
 * Queries: their description, their wire form, and a local implementation
 * of what a query selects. The local implementation mirrors the server's
 * domain.Query so that realtime results (including optimistic local writes)
 * can be filtered, sorted and limited on the client.
 */
import type { WireDoc, WireValue } from "./types.js";
import { arrayContains, compareStrings, compareValues, getField, valuesEqual } from "./values.js";

export type WhereOp =
  | "=="
  | "!="
  | "<"
  | "<="
  | ">"
  | ">="
  | "in"
  | "not-in"
  | "array-contains"
  | "array-contains-any";

export type OrderDirection = "asc" | "desc";

export interface FilterSpec {
  field: string;
  op: WhereOp;
  value: WireValue;
}

export interface OrderSpec {
  field: string;
  direction: OrderDirection;
}

export interface CursorSpec {
  values: WireValue[];
  inclusive: boolean;
}

export interface QuerySpec {
  collection: string;
  where: FilterSpec[];
  orderBy: OrderSpec[];
  limit: number; // 0 = no limit
  startAt?: CursorSpec;
  endAt?: CursorSpec;
}

/** The collection a document path belongs to. */
export function parentOf(path: string): string {
  return path.slice(0, path.lastIndexOf("/"));
}

export function idOf(path: string): string {
  return path.slice(path.lastIndexOf("/") + 1);
}

/** The JSON body of POST /v1/query and of a query listen target. */
export function queryToWire(q: QuerySpec): Record<string, unknown> {
  const out: Record<string, unknown> = { collection: q.collection };
  if (q.where.length) out.where = q.where;
  if (q.orderBy.length) out.orderBy = q.orderBy;
  if (q.limit > 0) out.limit = q.limit;
  if (q.startAt) out.startAt = q.startAt;
  if (q.endAt) out.endAt = q.endAt;
  return out;
}

function filterMatches(f: FilterSpec, doc: WireDoc): boolean {
  const v = getField(doc.data, f.field);
  if (v === undefined) return false;
  switch (f.op) {
    case "==":
      return valuesEqual(v, f.value);
    case "!=":
      return !valuesEqual(v, f.value);
    case "<":
    case "<=":
    case ">":
    case ">=": {
      // Ordering operators only match values of the same kind; comparing
      // across kinds yields a non-zero result that must not count.
      if (!sameKind(v, f.value)) return false;
      const c = compareValues(v, f.value);
      return f.op === "<" ? c < 0 : f.op === "<=" ? c <= 0 : f.op === ">" ? c > 0 : c >= 0;
    }
    case "in":
      return arrayContains(f.value as WireValue[], v);
    case "not-in":
      return !arrayContains(f.value as WireValue[], v);
    case "array-contains":
      return Array.isArray(v) && arrayContains(v, f.value);
    case "array-contains-any":
      return Array.isArray(v) && (f.value as WireValue[]).some((want) => arrayContains(v, want));
  }
}

function sameKind(a: WireValue, b: WireValue): boolean {
  return kindTag(a) === kindTag(b);
}

function kindTag(v: WireValue): string {
  if (v === null) return "null";
  if (Array.isArray(v)) return "array";
  if (typeof v === "object") {
    const keys = Object.keys(v);
    return keys.length === 1 && keys[0] === "$timestamp" ? "timestamp" : "map";
  }
  return typeof v;
}

/** Whether the document passes the filters and has every ordered field. */
export function matches(q: QuerySpec, doc: WireDoc): boolean {
  if (parentOf(doc.path) !== q.collection) return false;
  for (const f of q.where) if (!filterMatches(f, doc)) return false;
  for (const o of q.orderBy) if (getField(doc.data, o.field) === undefined) return false;
  return true;
}

function idDescending(q: QuerySpec): boolean {
  const last = q.orderBy[q.orderBy.length - 1];
  return last !== undefined && last.direction === "desc";
}

/** Orders two documents that both match the query. */
export function compareDocs(q: QuerySpec, a: WireDoc, b: WireDoc): number {
  for (const o of q.orderBy) {
    const c = compareValues(getField(a.data, o.field)!, getField(b.data, o.field)!);
    if (c !== 0) return o.direction === "desc" ? -c : c;
  }
  const c = compareStrings(a.id, b.id);
  return idDescending(q) ? -c : c;
}

function compareToCursor(q: QuerySpec, doc: WireDoc, cursor: CursorSpec): number {
  for (let i = 0; i < cursor.values.length; i++) {
    const cv = cursor.values[i]!;
    const order = q.orderBy[i];
    if (order === undefined) {
      const c = compareStrings(doc.id, String(cv));
      return idDescending(q) ? -c : c;
    }
    const c = compareValues(getField(doc.data, order.field)!, cv);
    if (c !== 0) return order.direction === "desc" ? -c : c;
  }
  return 0;
}

function inWindow(q: QuerySpec, doc: WireDoc): boolean {
  if (q.startAt) {
    const c = compareToCursor(q, doc, q.startAt);
    if (c < 0 || (c === 0 && !q.startAt.inclusive)) return false;
  }
  if (q.endAt) {
    const c = compareToCursor(q, doc, q.endAt);
    if (c > 0 || (c === 0 && !q.endAt.inclusive)) return false;
  }
  return true;
}

/** Runs the query over candidate documents: filter, sort, window, limit. */
export function runLocally(q: QuerySpec, candidates: Iterable<WireDoc>): WireDoc[] {
  const out: WireDoc[] = [];
  for (const doc of candidates) {
    if (matches(q, doc) && inWindow(q, doc)) out.push(doc);
  }
  out.sort((a, b) => compareDocs(q, a, b));
  return q.limit > 0 && out.length > q.limit ? out.slice(0, q.limit) : out;
}
