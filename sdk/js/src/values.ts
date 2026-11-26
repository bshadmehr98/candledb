/**
 * Values: converting between what applications use (Date, sentinels) and
 * the wire format, and comparing wire values in the server's order.
 *
 * Internally the SDK keeps documents in wire form and only decodes them when
 * the application asks for `data()`. That keeps timestamps at the server's
 * microsecond precision for sorting, which a JS Date (milliseconds) would
 * lose.
 */
import { CandleDBError, type WireMap, type WireValue } from "./types.js";

const TIMESTAMP = "$timestamp";

// ---- sentinels ----

/**
 * A placeholder for a value the server computes when it commits the write.
 * Create one with serverTimestamp(), increment(), arrayUnion(),
 * arrayRemove() or deleteField().
 */
export class FieldSentinel {
  /** @internal */
  constructor(
    /** @internal */ readonly _key: string,
    /** @internal */ readonly _arg: unknown,
  ) {}
}

/** The time at which the server commits the write. */
export function serverTimestamp(): FieldSentinel {
  return new FieldSentinel("$serverTimestamp", true);
}

/** Adds `n` to the field's current number (a missing field counts as 0). */
export function increment(n: number): FieldSentinel {
  if (!Number.isFinite(n)) throw new CandleDBError("invalid_argument", "increment needs a finite number");
  return new FieldSentinel("$increment", n);
}

/** Appends the elements that the array does not contain yet. */
export function arrayUnion(...elements: unknown[]): FieldSentinel {
  return new FieldSentinel("$arrayUnion", elements);
}

/** Removes every occurrence of the given elements from the array. */
export function arrayRemove(...elements: unknown[]): FieldSentinel {
  return new FieldSentinel("$arrayRemove", elements);
}

/** Removes the field. Only valid in update() and set(..., {merge: true}). */
export function deleteField(): FieldSentinel {
  return new FieldSentinel("$delete", true);
}

export const TRANSFORM_KEYS = new Set(["$serverTimestamp", "$increment", "$arrayUnion", "$arrayRemove", "$delete"]);

// ---- encoding and decoding ----

/** Formats a Date in the server's timestamp layout (six fractional digits). */
export function formatTimestamp(d: Date): string {
  if (Number.isNaN(d.getTime())) throw new CandleDBError("invalid_argument", "invalid Date");
  return d.toISOString().replace(/Z$/, "000Z");
}

export function isTimestamp(v: WireValue): v is { $timestamp: string } {
  if (v === null || typeof v !== "object" || Array.isArray(v)) return false;
  const keys = Object.keys(v);
  return keys.length === 1 && keys[0] === TIMESTAMP;
}

export function isPlainMap(v: WireValue | undefined): v is WireMap {
  return v !== null && v !== undefined && typeof v === "object" && !Array.isArray(v) && !isTimestamp(v);
}

/** Converts an application value into its wire form. */
export function encodeValue(v: unknown): WireValue {
  if (v === null) return null;
  switch (typeof v) {
    case "boolean":
    case "string":
      return v;
    case "number":
      if (!Number.isFinite(v)) throw new CandleDBError("invalid_argument", "numbers must be finite");
      return v;
    case "object":
      break;
    default:
      throw new CandleDBError("invalid_argument", `values of type ${typeof v} cannot be stored`);
  }
  if (v instanceof Date) return { [TIMESTAMP]: formatTimestamp(v) };
  if (v instanceof FieldSentinel) {
    const arg = Array.isArray(v._arg) ? v._arg.map(encodeValue) : (v._arg as WireValue);
    return { [v._key]: arg };
  }
  if (Array.isArray(v)) return v.map((e) => (e === undefined ? null : encodeValue(e)));
  return encodeMap(v as Record<string, unknown>);
}

/** Converts an application object into its wire form. Undefined fields are skipped. */
export function encodeMap(obj: Record<string, unknown>): WireMap {
  if (obj === null || typeof obj !== "object" || Array.isArray(obj) || obj instanceof Date) {
    throw new CandleDBError("invalid_argument", "document data must be an object");
  }
  const out: WireMap = {};
  for (const [k, v] of Object.entries(obj)) {
    if (v !== undefined) out[k] = encodeValue(v);
  }
  return out;
}

