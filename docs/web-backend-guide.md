# Browser frontend session

The React frontend uses the existing Go service under one public origin. This
flow is implemented separately from native/mobile bearer-token rotation.
Migration `000005_web_sessions` must be applied before running the new code;
readiness checks require its schema. Apply it first in a test environment.

1. Navigate to `/auth/google/start?client=web&return_to=/transactions`.
2. Complete Google consent for the configured verified owner. Google still
   returns to the backend `/auth/google/callback`.
3. The backend validates state, nonce, PKCE and owner, commits the Gmail
   connection and browser session, then redirects to the allowlisted app path.
4. Bootstrap with `GET /api/v1/auth/session`. Its JSON is
   `{user:{id,email,timezone},csrf_token,expires_at}`. Keep it in memory only.
5. Call the same domain APIs with the cookie. Mutations require both the
   `X-CSRF-Token` from bootstrap and an Origin exactly matching `PUBLIC_URL`.
6. `POST /api/v1/auth/logout` revokes the current browser session and clears
   its cookie. Gmail disconnect is a separate operation and retains sign-in.

The production cookie is `__Host-statement_session`, Secure, HttpOnly,
SameSite=Lax, Path=/, with no Domain. Absolute expiry is 30 days after login;
there is no sliding renewal or browser token refresh endpoint. Reconnect/login
issues a fresh credential and revokes the previous presented browser session
only after a successful transaction. Failed consent retains a valid old session.
Only the credential hash is stored; the CSRF token grants no access on its own.

The manual/Postman and mobile PKCE flows, bearer exchange/refresh/logout and
worker authentication retain their existing contracts. Do not send Authorization
with a browser cookie: mixed credentials return `400 ambiguous_credentials`.
Browser bootstrap does not accept bearer fallback. Missing/expired/revoked
cookies yield 401, CSRF/origin failure 403, database failure 503. Private API/auth
successes and errors are no-store. Cross-site cookie-authenticated reads are
rejected when Fetch Metadata identifies them.

`return_to` permits `/transactions`, `/imports`, `/settings`, and their supported
UUID detail paths. External URLs, queries, fragments, encoded separators and
path traversal fall back to `/transactions`. Web flows never emit an application
login code or bearer tokens into the app URL or page body.

## Local development

Use Vite's existing `/auth` and `/api/v1` proxy to loopback port 18080. Set
`PUBLIC_URL` to the browser origin, such as `http://127.0.0.1:5174`, and register
`http://127.0.0.1:5174/auth/google/callback` as that test OAuth client's exact
`GOOGLE_REDIRECT_URI`. The browser must use this same host and port throughout.
Do not merely change environment values without matching Google registration.

HTTP is allowed only for `localhost` or `127.0.0.1`, using `statement_session`
without Secure. A LAN IP, `0.0.0.0` or private service hostname is not an allowed
public HTTP origin. Other-device or production sign-in requires trusted HTTPS.
No credentials belong in Vite variables, static assets, logs or browser storage.

The implementation tests use an isolated PostgreSQL database and a synthetic
OIDC issuer. They do not prove a real Google registration, live Gmail consent,
production TLS or proxy behavior. Existing running containers and financial
databases are not updated by the frontend build.

For rollback, first stop web traffic and revoke browser sessions. Deploy the
previous backend, then apply the down migration if needed. It removes only web
sessions and web OAuth flows/columns, preserving financial data and bearer rows.
