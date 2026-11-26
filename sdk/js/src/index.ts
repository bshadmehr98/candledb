export {
  createClient,
  CandleDBClient,
  DocumentReference,
  CollectionReference,
  Query,
  DocumentSnapshot,
  QuerySnapshot,
  WriteBatch,
  Transaction,
} from "./client.js";
export type {
  ClientOptions,
  DocumentChange,
  SetOptions,
  SnapshotMetadata,
  TransactionOptions,
} from "./client.js";
export { Auth } from "./auth.js";
export type { AuthStorage, User } from "./auth.js";
export { FieldSentinel, arrayRemove, arrayUnion, deleteField, increment, serverTimestamp } from "./values.js";
export { CandleDBError } from "./types.js";
export type { DocumentData, FieldValue, Unsubscribe } from "./types.js";
export type { OrderDirection, WhereOp } from "./query.js";
export type { ReconnectOptions } from "./listen.js";
