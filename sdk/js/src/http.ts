/**
 * The HTTP transport: one function that sends a JSON request, attaches the
 * caller's credentials and turns every kind of failure into a CandleDBError.
 */
import { CandleDBError } from "./types.js";

export type FetchLike = typeof fetch;

/** What the transport needs to know about credentials. */
export interface Credentials {
  /** The bearer token to send, or null for an anonymous request. */
  bearer(): Promise<string | null>;
  /**
   * Called when the server answers 401. Resolves true if the credentials
   * were renewed and the request is worth sending again.
   */
  renew(): Promise<boolean>;
}

export interface RequestOptions {
  /** Send without credentials (used by the auth endpoints themselves). */
  anonymous?: boolean;
}

export class Http {
  constructor(
    readonly baseUrl: string,
    readonly fetchFn: FetchLike,
    private readonly credentials: Credentials,
  ) {}

  async request<T = unknown>(method: string, path: string, body?: unknown, opts: RequestOptions = {}): Promise<T> {
    let res = await this.send(method, path, body, opts);
    if (res.status === 401 && !opts.anonymous && (await this.credentials.renew())) {
      res = await this.send(method, path, body, opts);
    }
    if (res.status === 204) return undefined as T;

    let parsed: any;
    try {
      parsed = await res.json();
    } catch {
      parsed = undefined;
    }
    if (!res.ok) {
      const detail = parsed?.error;
      throw new CandleDBError(
        typeof detail?.code === "string" ? detail.code : "internal",
        typeof detail?.message === "string" ? detail.message : `request failed with status ${res.status}`,
        res.status,
      );
    }
    return parsed as T;
  }

  private async send(method: string, path: string, body: unknown, opts: RequestOptions): Promise<Response> {
    const headers: Record<string, string> = {};
    if (body !== undefined) headers["Content-Type"] = "application/json";
    if (!opts.anonymous) {
      const bearer = await this.credentials.bearer();
      if (bearer) headers["Authorization"] = `Bearer ${bearer}`;
    }
    try {
      return await this.fetchFn(this.baseUrl + path, {
        method,
        headers,
        body: body === undefined ? undefined : JSON.stringify(body),
      });
    } catch (err) {
      // fetch rejects only when no response arrived at all.
      throw new CandleDBError("unavailable", `cannot reach the server: ${(err as Error)?.message ?? err}`);
    }
  }
}

/** Encodes a document or collection path for use in a URL. */
export function encodePath(path: string): string {
  return path.split("/").map(encodeURIComponent).join("/");
}
