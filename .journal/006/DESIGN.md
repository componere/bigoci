# Phase 5 — Auth and real registries: governing design

Synthesized 2026-08-08 from a three-lens design panel (protocol/interop,
seams, failure-modes; all opus/xhigh) and two comparative adversarial judges
(correctness, provability; workflow `wf_6f7971f2-982`, outputs preserved in
the session scratchpad). Both judges ranked [failure] first and issued
overlapping mandatory corrections. This document is the synthesis: [failure]'s
architecture and 401 state machine, [seams]'s port shape, bearer gateway,
counting-transport test and `scrub`, [protocol]'s off-registry status table,
same-origin rule, and wire-facts ledger — every judge-mandated correction
applied. It supersedes the `Auth` sketch at `design.md:358-361` and the
"Transport sharp edges" bullets at `design.md:406-416` (whose "S3, GCS, and
Azure all reject presigned requests that carry one" is measured false — see
the ledger), and contradicts nothing in `.journal/004/DESIGN.md` or
`.journal/005/DESIGN.md`.

## Stance

Authentication is a **pre-condition of a request, not a recovery from one**.
bigoci resolves what a request must carry before it builds the request, stamps
`Authorization` in `newRequest`, and sends through the caller's client — so
the caller's transport is outermost for every request bigoci makes: the
ordinary ones, the token exchanges, and the redirect re-issues. The `-debug`
tap sees all of them, which is what makes the no-leak gate non-vacuous (C1).

bigoci never retries a request it already sent; the single exception is a
re-issue the registry itself demanded — a challenge or a redirect — and even
that is permitted only when `net/http` says the request is replayable
(`req.GetBody != nil`), which the blob `PUT` is not, by construction. The
orchestrator remains the only thing that repeats an identical request (C2/C3).

bigoci owns the bearer dance and borrows only the credential *store*. oras-go
v2's `auth.Client` refreshes by resending the request that failed, and its
cache has no notion of expiry precisely because the resend is its refresh
strategy — the architecture C3 forbids (evidence in the ledger). What oras-go
is unmatched at — reading `~/.docker/config.json`, running
`docker-credential-*` helpers, normalizing Docker Hub's server address — is
exactly what `internal/auth` wraps, behind a port with one method.

Everything a credential could leak into is enumerated and closed by
construction: the redirect re-issue carries a two-header allow-list, the store
is used through a read-only port, error messages name the registry endpoint
and never a signed URL, and the tap already renders schemes and never bodies.
Every gate has a negative control, because an auth test that would pass
against a registry with auth turned off proves nothing.

## The settled decisions

### D1 — The port: `Credentials` in `internal/oci`, a four-field `Credential`, implemented by `internal/auth`. (C4; [seams] Q1 shape per both judges.)

```go
// Registry names one registry by host, with a port when the reference
// carried one: "ghcr.io", "127.0.0.1:5000".
type Registry string

// Credential is what bigoci presents to one registry. It mirrors the shape a
// Docker configuration file stores. The zero value is the anonymous
// credential: bigoci still performs the token exchange with it, because
// registries that require a bearer token for anonymous reads (GHCR) answer
// an unauthenticated token request with a public-access token.
type Credential struct {
	Username      string // account name presented to the token endpoint
	Password      string // secret (or PAT) that goes with Username
	IdentityToken string // OAuth2 refresh token some logins store; detected to refuse loudly (D8)
	RegistryToken string // bearer to present verbatim, no exchange
}
func (c Credential) Empty() bool

// Credentials resolves the credential bigoci should present to one registry.
// A registry the resolver knows nothing about is the zero Credential and a
// nil error — anonymous is an answer, not a failure. An error means the
// lookup could not be performed and ends the transfer. Implementations must
// be safe for concurrent use and must not retry.
type Credentials interface {
	Credential(ctx context.Context, registry Registry) (Credential, error)
}
```

The two-string `Lookup` [failure] proposed is **rejected** (correctness
judge, mandatory): oras-go's native store returns an identity token as
`{Username:"", RefreshToken:…}` (`credentials/native_store.go:83-104`), which
two strings collapse to `("","")` — indistinguishable from "no credential",
so the loud identity-token refusal D8 requires would be unimplementable and
the credential would silently downgrade to anonymous.

The design-doc sketch `RoundTripper(ctx, registry, scope)` is **rejected and
must not come back**: it blinds the instrument or replaces the caller's
transport (C1/C5); it stamps `Authorization` on every request through it —
including the redirect re-issue, making the presigned leak the shape's
default behavior; and it cannot express "resolve before this spent body goes
out" (C3) or "the credential you sent was refused" (C9). Also rejected: the
port in `internal/transfer` (the core must not learn HTTP exists; `oci` may
not import `transfer`); the dance inside `internal/auth` returning a header
(splits the classification table — a token-endpoint 503 must classify through
the same table as a blob 503; and the dance needs the caller's client, which
would have to be injected twice and agree); exporting the port from the root
package (ports stay internal, A1).

