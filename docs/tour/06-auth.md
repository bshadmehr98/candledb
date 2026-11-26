# 6. Accounts

This page covers how a caller proves who they are: the built-in email and
password accounts, the two kinds of token and how they are renewed, what the
JavaScript client automates, the admin key for trusted server-side code, and
how to use tokens issued by your own system instead of the built-in accounts.

## Authentication and authorisation

Two separate questions are asked of every request:

- **Who is calling?** This is authentication, the subject of this page. The
  answer is either a user ID or "nobody" (an anonymous caller).
- **May they do this?** This is authorisation. It is decided entirely by the
  [security rules](07-security-rules.md), which see the answer to the first
  question as `request.auth`.

A request without credentials is not rejected. It proceeds as anonymous, and
the rules decide what anonymous callers may do. A request with *bad*
credentials (a malformed, forged or expired token) is always rejected with
401. It is never quietly downgraded to anonymous, so an expired session is
noticed instead of turning into mysterious permission errors.

Credentials are sent in a header:

```text
Authorization: Bearer <token>
```

## Built-in accounts

### Sign up

```sh
curl -s -X POST http://localhost:8090/v1/auth/signup \
  -d '{"email": "ada@example.com", "password": "correct horse"}'
```

```json
{"user":{"id":"VIOT2ZUZIFQXJYLJSVQ2DMAX26","email":"ada@example.com","createdAt":"2026-11-23T22:07:24.673374Z"},
 "accessToken":"eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJlbWFp...",
 "refreshToken":"LWRFXLGHXAKFDRBS2H5KZYYLRVUZCZJAFW7SFGS5LAIMLVDD74EX",
 "expiresAt":"2026-11-23T23:07:24.673547Z"}
```

The account is created and signed in at once. Rules:

| Field | Rule |
|---|---|
| email | must contain `@` with text on both sides, no whitespace, at most 254 characters; it is trimmed and lower-cased before being stored |
| password | 8 to 72 bytes |

The upper limit on passwords exists because the hashing algorithm (bcrypt)
ignores everything after 72 bytes; a longer password is refused instead of
being silently weakened. Passwords are stored only as bcrypt hashes.

```sh
curl -s -X POST http://localhost:8090/v1/auth/signup \
  -d '{"email": "bob@example.com", "password": "short"}'
```

```json
{"error":{"code":"invalid_argument","message":"invalid argument: password must be 8 to 72 bytes long"}}
```

Signing up with an email that is taken answers `already_exists` (409).

