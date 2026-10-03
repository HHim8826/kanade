# Authentication

The web client offers passwords and passkeys as alternative login methods. UI availability checks decide which controls to show; they never authorize a session. The server verifies every login.

Passwords use salted bcrypt hashes. Password login, passkey assertions and password confirmation for enrollment share the same per-client limit: five failed or currently running checks in a rolling fifteen-minute window. Checks reserve their slot before verification, so a parallel burst cannot bypass the limit. Successful checks release their slot.

Both methods issue a new random 256-bit session token. Only its SHA-256 hash is stored. Sessions expire on the server ninety days after creation, regardless of recent activity; the browser cookie has the same lifetime. Cookie responses do not expose the token in JSON, and use HttpOnly and SameSite=Strict. With an HTTPS public URL they also use Secure. Logout revokes the current session. Password changes revoke all existing sessions in the same transaction as the password update.

Login endpoints reject a foreign Origin or cross-origin Fetch Metadata. Cookie login additionally requires `X-Requested-With: kanade`, including before any session exists; the server does not grant CORS preflights. Native clients can still request a bearer token without browser headers. Cookies on authenticated writes require the same CSRF header.

Passkey registration requires the current password. Its authorization is a random, one-time challenge with a five-minute lifetime, bound to the account, purpose and verified password version. Enrollment checks that version again under the write lock, so an in-flight confirmation cannot survive a password reset. The account's twenty-key limit is also enforced under that lock.

Passkey assertions must match the exact configured website origin and RP ID hash, the challenge, a stored credential, user presence and user verification. The signature is checked with the stored public key; ES256, EdDSA and RS256 are accepted. Attestation is requested as `none`, allowing consumer and synced authenticators. A zero counter is accepted for authenticators that do not maintain one; a nonzero counter must advance. The credential and counter are checked again in the transaction that creates the session, preventing concurrent assertions or a completed key deletion from bypassing those checks.

Deleting a passkey prevents new logins with it. It does not revoke sessions already issued with it; those sessions end through logout, password reset or expiry. A password reset retains existing registered passkeys as independent login credentials, while invalidating pending registrations authorized by the old password.

Deployment must expose the app over HTTPS and set `-public-url` to its actual origin. The current Go listener serves HTTP: TLS must be provided by the tunnel or front end. Binding that listener to loopback keeps it behind the front end. Forwarded client IP headers are trusted only when the immediate peer is loopback, so the front end must supply authoritative client addresses. HTTP `localhost` is permitted for development and browser tests; this exception does not apply to an external HTTP hostname.

Validation includes deterministic SQLite concurrency regressions, wrong-origin and signature tests, challenge replay/expiry/ownership checks, cookie protection tests, and real Chromium password and WebAuthn flows with a virtual authenticator. Run from `server/`:

```bash
go test ./...
go test -race ./internal/auth ./internal/api ./internal/webauthn
go test ./internal/webauthn -run '^$' -fuzz FuzzParse -fuzztime=15s
```