**Home**: `internal/oci` defines the port and types (consumer defines, I2);
`internal/auth` implements and imports `internal/oci` for the two types — the
same adapter-speaks-consumer's-vocabulary edge as `oci → retry`. oras-go
enters the import graph at exactly one edge (`auth`). Compile-time fit in
`internal/auth/port_test.go`; mock in `internal/auth/mocks` (T2/T3). No
`Scope` type is exported anywhere: nothing outside the adapter names a scope.

### D2 — `internal/auth`: the store, and only the store. (C11.)

```
internal/auth
├── doc.go        package godoc, incl. the credential-helper execution warning
├── store.go      Store — Docker config / credential-helper source
└── static.go     Static — one fixed Credential for every registry
```

- `NewStore(configPath string) (*Store, error)` wraps
  `credentials.NewStore(configPath, credentials.StoreOptions{AllowPlaintextPut:
  false, DetectDefaultNativeStore: false})`. `DetectDefaultNativeStore` stays
  false because with it true, `getHelperSuffix` (`store.go:177-189`) falls
  through to the detected platform helper and `Get` execs
  `docker-credential-osxkeychain` against the developer's real keychain even
  from an empty temp config — precisely the C10 hazard. (The
  writes-to-config hazard is real but lives in `Put`, unreachable through
  this port.)
- `DefaultConfigPath()` resolves `$DOCKER_CONFIG/config.json`, else
  `$HOME/.docker/config.json` (Windows `%USERPROFILE%`) — computed by bigoci
  so tests pass a path without touching the environment
  (`NewStoreFromDocker` rejected for exactly that reason).