There is no email verification and no password reset. If you need them, issue
your own tokens (see [below](#bringing-your-own-tokens)).

### Sign in

```sh
curl -s -X POST http://localhost:8090/v1/auth/signin \
  -d '{"email": "ada@example.com", "password": "correct horse"}'
```

The response has the same shape as for sign-up. A wrong password and an
unknown email give the identical answer, after the same amount of work, so
the response does not reveal which emails have accounts:

```json
{"error":{"code":"unauthenticated","message":"unauthenticated: invalid email or password"}}
```

### Who am I

```sh
curl -s http://localhost:8090/v1/auth/me -H "Authorization: Bearer $ACCESS_TOKEN"
```

```json
{"id":"VIOT2ZUZIFQXJYLJSVQ2DMAX26","email":"ada@example.com","createdAt":"2026-11-23T22:07:24.673374Z"}
```

## Access tokens and refresh tokens

Signing in yields two tokens with different jobs.

| | Access token | Refresh token |
|---|---|---|
| Used for | every API request, in the `Authorization` header | getting a new access token |
| Lifetime | 1 hour (`-access-ttl`) | 30 days (`-refresh-ttl`) |
| Form | a signed JWT; the server verifies it without a database lookup | 52 random characters; the server stores only a hash of it |
| Can be revoked | no; it simply expires | yes; by using it or by signing out |

The split limits damage. The token that travels with every request is
short-lived, so a leaked one is useful for at most an hour. The long-lived
token is sent only to the refresh endpoint.

An access token is a **JWT** (JSON Web Token): three base64 parts, of which
the middle one is readable JSON. Decoded, the payload of the token above is:

```json
{"email":"ada@example.com","exp":1795475263,"iat":1795471663,"iss":"candledb",
 "jti":"AWYV3KWW7VR25I2UXIXQZLDI4P","sub":"VIOT2ZUZIFQXJYLJSVQ2DMAX26"}
```

`sub` is the user ID, which rules see as `request.auth.uid`. `exp` is the
expiry time in seconds since 1970. The token is signed with HMAC-SHA256
("HS256") using the server's secret; anyone can read it, but only the holder
of the secret can produce one that verifies.

### Refresh

```sh
curl -s -X POST http://localhost:8090/v1/auth/refresh \
  -d '{"refreshToken": "LWRFXLGH..."}'
```

The response is again the sign-in shape, with a new access token *and a new
refresh token*. The refresh token that was sent stops working. This is called
**rotation**: each refresh token can be used exactly once.

```json
{"error":{"code":"unauthenticated","message":"unauthenticated: invalid refresh token"}}
```

That is the answer to using a refresh token a second time, to an expired one
and to an unknown one alike. A client that receives it has to sign in again.

Because of rotation, two programs holding the same refresh token cannot both
use it: the second one is refused and has to sign in again.

Browser tabs of the same app are the common case of this, and the SDK
handles it when the tabs share a storage such as `localStorage`. Before a
tab refreshes, and again if its refresh is refused, it looks in storage for
tokens another tab has already obtained and takes those over instead of
signing out.

### Sign out

```sh
curl -s -X POST http://localhost:8090/v1/auth/signout -d '{"refreshToken": "..."}'
```

This revokes the refresh token and answers `204 No Content`, also for a token
that is unknown or already used. The access token is not revoked. It keeps
working until it expires, at most one access-token lifetime later. If that
window matters to you, shorten `-access-ttl`.

## What the JavaScript client does

```js
const db = createClient({ url: "http://localhost:8090", storage: localStorage });

await db.auth.signUp("ada@example.com", "correct horse");
await db.auth.signIn("ada@example.com", "correct horse");
db.auth.currentUser;        // { id, email, createdAt } or null
await db.auth.signOut();

const stop = db.auth.onAuthStateChanged((user) => {
  // called with the user after sign-in, with null after sign-out
});
```

The client then handles the tokens for you:

- It attaches the access token to every request.
- It refreshes shortly before the access token expires. "Shortly" is one
  minute, or 20% of the token's lifetime if that is less.
- If the server still answers 401, it refreshes once and repeats the request.
- If the refresh itself is refused, the user is signed out locally and
  `onAuthStateChanged` fires with `null`.
- Signing in or out reconnects the realtime stream, so listeners immediately
  reflect what the new identity may read.

Without the `storage` option the session lives in memory and is gone on a
page reload. With it, the session is kept under the key `candledb:session`.
`storage` can be `localStorage`, `sessionStorage`, or any object with
`getItem`, `setItem` and `removeItem`; the methods may be asynchronous, which
suits React Native's storage.

Anything stored in `localStorage` can be read by any script running on your
page. That is the usual trade-off of browser-side tokens: it makes
cross-site scripting bugs in your frontend more costly. The short access
token lifetime and refresh rotation limit the damage; they do not remove it.

## The admin key

Server-side code that you trust (a migration script, a scheduled job, a
webhook handler) often needs to read and write without regard to the rules.
Start the server with an admin key of at least 24 characters:

```sh
./bin/candledb serve -admin-key "$(openssl rand -hex 24)"
# or set CANDLEDB_ADMIN_KEY in the environment
```

A request that sends this key as its bearer token **bypasses the security
rules entirely**:

```sh
curl -s -X PUT http://localhost:8090/v1/documents/elsewhere/x \
  -H "Authorization: Bearer 0123456789abcdef0123456789abcdef" -d '{}'
```

```js
const admin = createClient({ url, adminKey: process.env.CANDLEDB_ADMIN_KEY });
```

The key also unlocks `/metrics`. It must never reach a browser or a mobile
app: anything shipped to users can be extracted by them. With no key
configured, admin access does not exist.

## Bringing your own tokens

The built-in accounts are optional. CandleDB accepts any JWT that:

- is signed with **HS256** using the server's secret,
- has a `sub` claim (it becomes the user ID), and
- has an `exp` claim in the future.

So if you already have a login system, give candledb a secret you control
and have your system sign tokens with it:

```sh
./bin/candledb serve -jwt-secret "$SHARED_SECRET" -disable-signup
```

The secret must be at least 32 bytes. Every claim in the token other than the
standard bookkeeping ones (`sub`, `iss`, `iat`, `exp`, `nbf`, `aud`, `jti`)
is passed to the rules under `request.auth.token`. A token with the payload

```json
{"sub": "external-user-1", "role": "admin", "exp": 1795475245}
```

satisfies this rule:

```text
match /staff/{id} {
  allow read, write: if request.auth.token.role == "admin";
}
```

Custom claims are how roles and tenant IDs reach the rules without a database
lookup. They are trusted exactly as far as the secret is: whoever holds it
can claim anything. Keep it on servers only.

Notes:

- Tokens of the built-in accounts carry one extra claim, `email`, readable
  as `request.auth.token.email`.
- A claim whose name is not a valid field name (it contains a dot or starts
  with `$`) is dropped.
- `GET /v1/auth/me` looks the caller up in the built-in accounts, so for an
  externally issued token it answers `not_found`.
- Only HS256 is accepted. Tokens signed with a public-key algorithm (RS256,
  ES256), as issued by most hosted identity providers, cannot be verified;
  your backend would have to exchange them for an HS256 token.

If you do not set `-jwt-secret`, the server generates one on first start and
keeps it in `<data-dir>/jwt.secret`, so tokens stay valid across restarts.
Changing or deleting the secret invalidates every access token at once.

## Rate limiting

Sign-up, sign-in and refresh are limited per client address, to slow down
password guessing: 10 requests may come in a burst, after which the allowance
refills at 30 per minute. Beyond that the answer is:

```text
HTTP/1.1 429 Too Many Requests
Retry-After: 10

{"error":{"code":"resource_exhausted","message":"resource exhausted: too many attempts, try again shortly"}}
```

The numbers are set with `-auth-rate` (per minute) and `-auth-burst`. No
other endpoint is rate limited.

"Client address" is the address of the TCP connection. Behind a reverse proxy
that is the proxy's address for every user, which would make all users share
one allowance. Start the server with `-trust-proxy` in that case so that it
reads the address from the `X-Forwarded-For` header instead, and only in that
case, since otherwise clients could forge the header.

## Closing registration

`-disable-signup` makes `POST /v1/auth/signup` answer `permission_denied`
(403). Existing users can still sign in and refresh. Use it for an app with a
fixed set of users, or together with your own tokens.

## Summary of endpoints

| Endpoint | Body | Answer |
|---|---|---|
| `POST /v1/auth/signup` | `{"email", "password"}` | session |
| `POST /v1/auth/signin` | `{"email", "password"}` | session |
| `POST /v1/auth/refresh` | `{"refreshToken"}` | session, with new tokens |
| `POST /v1/auth/signout` | `{"refreshToken"}` | 204 |
| `GET /v1/auth/me` | none; needs an access token | the user |

"Session" is `{"user": {"id", "email", "createdAt"}, "accessToken",
"refreshToken", "expiresAt"}`. Session responses carry
`Cache-Control: no-store`.

Next: [7. Security rules](07-security-rules.md)
