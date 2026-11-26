/**
 * Accounts: sign-up, sign-in, sign-out, and keeping the access token fresh.
 */
import { Http, type Credentials } from "./http.js";
import { CandleDBError, type Unsubscribe } from "./types.js";

export interface User {
  id: string;
  email: string;
  createdAt: Date;
}

/**
 * Where the session is kept between page loads. `localStorage` fits this
 * interface as it is; methods may also return promises (React Native's
 * AsyncStorage, for instance). The default keeps the session in memory.
 */
export interface AuthStorage {
  getItem(key: string): string | null | Promise<string | null>;
  setItem(key: string, value: string): void | Promise<void>;
  removeItem(key: string): void | Promise<void>;
}

interface Session {
  user: { id: string; email: string; createdAt: string };
  accessToken: string;
  refreshToken: string;
  expiresAt: number; // ms since epoch
  lifetime: number; // ms the access token was valid for when issued
}

interface SessionResponse {
  user: { id: string; email: string; createdAt: string };
  accessToken: string;
  refreshToken: string;
  expiresAt: string;
}

const STORAGE_KEY = "candledb:session";

export class Auth implements Credentials {
  private session: Session | null = null;
  private adminKey: string | null;
  private refreshing: Promise<void> | null = null;
  private readonly listeners = new Set<(user: User | null) => void>();
  /** @internal Called when the identity requests are made with changes. */
  _onIdentityChange: () => void = () => {};
  /** Resolves once a stored session, if any, has been loaded. */
  readonly ready: Promise<void>;
  private readonly http: Http;

  /** @internal */
  constructor(baseUrl: string, fetchFn: typeof fetch, private readonly storage: AuthStorage | null, adminKey?: string) {
    this.adminKey = adminKey ?? null;
    // The auth endpoints are called without credentials, so this transport
    // never calls back into bearer() or renew().
    this.http = new Http(baseUrl, fetchFn, { bearer: async () => null, renew: async () => false });
    this.ready = this.load();
  }

  /** The signed-in user, or null. */
  get currentUser(): User | null {
    return this.session ? toUser(this.session) : null;
  }

  async signUp(email: string, password: string): Promise<User> {
    return this.begin("/v1/auth/signup", email, password);
  }

  async signIn(email: string, password: string): Promise<User> {
    return this.begin("/v1/auth/signin", email, password);
  }

  /** Signs out locally and revokes the refresh token on the server. */
  async signOut(): Promise<void> {
    await this.ready;
    const session = this.session;
    if (!session) return;
    await this.setSession(null);
    try {
      await this.http.request("POST", "/v1/auth/signout", { refreshToken: session.refreshToken }, { anonymous: true });
    } catch {
      // Already signed out locally; a token that could not be revoked
      // simply expires on its own.
    }
  }

  /**
   * Calls `cb` with the current user once the stored session has loaded,
   * and again whenever a user signs in or out.
   */
  onAuthStateChanged(cb: (user: User | null) => void): Unsubscribe {
    this.listeners.add(cb);
    void this.ready.then(() => {
      if (this.listeners.has(cb)) cb(this.currentUser);
    });
    return () => this.listeners.delete(cb);
  }

  /** A valid access token for the signed-in user, or null. */
  async getAccessToken(): Promise<string | null> {
    await this.ready;
    if (!this.session) return null;
    // Refresh a little before the token runs out, so a request never
    // leaves with a token that expires on the way.
    const margin = Math.min(60_000, this.session.lifetime * 0.2);
    if (this.session.expiresAt - Date.now() < margin) {
      try {
        await this.refresh();
      } catch (err) {
        if (!this.session) return null; // the refresh token was rejected
        if (this.session.expiresAt <= Date.now()) throw err; // offline and expired
      }
    }
    return this.session?.accessToken ?? null;
  }

  /** Uses the server's admin key for every request (server-side code only). */
  setAdminKey(key: string | null): void {
    this.adminKey = key;
    this._onIdentityChange();
  }

  /** @internal */
  async bearer(): Promise<string | null> {
    return this.adminKey ?? (await this.getAccessToken());
  }

  /** @internal */
  async renew(): Promise<boolean> {
    if (this.adminKey || !this.session) return false;
    try {
      await this.refresh();
      return this.session !== null;
    } catch {
      return false;
    }
  }

  /**
   * Exchanges the refresh token for a new session. Concurrent callers share
   * one request: refresh tokens are single-use, so two parallel refreshes
   * would invalidate each other.
   */
  private refresh(): Promise<void> {
    this.refreshing ??= (async () => {
      const session = this.session;
      if (!session) return;
      // Another tab sharing the same storage may have refreshed already.
      // Its new tokens are in storage, and ours no longer work, so take
      // those over instead of spending a refresh token that is used up.
      if (await this.adoptStored(session)) return;
      try {
        const res = await this.http.request<SessionResponse>(
          "POST",
          "/v1/auth/refresh",
          { refreshToken: session.refreshToken },
          { anonymous: true },
        );
        if (this.session === session) await this.setSession(toSession(res));
      } catch (err) {
        if (err instanceof CandleDBError && err.status === 401 && this.session === session) {
          // The other tab may have won the race between our check and our
          // request; only sign out if storage has nothing newer either.
          if (await this.adoptStored(session)) return;
          await this.setSession(null);
        }
        throw err;
      }
    })().finally(() => {
      this.refreshing = null;
    });
    return this.refreshing;
  }

  private async begin(path: string, email: string, password: string): Promise<User> {
    await this.ready;
    const res = await this.http.request<SessionResponse>("POST", path, { email, password }, { anonymous: true });
    const session = toSession(res);
    await this.setSession(session);
    return toUser(session);
  }

  private async load(): Promise<void> {
    this.session = await this.readStored();
  }

  private async readStored(): Promise<Session | null> {
    if (!this.storage) return null;
    try {
      const raw = await this.storage.getItem(STORAGE_KEY);
      return raw ? (JSON.parse(raw) as Session) : null;
    } catch {
      return null; // unreadable storage means "not signed in"
    }
  }

  /**
   * Switches to the session in storage if it belongs to the same user and
   * carries different tokens than `current`. Returns whether it did.
   */
  private async adoptStored(current: Session): Promise<boolean> {
    const stored = await this.readStored();
    if (!stored || stored.user.id !== current.user.id || stored.refreshToken === current.refreshToken) return false;
    if (this.session === current) this.session = stored;
    return true;
  }

  private async setSession(next: Session | null): Promise<void> {
    const before = this.session?.user.id ?? null;
    this.session = next;
    try {
      if (next) await this.storage?.setItem(STORAGE_KEY, JSON.stringify(next));
      else await this.storage?.removeItem(STORAGE_KEY);
    } catch {
      // The session still works for this page load.
    }
    if ((next?.user.id ?? null) !== before) {
      this._onIdentityChange();
      const user = this.currentUser;
      for (const cb of [...this.listeners]) cb(user);
    }
  }
}

function toSession(res: SessionResponse): Session {
  const expiresAt = new Date(res.expiresAt).getTime();
  return {
    user: res.user,
    accessToken: res.accessToken,
    refreshToken: res.refreshToken,
    expiresAt,
    lifetime: Math.max(0, expiresAt - Date.now()),
  };
}

function toUser(s: Session): User {
  return { id: s.user.id, email: s.user.email, createdAt: new Date(s.user.createdAt) };
}