- `Store.Credential` maps the host through
  **`credentials.ServerAddressFromHostname`** (correctness judge, mandatory:
  it, not `ServerAddressFromRegistry`, maps `registry-1.docker.io` onto the
  `https://index.docker.io/v1/` key `docker login` writes; it is also the
  function oras-go's own `credentials.Credential(store)` uses).
- `Store` bounds the lookup with `credLookupTimeout = 10 * time.Second`
  derived from ctx — a wedged external helper must not hang an unbounded
  transfer. A constant, not a knob.
- The port has one read method, so `Store.Put`/`Delete` are unreachable: a
  credential-writing bug is unrepresentable.

**Dependency footprint (L1), verified**: oras-go v2.6.0 requires exactly
go-digest v1.0.0, image-spec v1.1.1, x/sync v0.14.0 — all three already
direct requires of bigoci. Net new modules: **one**.

Why not oras-go's `auth.Client` as the engine (verified in v2.6.0 source):
no exported fetch-a-token-for-a-scope primitive (`fetchBearerToken` etc.
unexported); `auth.Cache` stores bare strings with no expiry; `Client.Do`
refreshes by resending through `rewindRequestBody`, which fails when
`req.GetBody == nil` — and the blob PUT's body is `io.NopCloser(r)`
(`blobs.go:220-226`), so `GetBody` is nil (`net/http/request.go:928-942`
populates it only for `*bytes.Buffer`, `*bytes.Reader`, `*strings.Reader`).
oras-go's own blob push sidesteps its own auth client by copying
`Authorization` from the POST response onto the PUT
(`registry/remote/repository.go:913-916`). Reuse the store, own the dance.

### D3 — `Authorization` is set in `newRequest`, and nothing else sets it. (C1.)

`newRequest` (`repository.go:197-209`) gains: compute the scope from the
method, ask the repository's auth state for the header, set it when
non-empty. `grep -rn 'Authorization' internal/oci` returning exactly the set
site and the redirect allow-list is a reviewable invariant. `r.client.Do`
remains the single send, so the tap sees everything; the token exchange is a
request built and sent by `internal/oci`, logged as `class=other` — which
`cli/debug.go:47-48` already documents. **No `-debug` grammar change** (C6).

### D4 — Anonymous stays free: no probe, no ping. (C8; both judges.)

The repository's auth state starts unresolved; a bare request goes out; the
401 that comes back carries the challenge. Against a registry that never
challenges — zot, every existing gate — **not one extra request is made and
not one byte of output changes**. The nine committed exact-summary-line
assertions ending `other=0` (`cli/registry_test.go:396-397,407-408,422-423`,
`cli/debug_test.go:304-305`, `cli/README.md:427,512,527,690,722`) stay green
untouched — **that is PR 1's regression gate**, the same inertness proof
phases 3 and 4 bought their credibility with.

Rejected: an unconditional `GET /v2/` preflight (nine assertion rewrites to
buy nothing on the registries the automated gates use; [seams]'s
justifications refuted by the correctness judge). The probe survives only as
D6's provably-unreachable guard.

**Priced instrument cost** (provability judge, mandatory):
`cli/debug.go:226-232` counts any status ≥ 400 as `failed`, so a healthy
challenged transfer reports `failed>=1` — the protocol's own 401. Documented
in `cli/README.md`; **no phase gate asserts `failed=0` against a challenging
registry**. Doc change only.

### D5 — The only re-issue rule.

> The adapter may send a request a second time only when the registry
> demanded a **different** request — a credential it asked for in a
> challenge, or a location it redirected to — and only when
> `req.GetBody != nil` or the request has no body. It never sends an
> identical request twice. The orchestrator remains the only thing that
> repeats an identical request.

Not a retry: no budget consumed, no wait taken, `internal/retry` not
involved. The replayability test is a fact `net/http` computes: the blob PUT
fails it by construction, the manifest PUT (`bytes.NewReader`,
`manifests.go:103`) passes it. **The one request that must never be re-issued
is the one the standard library already refuses to re-issue.** Precedent:
phase 4's 200-for-range rule — a legal protocol answer acted on inside the
same attempt costs no attempt.

### D6 — A body that can be sent once never meets a challenge.

`newRequest`: `if req.Body != nil && req.GetBody == nil` → `auth.resolve(ctx)`
(no-op when the challenge is known; otherwise one bodyless `GET /v2/`).
Unreachable today — `Blobs.Put` opens with a bodyless POST
(`blobs.go:127-130`) — and pinned by two tests: a normal `Put` against a
challenging fixture makes no `/v2/` request; `completeUpload` on a virgin
repository fires the probe. A future refactor fails loudly instead of
hanging on a spent reader.

### D7 — Scope: a pure function of the method.

`pull` for GET/HEAD, `pull,push` for everything else; at most two cache
entries per repository. When a challenge carries its own `scope` parameter,
the exchange requests the union (deduped, sorted, one `scope=` query
parameter per element — GHCR accepts repeats, measured), and the cache key is
the merged string. A registry that issues a narrower token than requested
401s later and D8 handles it. Rejected: binding `pull,push` per repository —
anonymous `pull,push` at GHCR is 403 `DENIED` (measured), so it breaks every
anonymous pull. Scope-from-a-registry-wide `/v2/` challenge is refused:
GHCR's `/v2/` challenge carries the literal placeholder
`scope="repository:user/image:pull"` (measured); only `realm` and `service`
are usable from it.

### D8 — The 401/403 verdict. (C9 — the hardest ruling in the phase.)

**Invariant:**

> A refusal is transient **if and only if** handling it changed what the
> next request will present. Otherwise it is terminal `ErrUnauthorized`. A
> credential that was itself a post-refusal refresh and has never carried a
> successful request may not be refreshed again.

Per-scope cache entry states: `none → acquiring → live/unproven →
live/proven` (any non-401/403 response proves it — a 404 proves a token as
well as a 200); a refusal against an `unproven` entry → `denied`
(absorbing, fails fast for every worker with no request); a refusal against
a `proven` entry retires it and acquires a fresh `unproven` one.

| Situation | Outcome | Budget |
|---|---|---|
| Bad credentials (token endpoint answers 401/403) | `denied`; terminal `ErrUnauthorized`: `run "docker login <registry>"` | **0** |
| Anonymous against a private repo (anonymous token, then refused) | second refusal on unproven → `denied`, terminal | **0** |
| Token expired mid-transfer, request not replayable (blob PUT) | retire, acquire unproven, tag `retry.Transient(…, 0)`; orchestrator re-streams from disk (`push.go:283-305`) | −1 of 4 |
| Token expired, request replayable | retire, acquire, re-issue inside the same call | **0** |
| Insufficient scope (registry downgraded the token) | one identical-scope refresh, then unproven rule → terminal naming the scope | −1 max |
| Clock skew | cannot occur (D9) | — |

Termination: at most two refusal-driven exchanges per scope (`denied`
absorbing); expiry-driven exchanges each become `proven` on first success.
Four racing workers: `refused` first compares the entry's current header
against what the failing request carried — if they differ, another worker
already refreshed; return the current header. Stampede costs one exchange.

**The 403 split** (correctness judge, mandatory — GHCR answers an
unparseable bearer with 403, not 401, and nobody measured a genuinely
expired token):

- **403 carrying a `WWW-Authenticate` challenge** → the same refresh-once
  path a 401 takes, bounded by the same unproven rule.
- **403 without a challenge** → terminal `ErrUnauthorized` (a WAF or
  permission answer; refreshing the same identity cannot change it). The
  non-credential-403 risk is admitted the way `ErrTooLarge` admits a 413 on
  a manifest write.
- Manual gate 5 (a push long enough to outlive GHCR's token lifetime) is
  **blocking** for phase close: the expiry path must be proven against the
  real issuer, not assumed.

**Token-endpoint failures** classify through the existing table: 429/5xx →
`retry.Transient` with the Retry-After floor; 401/403 → terminal
`ErrUnauthorized`; other non-200 → terminal `*StatusError`; a 200 whose body
is unparseable or tokenless → terminal and deliberately **not**
`ErrUnauthorized` (a caller would go fix credentials for a registry bug).

**Challenge parse failure** (absent, malformed, unimplemented scheme) →
terminal `ErrUnauthorized` quoting the challenge truncated to 200 bytes.

**Identity tokens are refused loudly**: a config entry carrying only an
`identitytoken` (ACR) fails with `ErrUnauthorized` saying so — never a
silent downgrade to anonymous. The OAuth2 `refresh_token` grant is a named
follow-up. A `RegistryToken` is presented verbatim as `Bearer <token>` with
no exchange.

### D9 — Expiry on the monotonic clock, proactively.

A token is usable while `now() - acquired < lifetime - margin`;
`lifetime` = `expires_in` seconds (**default 60 when absent/zero/negative** —
the distribution token spec's value; GHCR sends none, measured, so bigoci
re-mints roughly every 30–45s against GHCR and the expiry path is exercised
naturally), `margin = min(30s, lifetime/2)`. `issued_at` is ignored — it is
the registry's wall clock, the only place skew could enter. `time.Since`
reads the monotonic clock, so an NTP step cannot make a live token look
dead; a registry that disagrees anyway produces a 401 and D8 refreshes once.
Skew degrades to one extra exchange, never a failed transfer.

The auth state takes `now func() time.Time` (injected; production
`time.Now`) so T2's expiry rows need no sleeps (provability judge,
mandatory).

**Stated assumption, prominent** (correctness judge, mandatory): the margin
is sufficient only because a registry authorizes a request when it reads the
headers, not after it reads the body. At `DefaultPartSize` (512 MiB) a
single PUT outlives a 60-second token on any real link; the whole margin
argument rests on admission-time authorization. De-risked by the blocking
manual gate 5.

### D10 — Redirects: up to 3 hops, GET/HEAD only, a derived client, a two-header allow-list, same-origin carry. (C7.)

`NewRepository` derives clients from the caller's by struct copy —
`http.Client` is exactly `{Transport, CheckRedirect, Jar, Timeout}`, no
unexported state, so the copy is complete and the caller's client is never
mutated (C5):

- `requestClient`: the copy with `CheckRedirect = refuseRedirect`
  (`http.ErrUseLastResponse`) — auto-follow is off for **every** request,
  closing the hole where Go forwards `Authorization` to any
  domain-or-subdomain target (`net/http/client.go:1005-1028`).
- `redirectClient`: the same, plus `Jar = nil` — no cookie of the registry's
  reaches storage.

Follow rules:

| Condition | Action |
|---|---|
| 301/302/307/308 on GET or HEAD, 303 on GET | re-issue to the resolved Location |
| 303 on HEAD; any 3xx on POST/PUT/PATCH/DELETE | terminal `*StatusError` |
| more than 3 hops | terminal |
| empty/unparseable Location; non-http(s) scheme; `http` target from an `https` repository; target carries userinfo (`net/http/client.go:251-256` turns userinfo into a `Basic` header — a Location with userinfo is the peer choosing bigoci's credential) | terminal |

The re-issued request is built fresh with `http.NewRequestWithContext` (no
inherited headers) and carries exactly the allow-list `{"Range", "Accept"}`
— default-deny, mirroring `cli/redact.go`. Carrying `Range` also suppresses
Go's transparent gzip (`transport.go:2841-2844` skips `Accept-Encoding:
gzip` under `Range`), so a ranged re-issue can never deliver decompressed
bytes under a compressed `Content-Length` ([protocol], grafted).

**Same-origin carry** (correctness judge, mandatory, replacing [failure]'s
clean-always): `Authorization` is attached to a hop iff the target's scheme,
host, **and port** all equal those of the request that received the
redirect; otherwise no `Authorization` at all. Stricter than Go's
domain-or-subdomain rule (a subdomain CDN gets nothing); preserves a
registry that redirects within itself and still authenticates — which works
today and would break under clean-always.

**Never stored**: the Location is a local variable inside one `Blobs.Get`
frame; `ports.go:73-77` already promises a fresh request per call. The
single-use-signature e2e (T7) makes reuse fail rather than merely be absent.

**Off-registry classification** ([protocol] Q11, both judges mandatory):
once a hop has left the registry's origin, responses classify by a separate
table — **401/403/404/410 are transient** (the signature expired or was
revoked; the next attempt re-requests the blob from the registry and
follows a fresh redirect), 429/5xx transient as today, everything else
terminal. **Never `ErrUnauthorized`, never `ErrNotFound`**: GHCR's presigned
`se=` window is ≤10 minutes (measured) and one backoff can be 30s, so an
expired signature is routine, and `ErrUnauthorized` must keep meaning "your
credentials". The 200/206 contract survives end to end: `blobReadStart` runs
on the final response, a 206 still passes `checkRangeStart`, start is `off`
or `0` and nothing else.

### D11 — No error may carry a presigned URL. ([seams] Q8, both judges mandatory.)

Three leak paths, all closed:

1. `statusError` reads `resp.Request.URL.Path` (`repository.go:324-325`) —
   after a re-issue that is the storage request. → `statusErrorAt(method,
   path, resp)` threads the **registry** origin; `statusError(resp)` remains
   for endpoints that never leave origin. `StatusError.Path` gains the godoc
   guarantee: always a path on the registry.
2. `checkRangeStart`/`blobReadStart` same — they take `(method, path)`.
3. Worst: `do` wraps transport failures as `fmt.Errorf("%s %s: %w", …)`
   (`repository.go:225`), and `http.Client.Do` returns a `*url.Error` whose
   `Error()` renders the **full URL including the query** — where `sig=` /
   `X-Amz-Signature` lives — reaching the terminal verbatim via
   `cli/run.go:163`, untouched by any redactor. → `scrub(err)` drops the
   `*url.Error`'s URL (keeping the cause) for off-origin failures.

Where naming the target helps, only its **host** appears. The tap's `err=`
field is safe today and stays safe: a RoundTripper error is not a
`*url.Error` (`http.Client.Do` adds it above the transport) — stated, with
the citation, not assumed. Test: a 403 at a redirect target produces an
error containing the storage host, no `?`, no `sig=`. `StatusError.Detail`
keeps the first 4 KiB of the storage body (request IDs, not signatures) —
accepted, bounded residual.

### D12 — Public API: two options, one sentinel, one exit row. (C8.)

```go
func WithDockerCredentials() Option        // docker login's config + helpers
func WithCredentials(username, secret string) Option  // direct, e.g. CI env
var ErrUnauthorized = errors.New("unauthorized")
```

`New` gains its first reachable error: `WithDockerCredentials` records
intent, `New` builds the store, a malformed `config.json` fails at `New` —
the exact case `client.go:47-50` reserved the error for. A missing config
file is not an error (empty store; anonymous behavior).

**Opt-in in the library**: a library that silently reads
`~/.docker/config.json` and execs `docker-credential-*` binaries is a
surprise with a security dimension. **Always-on in the CLI, no flag**: the
manual gate is `docker login` + zero bigoci-side config, which is what every
ecosystem tool does; a flag would exist only to switch off the behavior
under test, and the no-credentials gate is proved from the environment
(`DOCKER_CONFIG=$(mktemp -d)` — [protocol]'s gate, grafted). `newClient`
gains one unconditional `bigoci.WithDockerCredentials()`.

Consequence handled in the same PR: `cli` gains a `TestMain`
(`cli/main_test.go` — it has none today; the repo's only `TestMain` is
`e2e_kill_test.go:80`) isolating `DOCKER_CONFIG`, `HOME`, `USERPROFILE`,
`PATH` (empty temp dirs; `cli` tests are in-process so an empty `PATH` is
safe there). The **root package's** existing `TestMain` (`e2e_kill_test.go`)
additionally sets `DOCKER_CONFIG`/`HOME` to temp dirs **and carries them in
the re-exec child's `cmd.Env`**, keeping `PATH` intact — testcontainers
needs docker on it (provability judge, mandatory).

Anonymous remains the zero-config default and works against registries that
demand a bearer token for anonymous pulls: with no credential option,
`Credential` returns the zero value, the token request goes out without
Basic, GHCR issues an anonymous token. **The bearer dance is not a
credentialed path; it is the path.**

Rejected: exporting a credential-source interface (ports stay internal;
exotic callers use `WithHTTPClient` — with D13's warning); a CLI flag.

### D13 — The documented alternative is documented as a hazard.

go-containerregistry's `authn` keychain stays the documented alternative via
`WithHTTPClient`. The how-to must say in bold: **a RoundTripper that adds
`Authorization` unconditionally will add it to bigoci's presigned-redirect
re-issue** — that transport sits below the redirect decision by construction
— and show the three-line host check that fixes it. `options.go`'s
`WithHTTPClient` godoc gains the same warning plus the Timeout correction: a
client `Timeout` bounds each **request**, and an attempt that authenticates
or follows a redirect makes up to four; the deadline for a transfer stays on
the context. Godoc correction, not a behavior change; lands with PR 2.

## Algorithms

A1 request lifecycle, A2 `authorize` (single-flight: waiters `select` on
`e.ready`/`ctx.Done()`, never a mutex held across a network call), A3
`handleRefused` (401, and 403-with-challenge, straight-line, at most two
sends, no loop), A4 `follow` (up to 3 hops, per-hop validation and
same-origin carry), A5 `acquire` (Basic → header; Bearer → `GET
realm?service&scope…` with `req.SetBasicAuth` when credentialed — **the
OAuth2 POST grant is never used**: it puts the secret in a body, and GET
plus Basic is what the token spec defines; 64 KiB body cap), A6 `refused`
(header-comparison stampede guard, unproven→denied), A7 `validateRealm`
(absolute https URL — `http` only under `WithPlainHTTP`; no userinfo; no
fragment; realm query preserved and merged; cross-host realm allowed — the
protocol requires it, and the compensating control is that the credential
looked up is always for the host bigoci dialed, never for `service=`), A8
`parseChallenge` (RFC 9110 §11.6.1 scanner — `strings.Split` on commas
breaks on `scope="…:pull,push"`, the first real challenge bigoci will see;
8 KiB cap; prefer Bearer, then Basic, else terminal) — all as [failure]
specified, with the D8/D9/D10 corrections above folded in.

## Port and contract changes

- New in `internal/oci`: `Registry`, `Credential`, `Credentials`,
  `WithCredentials(c Credentials)` (nil ignored), `ErrUnauthorized`.
- `StatusError.Is` gains: 401 → `ErrUnauthorized`, 403 → `ErrUnauthorized`
  (on-registry responses only; off-registry classification never constructs
  a sentinel-matching error, per D10).
- `internal/transfer/ports.go`: **not one line.** `internal/retry`,
  `internal/plan`, `internal/manifest`, `internal/file`,
  `docs/docs/reference/format.md`: untouched.
- `.mockery.yml`: one block — `internal/oci: Credentials → dir:
  internal/auth/mocks`.
- Root: `ErrUnauthorized` sentinel; `classify` gains the row (after
  `ErrNotFound`, before `ErrTooLarge`); the "cannot happen yet" paragraph at
  `errors.go:60-63` deleted; `Push`/`Pull` godoc list the new sentinel.
- CLI: `exitUnauthorized = 6`; one `sentinelExits` row between
  `ErrDigestMismatch` and `ErrPartTooLarge`; `cli/doc.go` table updated;
  `debug.go`/`redact.go` **not one line** (C6 satisfied by not editing).

New `internal/oci` files (R2): `credentials.go` (port, `WithCredentials`,
`scopeFor`), `challenge.go`, `token.go`, `authstate.go`, `redirect.go`.
D1 godoc everywhere incl. unexported; D4 `doc.go` for `internal/auth`. No
digest computation in `internal/auth` → no sha256 blank import needed
(stated, per the recurring trap).

## Testing plan

Every gate has a stated negative control or a stated vacuity mode and what
closes it.

- **T1 unit (oci)**: `parseChallenge` table (comma-in-quoted-scope first;
  two challenges; escapes; 9 KiB garbage; case/order) and `validateRealm`
  table (https ok; http refused / accepted under plain-HTTP; userinfo,
  relative, fragment, empty host refused; query merged).
- **T2 unit (oci), the auth state machine**: httptest fixture +
  `mocks.MockCredentials`, injected `now`. Rows per D8's table plus:
  anonymous-costs-nothing baseline (request count equals pre-auth baseline);
  bad creds → **exactly one token request across four concurrent workers,
  zero recorded backoff sleeps** (numeric no-burn, provability judge);
  proven-token blob PUT 401 → `retry.Transient`, fixture counts bodies
  (**`Put` never sent a second body**), next call carries a different
  header; 403-with-challenge → refresh path; 403-bare → terminal; expiry via
  injected clock (cached inside the margin, re-minted past it, `expires_in`
  absent → 60; **exactly one mint under four concurrent authorizes**);
  token-endpoint 503 with `Retry-After: 2` → transient, 2s floor; token 200
  with `{}` → terminal, not `ErrUnauthorized`; ctx cancelled while three
  workers wait → all return promptly, no goroutine leak (`-race`).
- **T2b unit (oci), the counting transport** ([seams], both judges
  mandatory): a fixture client whose transport counts everything; assert the
  challenged request, the token exchange, the endpoint request, and **every
  redirect hop** crossed it. Reviewable invariant: `internal/oci` constructs
  no `http.Client` outside the two documented derivations (grep).
- **T3 unit (oci), redirects**: httptest registry + storage; storage asserts
  no `Authorization`/`Cookie`; **positive control: storage flipped to
  require `Authorization` must fail the read** (else the no-leak row can
  pass because the handler never ran); `Range` survives (206 at N →
  `blobReadStart` reports N); 200-for-range → 0; storage 403 → transient
  under the off-registry table and the error names host only, no `?`, no
  `sig=`; >3 hops terminal; userinfo/http-downgrade/empty Location
  terminal; 307 on PUT terminal, no body re-read; same-origin hop keeps
  `Authorization`, subdomain hop gets none; a caller Jar's cookie never
  reaches storage.
- **T4 unit (auth), isolation is the test** (C10): `TestMain` isolates
  `DOCKER_CONFIG`, `HOME`, `USERPROFILE`, `XDG_RUNTIME_DIR`, `PATH`.
  Planted config returned; `store.ConfigPath()` asserted under `t.TempDir()`
  (**the proof no real config was read**). Helper positive control: an
  executable `docker-credential-fake` on the temp `PATH`, config naming
  `credsStore: "fake"` — lookup returns what the fake printed **and the
  fake recorded it ran**; a row without `credsStore` asserts it did **not**
  run; `credentials/trace.WithExecutableTrace` with `ExecuteStart` →
  `t.Fatalf` installed on every config-only row (**structural proof the
  suite never shells out**, [seams], both judges). Identity-token config →
  the D8 refusal. Config bytes unchanged after every row. Wedged helper
  (60s sleep) → returns within `credLookupTimeout`.
- **T5 integration (transfer)**: no new tests; the suites staying green is
  the assertion that auth landed outside the core.
- **T6 e2e (`e2e_auth_test.go`), htpasswd zot**: htpasswd is one config
  field via the existing `newZot` `WithFiles` seam; bcrypt hash committed as
  a constant (no x/crypto dep). Rows: **negative control first** (no creds →
  `ErrorIs ErrUnauthorized`; if it passes trivially the registry is not
  enforcing auth and everything below proves nothing); wrong password →
  sentinel, exactly one request per operation, zero sleeps, no blob writes;
  right creds → multi-part push+pull byte-identical; tagless round trip;
  creds for a different host in the config → anonymous → refused (lookup
  key is the dialed host); CLI in-process wrong creds → **exit 6**, exact
  `matched sentinel bigoci.ErrUnauthorized (exit 6)` line.
- **T6b e2e, the bearer gateway** ([seams], both judges mandatory — this
  closes the "bearer dance untested per-commit" hole): an in-process
  `httptest` gateway fronting the same zot container — issues `Bearer
  realm=<gateway>/token`, serves `/token`, proxies authenticated requests to
  zot, expires tokens after K requests, 302s blob GETs to a storage stub
  that fails on `Authorization`. Rows: correct creds → push+pull
  byte-identical, exactly one `/token` request (shared slot); **token
  expiry with K < parts → push and pull still complete byte-identical, ≥2
  `/token` requests, no part exceeds 4 attempts** — the C9 ruling
  end-to-end through the orchestrator's fresh `SectionReader`, per commit;
  302 row → storage saw ≥1 request and never an `Authorization` header,
  plus one 206 to a `Range` request.
- **T7 e2e, redirect (`e2e_redirect_test.go`)**: proxy in front of zot
  307ing blob GETs to a storage stub that rejects `Authorization` (400),
  requires `sig`, accepts each signature **once**. Multi-part pull succeeds;
  zero `Authorization` at storage; a mid-part break's continuation gets a
  **fresh** 307 and fresh signature (no stored URL, C7); positive control:
  storage requiring `Authorization` fails the pull.
- **T8 manual gates (frozen; evidence to the journal)**:
  1. `docker login ghcr.io`; push 200 MB at 16 MiB parts to a **private**
     GHCR repo, zero bigoci config; pull; `shasum` matches. Evidence: both
     `-debug` logs + summary lines.
  2. No-leak, presence AND universality (provability judge): from gate 1's
     pull log — the count of off-`ghcr.io` `http>` lines is **≥ the number
     of parts read**, **every** one reads `auth=none` (keyed on host+auth;
     `class=blob-read` as corroboration only), and ≥1 token-exchange line
     exists (`class=other` `http>` preceded by an `http<` carrying
     `challenge=`). A working pull is NOT evidence — GHCR's storage answers
     200 to a leaked bearer (measured).
  3. `DOCKER_CONFIG=$(mktemp -d) bigoci pull <private>` → exit 6 + matched
     sentinel line; then with a wrong PAT → exit 6, log shows exactly one
     token request (no burn).
  4. Tagless: push/pull `ghcr.io/<owner>/<repo>@sha256:…`.
  5. **Blocking**: a push long enough to outlive GHCR's token lifetime
     completes; >1 `class=other` token exchange in the log, no failed part.
     The only gate exercising D8/D9 against a real issuer; the phase does
     not close without it.
- **T9 CI**: `moon ci` unchanged; T6/T6b/T7 run per commit.

## PR breakdown

Four PRs — the plan's three plus the inert classification PR the phase-3/4
precedent requires (PR #21, PR #25):

- **PR 0 `feat(oci): classify a refused request as unauthorized`** *(inert)*:
  `oci.ErrUnauthorized`, the two `StatusError.Is` rows,
  `bigoci.ErrUnauthorized`, the `classify` row, CLI exit-6 row + `doc.go`,
  `errors.go:60-63` deleted. Unit + CLI fake-registry 401→exit-6 tests.
  Inert because nothing bigoci talks to today returns 401/403; existing
  suites green, no assertion edited.
- **PR 1 `feat(auth): oras-go credentials adapter behind the Auth port`**:
  `internal/auth` + mocks + `.mockery.yml`; `internal/oci` credentials/
  challenge/token/authstate + `newRequest`/`send` wiring; root options +
  `New` error path; CLI always-on + `cli/main_test.go`; root `TestMain`
  env isolation; T1/T2/T2b/T4/T6/T6b; auth how-to + docs. Regression gate:
  the nine golden lines, untouched.
- **PR 2 `feat(oci): presigned redirect handling`**: `redirect.go`, derived
  clients, off-registry table, origin threading + `scrub` (D11),
  `WithHTTPClient` godoc, T3 (+redirect rows of T2b)/T7, design.md
  sharp-edges rewrite.
- **PR 3 `ci: manual cloud-registry conformance job`**:
  `.github/workflows/conformance.yml` — `workflow_dispatch` only, `if:
  github.repository == 'componere/bigoci'`, `permissions: {contents: read,
  packages: write}`, `persist-credentials: false`, mise/cache steps copied
  from `ci.yml`; writes `GITHUB_TOKEN` into a temp `DOCKER_CONFIG` (never
  echoed); runs a conformance-tagged test against
  `ghcr.io/componere/bigoci/bigoci-conformance`: multi-part push+pull,
  tagless, wrong-token exit 6, and gate 2's counted no-leak assertions over
  the `-debug` log (uploadable as artifact — redaction is by construction).
  Package cleanup best-effort. No production code.

## Docs impact (D6; PR 1 unless noted)

New `docs/docs/how-to/authenticate.md` (docker login; helpers — bigoci will
execute them; both options; exit 6 / `ErrUnauthorized` and its three causes;
the ggcr alternative **with the D13 leak warning**; the identity-token
limitation). design.md: rewrite the auth decision (`:155-170`), replace the
Ports sketch (`:358-361`), rewrite Transport sharp edges (`:406-416` —
including correcting the false "S3/GCS/Azure reject" claim to a
confidentiality requirement and adding "a working pull is not evidence"),
add an Authentication section (D8 table, D9 clock), add `internal/auth` to
the layout (`:390-401`). `docs/docs/index.md`, `how-to/push-and-pull.md`
pointers. `internal/oci/doc.go:36-38` replaced (PR 1 auth, PR 2 redirects).
`cli/README.md`: exit-code row 6; three recipes (authenticated GHCR push;
exit-6; counted no-leak redirect grep); the `failed>=1`-under-challenge
note; the "blob-read counts two per part against a redirecting registry, so
the resume recipe's counts are local-zot-only" note. **No grammar change.**
`options.go` (PR 2), `client.go`, `errors.go` godoc. `format.md` untouched,
deliberately.

## Accepted costs

- Two extra requests per authenticated scope (challenge + exchange), ~one
  exchange per `expires_in − 30s` of transfer; zero extra against anonymous
  registries.
- Token cache dies with each `*Repository` (per-transfer): a token never
  outlives the credential that made it; every e2e runs the full dance.
- A 401 on a blob PUT costs one of the part's four attempts even when the
  refresh succeeds (the alternative is resending a spent body).
- A challenged transfer's summary reads `failed>=1` (the protocol's own
  401); documented, no gate asserts `failed=0` there.
- 403-without-challenge moves from exit 1 to exit 6; a WAF 403 misreports
  as unauthorized (admitted, like `ErrTooLarge`'s 413).
- Identity-token credentials refused, not supported (named follow-up:
  OAuth2 `refresh_token` grant).
- A caller-supplied authenticating RoundTripper can still leak to presigned
  storage — below the redirect decision by construction; documented twice
  with the fix.
- An attempt can take longer than one client `Timeout` (up to four
  requests). Godoc correction.
- `StatusError.Detail` may carry the first 4 KiB of a storage error body
  (request IDs, not signatures) — bounded residual.
- **Rejected forever**: a RoundTripper-shaped auth port; oras-go's
  `auth.Client` as the sender; an in-band resend of a body-bearing request;
  an unconditional preflight; forwarding `Authorization` cross-origin; the
  OAuth2 POST grant; storing a redirect URL; a `-debug` grammar change;
  writing the user's Docker config; a CLI credentials flag.

## Verified versus assumed

**Measured on the wire (panel, 2026-08-09, ghcr.io + registry-1.docker.io;
re-measured by the correctness judge):** GHCR's 401 challenge shape; `/v2/`
challenge carries a literal placeholder scope (unusable); anonymous token
response is `{"token":"…"}` — **no `expires_in`** (spec default 60s
governs); anonymous `pull,push` scope → 403 `DENIED`; bad Basic at the
token endpoint → 403 `DENIED` (bad creds never reach a body-bearing
request); private repo → 401+challenge, not 404; blob GET → 307 to
`pkg-containers.githubusercontent.com/...?se=…&sig=…` with a ≤10-minute
window; that target honors `Range` (206) **and returns 200 to a request
also carrying the registry bearer** — so does Docker Hub's CloudFront (a
working pull proves nothing about leaks); an unparseable bearer at GHCR →
**403**, well-formed-unsigned → 404, none → 401 (nobody measured a
genuinely expired token — hence the blocking gate 5); repeated `scope=`
params accepted; Docker Hub sends `expires_in: 300`, `token` +
`access_token`.

**Verified in go1.26.4 source:** `http.Client`'s four exported fields;
`shouldCopyHeaderOnRedirect`/`isDomainOrSubdomain`; 307/308 with nil
`GetBody` not followed; `ErrUseLastResponse` returns the 3xx with an open
body (each hop must be drained); `GetBody` populated only for
bytes/strings readers; URL userinfo → Basic header; gzip suppressed under
`Range`; `url.Error.Error()` renders the full URL including query;
`stripPassword` redacts only userinfo.

**Verified in oras-go v2.6.0 source:** three requires, all already bigoci
directs; no exported token-fetch primitive; `auth.Cache` expiry-less;
`rewindRequestBody` requires `GetBody`; oras-go's own PUT sidesteps its
auth client; `credentials.Credential` normalizes via
`ServerAddressFromHostname`; `DynamicStore.ConfigPath()`;
`StoreOptions{AllowPlaintextPut, DetectDefaultNativeStore}` and the
helper-fallthrough hazard; `credentials/trace.ExecutableTrace`;
`config.Load` tolerates a missing file.

**Assumed, de-risked:** `GITHUB_TOKEN` + `packages: write` can push under
the owning repo (PR 3 is hand-triggered; first run reports exactly what is
missing); zot htpasswd denies anonymous as configured (T6's negative
control runs first); oras-go's helper executer honors ctx
(`credLookupTimeout` + wedged-helper row bound it regardless); admission-
time authorization (blocking gate 5); ECR/ACR specifics (documented, out
of gate scope).