/** Converts a wire value into an application value (timestamps become Dates). */
export function decodeValue(v: WireValue): any {
  if (v === null || typeof v !== "object") return v;
  if (Array.isArray(v)) return v.map(decodeValue);
  if (isTimestamp(v)) return new Date(v.$timestamp);
  const out: Record<string, unknown> = {};
  for (const [k, e] of Object.entries(v)) out[k] = decodeValue(e);
  return out;
}

// ---- field paths ----

/** Reads the field at a dotted path, or undefined if it does not exist. */
export function getField(data: WireMap, path: string): WireValue | undefined {
  let cur: WireValue | undefined = data;
  for (const name of path.split(".")) {
    if (!isPlainMap(cur) || !Object.hasOwn(cur, name)) return undefined;
    cur = cur[name];
  }
  return cur;
}

// ---- ordering ----

/** Kinds sort in this order when a field holds mixed types. */
function kindOf(v: WireValue): number {
  if (v === null) return 0;
  switch (typeof v) {
    case "boolean":
      return 1;
    case "number":
      return 2;
    case "string":
      return 4;
  }
  if (Array.isArray(v)) return 5;
  return isTimestamp(v) ? 3 : 6;
}

/**
 * Compares strings by Unicode code point, which is the server's order
 * (UTF-8 bytes). JavaScript's own `<` compares UTF-16 units, which puts
 * characters outside the Basic Multilingual Plane (emoji, for instance)
 * before U+E000..U+FFFF instead of after them.
 */
export function compareStrings(a: string, b: string): number {
  const n = Math.min(a.length, b.length);
  for (let i = 0; i < n; i++) {
    const ca = a.charCodeAt(i);
    const cb = b.charCodeAt(i);
    if (ca === cb) continue;
    const sa = ca >= 0xd800 && ca <= 0xdfff;
    const sb = cb >= 0xd800 && cb <= 0xdfff;
    if (sa !== sb) return sa ? 1 : -1; // a surrogate stands for a code point >= U+10000
    return ca < cb ? -1 : 1;
  }
  return a.length === b.length ? 0 : a.length < b.length ? -1 : 1;
}

/** Orders two wire values the way the server's domain.Compare does. */
export function compareValues(a: WireValue, b: WireValue): number {
  const ka = kindOf(a);
  const kb = kindOf(b);
  if (ka !== kb) return ka < kb ? -1 : 1;
  switch (ka) {
    case 0:
      return 0;
    case 1:
      return a === b ? 0 : a === false ? -1 : 1;
    case 2:
      return (a as number) === (b as number) ? 0 : (a as number) < (b as number) ? -1 : 1;
    case 3:
      // The layout is fixed-width, so text order is chronological order.
      return compareStrings((a as { $timestamp: string }).$timestamp, (b as { $timestamp: string }).$timestamp);
    case 4:
      return compareStrings(a as string, b as string);
    case 5: {
      const aa = a as WireValue[];
      const bb = b as WireValue[];
      for (let i = 0; i < Math.min(aa.length, bb.length); i++) {
        const c = compareValues(aa[i]!, bb[i]!);
        if (c !== 0) return c;
      }
      return aa.length === bb.length ? 0 : aa.length < bb.length ? -1 : 1;
    }
    default: {
      const am = a as WireMap;
      const bm = b as WireMap;
      const ak = Object.keys(am).sort(compareStrings);
      const bk = Object.keys(bm).sort(compareStrings);
      for (let i = 0; i < Math.min(ak.length, bk.length); i++) {
        const c = compareStrings(ak[i]!, bk[i]!) || compareValues(am[ak[i]!]!, bm[bk[i]!]!);
        if (c !== 0) return c;
      }
      return ak.length === bk.length ? 0 : ak.length < bk.length ? -1 : 1;
    }
  }
}

export function valuesEqual(a: WireValue, b: WireValue): boolean {
  return compareValues(a, b) === 0;
}

export function arrayContains(list: WireValue[], v: WireValue): boolean {
  return list.some((e) => valuesEqual(e, v));
}
