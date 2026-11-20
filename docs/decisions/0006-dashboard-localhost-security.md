# 0006: The dashboard is unauthenticated and reachable only from this machine, so the browser is the threat

Status: Accepted (Phase 7, steps 7b and 7c, commits 9cb1405 and 0b42f07)
Date: 2026-11-19 (plan approved by the user 2026-11-18, all defaults Q1-Q7, docs/phase7-plan.md:3, :141-149)

## Context
The dashboard exposes replay and undo, which change production data, over HTTP. Phase 7 puts that behind
`admin serve` with no login (out of scope in the plan: authentication, multi-user, TLS, remote access;
docs/phase7-plan.md:63-64). That moves the question from "who may call this" to "what else on this machine can
reach it". The realistic answer is a web page in the operator's own browser: any site can make the browser send
a request to localhost (CSRF), and a hostile name that resolves to 127.0.0.1 makes the attacker's page same-origin
(DNS rebinding) (internal/adminhttp/server.go:6-12; plan :51-56).

## Options considered
1. **An auth token or login.** The plan lists authentication as out of scope and does not weigh a token design
   (plan :63). Author's reasoning, not in the plan: for one operator on one machine a token stored next to the
   binary adds secret handling without stopping a local process that can already read it.
2. **Bind `localhost` or `::1` as well.** The plan's Safety section says `serve` refuses any non-loopback address,
   including hostnames (plan :51). The implementation went further: it accepts only the literal `127.0.0.1:<port>`,
   so `::1` and other 127.x addresses are refused as well, because the Host allow-list wants one known literal and
   half-supporting IPv6 is harder to defend (ParseAddr comment, server.go:47-52; plan implementation note :107-109).
   The Host allow-list still admits the name `localhost:<port>` (server.go:120).
3. **A SPA framework.** The plan chose plain HTML, CSS and vanilla JS embedded with embed.FS and no JS build
   toolchain (plan :25). It weighed only server-rendered html/template, rejected because the roadmap asks for a
   JSON admin API tested on its own and vanilla JS keeps the UI a thin client (plan :25-27). Author's reasoning,
   not in the plan: a framework would add a build step and a dependency tree for six views.
4. **Preview as GET.** The plan makes preview a POST (Q6): it is not audited but takes row locks, so a browser
   prefetch must not trigger it (plan :57, :148).
5. **Check Origin only.** The plan rejects relying on Origin alone, since after a rebind the attacker page is
   same-origin (plan :52-53). Hence a Host allow-list as well (chosen).

## Decision
Layers, outermost first (Handler, server.go:93-101):
- **Bind.** `serve` accepts only `127.0.0.1:<port>`, port 1-65535, default 8090 (ParseAddr, server.go:53-68;
  cmd/admin/serve.go:20). Refused: 0.0.0.0, `:8090`, `[::]`, `[::1]`, hostnames, other 127.x, no or zero port.
  Test: TestParseAddr (addr_test.go:10-40).
- **Security headers on every response**, including rejections and 404s: `Content-Security-Policy: default-src
  'self'` and `X-Content-Type-Options: nosniff` (server.go:105-111). Test: TestSecurityHeadersOnEveryResponse
  (addr_test.go:44), and TestStaticFilesServed for the embedded files (static_test.go:23).
- **Host allow-list (DNS rebinding).** 403 `bad_host` unless Host equals `127.0.0.1:<port>` or `localhost:<port>`,
  compared case-insensitively (server.go:117-126). Applies to GETs too. Test: TestDBWrongHost403 (wrong name,
  wrong port, no port, empty, a suffix trick), with controls for both allowed names (handlers_db_test.go:260).
- **CSRF.** net/http `CrossOriginProtection` (Sec-Fetch-Site and Origin) on POST with a JSON deny handler: 403
  `cross_origin` (server.go:94-97). Test: TestDBCrossSitePost403 refuses cross-site, same-site and foreign
  or look-alike Origins with zero audit rows and the event unchanged, and a same-origin control succeeds
  (handlers_db_test.go:232).
