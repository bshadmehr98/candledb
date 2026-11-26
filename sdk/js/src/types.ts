/**
 * Shared types: the JSON shapes the server speaks ("wire" types) and the
 * error class every failed operation rejects with.
 */

/** A field value exactly as it travels in JSON. */
export type WireValue =
  | null
  | boolean
  | number
  | string
  | WireValue[]
  | { [field: string]: WireValue };

export type WireMap = { [field: string]: WireValue };

/** A document as the server sends it. */
export interface WireDoc {
  path: string;
  id: string;
  data: WireMap;
  createTime: string;
  updateTime: string;
  version: number;
}

/** A value an application may store. Dates become server timestamps. */
export type FieldValue =
  | null
  | boolean
  | number
  | string
  | Date
  | FieldValue[]
  | { [field: string]: FieldValue };

export type DocumentData = { [field: string]: any };

/**
 * The error every SDK operation rejects with.
 *
 * `code` is the server's error code ("not_found", "permission_denied",
 * "failed_precondition", ...). Failures that never reached the server use
 * "unavailable" with status 0; invalid arguments caught locally use
 * "invalid_argument" with status 0.
 */
export class CandleDBError extends Error {
  readonly code: string;
  readonly status: number;

  constructor(code: string, message: string, status = 0) {
    super(message);
    this.name = "CandleDBError";
    this.code = code;
    this.status = status;
  }
}

export type Unsubscribe = () => void;