- **JSON-only POST bodies.** A POST without `Content-Type: application/json` is 415. The plan's reasoning is that a
  cross-site form or simple fetch cannot send that without a CORS preflight, which the server never approves
  (server.go:128-139; plan :55-56). Test: TestDBWrongContentType415, including a missing header and a charset
  control (handlers_db_test.go:289). Preview requires it too although it reads no body (plan :109-110).
- **Body limit and strict decoding.** Bodies are capped at 16 KiB (`maxBodyBytes`, server.go:35, :140); over the
  cap is 413 `body_too_large`; unknown JSON fields and trailing data are 400, so a typo such as "reasn" cannot
  become an empty reason (handlers.go:54-88). Tests: TestDBBodyTooLarge413 (handlers_db_test.go:311) and the
  "unknown field" and "trailing data" cases of TestDBInvalidInput400NoAudit (handlers_db_test.go:179-180).
- **No raw error text.** Unknown errors are 500 with generic text and the detail only in the server log; 503 busy
  and 409 changed send fixed texts and log the wrapped Postgres or apply error (errors.go:97-121). Test:
  TestDBInternalErrorNotLeaked closes the pool and checks the body has no detail while the log has it
  (handlers_db_test.go:452). Fixed texts for 409 changed and 503 busy: TestFailHidesWrappedDetail
  (errors_test.go:51) and TestDBBusy503 (handlers_db_test.go:435).
- **Audited, attributed actions.** Replay and undo require `actor` and `reason`, call the same audited
  internal/admin functions as the CLI, and invalid input writes no audit row (TestDBInvalidInput400NoAudit,
  handlers_db_test.go:171; refusals audited: TestDBReplayRefusedIsAudited, :151). The UI never pre-fills actor
  (Q7, plan :149; app.js:379-383 comment and form). `actor` is self-declared, not an identity (plan :63-64).
- **Untrusted text is never parsed as markup.** The UI renders payloads, actor, reason and errors with
  textContent through one attribute allow-list helper (app.js `el()`; plan :27-28 for textContent). Tests: TestUISourceHasNoMarkupOrCodeSinks
  and TestElAttributeAllowList (static_test.go:94, :107). The CSP is the second line of defence.
- **Detached calls.** Admin calls run on `context.WithoutCancel` with a 30s timeout, so a closed tab cannot cancel
  a replay or undo half way (handlers.go:15-20; plan :58-60). This is stated in code and plan; no test in this
  decision exercises a client disconnect.

## Consequences
- What it stops: a hostile web page in the operator's browser, including a DNS-rebinding page, is refused by the
  layers above before any handler runs. Each of the bind, Host, cross-origin, content-type and body-limit layers
  has a test that pairs the refused requests with an allowed control request. The detached-context layer has no
  test (see Decision). These are handler-level tests, not a browser experiment.
- No authentication: anyone with a shell or a browser on this machine can read all events and replay or undo any
  of them. The audit trail then records a self-declared actor, as 0004 already notes (0004:51, :54).
- Not tested: no penetration test, no browser-based rebinding experiment, no TLS or proxy setup. docs/phase7-results.md
  says it is "not a load, concurrency or security test" (Limitations). The evidence there is a single-operator
  transcript and screenshots of fixture data.
- Cost: refusing `::1` and loopback addresses other than 127.0.0.1 is stricter than the plan text (plan :51 already
  refuses hostnames such as `localhost`) and may surprise an IPv6-only setup. A mounted static route answers
  unknown names with the JSON `no_route` 404 (static.go:25-34). Test: TestStaticMissingIsJSON404 (static_test.go:51).
- Before exposing it beyond localhost (author's reasoning, not in the plan): real authentication with per-user
  identities instead of a self-declared actor, TLS, a Host allow-list taken from configuration, a re-review of the
  CSRF and CORS assumptions (they rely on same-machine browsers), a rate limit, and an audit table the writing role
  cannot alter (the same revisit as 0004).
