# ZeroBounce vendor adapter (P6 external fallback)

> **NOTE — read first. What has been checked, and by whom.** On **2026-11-17 the owner read ZeroBounce's live status-codes page** (<https://www.zerobounce.net/docs/email-list-validation/status-codes>) and ruled on the (status, sub_status) mapping; that ruling is the source of §4: `valid` also has the sub-statuses `alias_address`, `alternate` and `accept_all`; `do_not_mail` also has `mx_forward`; the eight `unknown` sub-statuses are all non-answers, and the page states that ZeroBounce does not charge for `unknown`. Our own tooling still gets a **Cloudflare 403** from that host (the documentation pages were never reachable for it), so the ruling is the owner's reading, not a fetch anyone can repeat from here; it replaced the earlier SDK-only reading (§1). Four pairs are **not** on the page and stay mapped as **SDK-only pairs** (§4), and `blocked` and `allowed` are on neither the page nor in any pair, so they stay non-answers. **Not yet checked:** everything in §6 (UNVERIFIED) — the 429 contract, the exact error body, the price per answer, the `timeout` semantics, whether a call *we* abandon is charged, the regional hosts — must be checked against the live documentation by the owner before the vendor dial is switched on (an owner action).

**Status:** built, tested against a stand-in, and **disabled by default**. No real vendor call is made in CI, and no real key is anywhere in the repository or the workflow.
**What this is:** the first real adapter behind the existing `external.Vendor` interface ([P6 design](history/phase-6-fallback-spec.md): the residue-only cost firewall, anti-laundering, the OQ-P6-3 asymmetry, spend-once, the breaker). It is built **behind** that design, not around it: `external.Resolve`, `external.Classify`, `external.Decide` and the stage are unchanged except for one additive budget field (§4).
**Code:** `control-plane/internal/external/zerobounce.go` (client), `zerobounce_map.go` (the table below), `control-plane/cmd/control-plane/main.go` (`buildExternal`), `control-plane/test/zbstub` (the stand-in).
**Owner ruling implemented:** wire ZeroBounce behind the existing design; only the residue goes to the vendor; a vendor `valid` becomes `risky` / `external_verified` at the policy's capped confidence, never `valid`; a vendor "no" is trusted more than a vendor "yes"; spend-once; the global vendor kill switch; the module ships disabled.

> **policy.vendor is informational and currently names a different vendor; the external stage uses the compiled vendor; changing the policy is a separate decision.** The shipped `cold-email` v1 policy says `"vendor": "millionverifier"`. The stage never reads that field: it keys the budget, the breaker and the `external_calls` rows by `Vendor.Name()` (`zerobounce`). A policy is versioned and immutable, and a new version changes the cache fingerprint, so it was deliberately not edited here. `buildExternal` logs the mismatch once at startup.

---

## 1. Sources read (and what each one confirms)

**The live page (the source of the mapping).** [`status-codes`](https://www.zerobounce.net/docs/email-list-validation/status-codes), read by **the owner on 2026-11-17**. Our tooling cannot fetch it (Cloudflare 403), so the text below is the owner's ruling, not a copy of the page. It says: `valid` has the sub-statuses `alias_address`, `alternate` and `accept_all` (besides the ones already mapped); `do_not_mail` has `mx_forward`; `unknown` has `antispam_system`, `exception_occurred`, `failed_smtp_connection`, `forcible_disconnect`, `greylisted`, `mail_server_did_not_respond`, `mail_server_temporary_error` and `timeout_exceeded`, and ZeroBounce does not charge for an `unknown`; `blocked` and `allowed` are not on the page; and the four pairs `valid`/`gold`, `valid`/`role_based_accept_all`, `valid`/`ai_agent_mailbox` and `catch-all`/`ai_agent_mailbox` are not on it either. The ruling names those four as the only mapped pairs the page lacks, so every other row of §4 is labelled **page** on that basis; if the page turns out to differ for a row, demote it by deleting it from `zbRows` (it becomes a non-answer) and change its row here.

**The official SDKs** (read earlier, on **2026-11-16 (UTC)**, from `github.com/zerobounce`, ZeroBounce's own organisation, at the commits below; nothing was cloned into this repository). They gave the status and sub-status sets and the example pairs the mapping was first written from, and they remain the only source for the four SDK-only pairs.

| Short | Repository | Commit | What it gave |
|---|---|---|---|
| **Go** | [`zerobounce/zerobouncego`](https://github.com/zerobounce/zerobouncego) (v3) | `9286ea5b0a1164b60aa41e64128ea52a213605cf` | [`validate_constants.go`](https://github.com/zerobounce/zerobouncego/blob/9286ea5b0a1164b60aa41e64128ea52a213605cf/validate_constants.go) (7 statuses, 31 sub-statuses); [`mocking_validation_content_test.go`](https://github.com/zerobounce/zerobouncego/blob/9286ea5b0a1164b60aa41e64128ea52a213605cf/mocking_validation_content_test.go) (the sandbox answer for each test address: the **(status, sub_status) pairs**); [`documentation.md`](https://github.com/zerobounce/zerobouncego/blob/9286ea5b0a1164b60aa41e64128ea52a213605cf/documentation.md) and [`http_error.go`](https://github.com/zerobounce/zerobouncego/blob/9286ea5b0a1164b60aa41e64128ea52a213605cf/http_error.go) (error shape; 429 named; redirects never followed because the key travels with the request; a server that echoes the key); `CHANGELOG.md` |
| **Rust** | [`zerobounce/zerobounce-rust-api`](https://github.com/zerobounce/zerobounce-rust-api) | `9cdf82de8b25f134bc4f112d96ba34b5b12cb67e` | [`src/utility/structures/validate_enums.rs`](https://github.com/zerobounce/zerobounce-rust-api/blob/9cdf82de8b25f134bc4f112d96ba34b5b12cb67e/src/utility/structures/validate_enums.rs) (the same 7 + 31) |
| **Ruby** | [`zerobounce/zero-bounce-ruby`](https://github.com/zerobounce/zero-bounce-ruby) (`develop`) | `bb1488a377cb22057b856b9318086e3cb79e4831` | [`validate_status.rb`](https://github.com/zerobounce/zero-bounce-ruby/blob/bb1488a377cb22057b856b9318086e3cb79e4831/lib/zerobounce/validate_status.rb) + [`validate_sub_status.rb`](https://github.com/zerobounce/zero-bounce-ruby/blob/bb1488a377cb22057b856b9318086e3cb79e4831/lib/zerobounce/validate_sub_status.rb) (the same 7 + 31); [`documentation.md`](https://github.com/zerobounce/zero-bounce-ruby/blob/bb1488a377cb22057b856b9318086e3cb79e4831/documentation.md) (30 credit-free test addresses with their (status, sub_status) answers) |
| **Python v2** | [`zerobounce/zerobounce-python-api-v2`](https://github.com/zerobounce/zerobounce-python-api-v2) | `3acbb2377affefa3c5def568205000a7cf8272ff` | README: 7 statuses, 22 sub-statuses ("Properties and possible values"), the credit-free test addresses, the response field list |
| **JavaScript v2** | [`zerobounce/zerobounce-javascript-api-v2`](https://github.com/zerobounce/zerobounce-javascript-api-v2) | `bb7d76a614d5569cc2db371b2438a202fc91da14` | README: the same 7 + 22 |
| **PHP v2** | [`zerobounce/zerobounce-php-api-v2`](https://github.com/zerobounce/zerobounce-php-api-v2) | `6c5affc287a38eceb0595bee03a846e787b3cc5d` | README: the same 7 + 22 (writes `possible_traps`, a typo for `possible_trap`) |
| **Java v2** | [`zerobounce/zerobounce-java-api-v2`](https://github.com/zerobounce/zerobounce-java-api-v2) | `1d7decf17e2735eec17abc7cb0a6575f7af350ff` | README: 7 statuses, 22 + `alias_address`; "if the API times out it returns status Unknown and sub_status `timeout_exceeded`" |
| **Postman** | [`zerobounce/postman-v2`](https://github.com/zerobounce/postman-v2) | `865403bb4504375e9dfd66fadc1d6c97e56a9911` | the validate request: `GET {base}/validate?api_key&email&ip_address[&timeout 3-60s…]`; base `https://api.zerobounce.net/v2`, regional `api-us` / `api-eu` |

**The sets used (the union across the SDKs; the live page is a subset of the 32 words below, `blocked` and `allowed` being SDK-only words):**

* **Statuses (7):** `valid`, `invalid`, `catch-all`, `unknown`, `spamtrap`, `abuse`, `do_not_mail` — in all eight sources.
* **Sub-statuses (31):** 22 appear in Go, Rust, Ruby, Python, JavaScript, PHP and (bar two) Java: `antispam_system`, `greylisted`, `mail_server_temporary_error`, `forcible_disconnect`, `mail_server_did_not_respond`, `timeout_exceeded`, `failed_smtp_connection`, `mailbox_quota_exceeded`, `exception_occurred`, `possible_trap`, `role_based`, `global_suppression`, `mailbox_not_found`, `no_dns_entries`, `failed_syntax_check`, `possible_typo`, `unroutable_ip_address`, `leading_period_removed`, `does_not_accept_mail`, `role_based_catch_all`, `disposable`, `toxic`. `alias_address` appears in Go, Rust, Ruby and Java (4). Eight more appear in **exactly three** (Go, Rust, Ruby — the newer SDKs): `accept_all`, `alternate`, `mx_forward`, `blocked`, `allowed`, `role_based_accept_all`, `gold`, `ai_agent_mailbox`. The vendor sends `""` when there is no sub-status.
* **Answer shape** (a `200` body): `address`, `status`, `sub_status`, `free_email`, `did_you_mean`, `account`, `domain`, `domain_age_days`, `smtp_provider`, `mx_found`, `mx_record`, `firstname`, `lastname`, `gender`, `country`, `region`, `city`, `zipcode`, `processed_at` (Python README; Ruby/Go examples).

---

## 2. Gating: every switch is independent, and any one off sends nothing

Evaluated in this order. The first six are decided once at startup by `buildExternal`; the rest are decided per residue by the stage and the adapter.

| # | Gate | Where | Default | Off means |
|---|---|---|---|---|
| 1 | **Kill-switch rung** `TM_FALLBACK_MODE=enabled` (and the global-fallback-kill, `off`, which overrides everything below) | `config.ExternalFallbackEnabled` | `off` | `buildExternal` returns nil: the layer does not exist |
| 2 | **DPA confirmed** `TM_EXTERNAL_DPA_CONFIRMED=true` (exactly the string `true`) | same | `false` | nil |
| 3 | **Policy opts in**: the `external` block exists | `buildExternal` | shipped policy has it | nil |
| 4 | **Vendor key present** in the environment variable named by `TM_EXTERNAL_ZEROBOUNCE_KEY_ENV` | `buildExternal` | variable unset | nil (the log names the *variable*, never a value) |
| 5 | **Adapter config valid** (base URL https, timeout in range) | `config.Validate`, `NewZeroBounce` | valid defaults | startup error / nil |
| 6 | **Hard daily spend cap** `TM_EXTERNAL_DAILY_SPEND_CAP` | `external.Decide` (first check) | **`0` = no calls** | the stage is wired but **every call is refused** (`vendor_daily_spend_cap`); `buildExternal` logs it |
| 7 | **Global kill** `ExternalDeps.GloballyDisabled` | stage | false | `runExternal` returns before any call |
| 8 | **Policy `external.enabled` and `route_when`** include this residue class | stage | all four classes | no escalation |
| 9 | **Residue class** (`Classify`: catch_all, provider_no_smtp, retry_exhausted_greylisted, gave_up_rate_limited) | stage | closed set | nothing else is ever eligible |
| 10 | **Budget and breaker** (`Decide`): hard vendor cap → per-job → owner-OQ daily → monthly → calls/minute → breaker cooldown / error rate | stage | caps unset except #6 | `keep_residue`, no call |
| 11 | **429 pause** | adapter | none | refused locally, **no request** |
| 12 | **In-flight collapse**: concurrent identical lookups share one request | adapter | on | the follower shares the leader's answer with cost 0 |

A call that passes all twelve is ONE `GET /v2/validate` carrying only `api_key`, `email`, an **empty** `ip_address` and `timeout` (address-only egress, [privacy §7.2](history/phase-6-fallback-spec.md)). A queued escalation's spend record is written inside the settlement compare-and-set (spend-once, unchanged).

---

## 3. Dials

| Dial | Type | Default | Validation | Consumed by |
|---|---|---|---|---|
| **`TM_EXTERNAL_DAILY_SPEND_CAP`** | dial (vendor credits per rolling 24 h) | **`0`** (= no calls) | non-negative integer | `buildExternal` → `external.Budget.VendorDailyCap` (`VendorDailyCapEnforced=true`) → `Decide` |
| **`TM_EXTERNAL_ZEROBOUNCE_KEY_ENV`** | dial (a *name*) | `ZEROBOUNCE_API_KEY` | upper-case env-var name (`[A-Z_][A-Z0-9_]*`, ≤ 64), **not** starting `TM_`; the value is never echoed | `buildExternal` reads `os.Getenv(<name>)` |
| **`TM_EXTERNAL_ZEROBOUNCE_TIMEOUT_MS`** | dial | `8000` | integer 3000–60000 (ZeroBounce's own documented `timeout` range is 3–60 s) | `ZeroBounceConfig.Timeout`; also sets the vendor-side `timeout` parameter just under it |
| **`TM_EXTERNAL_ZEROBOUNCE_BASE_URL`** | dial | `https://api.zerobounce.net` | `scheme://host[:port]` only; **https**, except a loopback / `zbstub` host over http when `TM_ENV` is `development` or `test`; no credentials, path, query or fragment | `ZeroBounceConfig.BaseURL` (`api-us` / `api-eu` are the published regional hosts) |
| `ZEROBOUNCE_API_KEY` (or whatever the key-env dial names) | **secret** | unset | non-empty after trimming | the client; held in memory only, redacted from every `String`/`%#v`/error |

**No new "enabled" flag.** The vendor-enabled switch already exists and is reused: `TM_FALLBACK_MODE=enabled` **and** `TM_EXTERNAL_DPA_CONFIRMED=true`. Nothing in the table above turns anything on (`TestZB_Config_NewDialsDoNotEnableTheVendor`).

**How the new cap relates to the existing §5.2 ceilings.** `TM_EXTERNAL_DAILY_CAP`, `_MONTHLY_CAP`, `_PER_JOB_CAP` and `_RATE_CEILING` are the owner-OQ dials: **unset means "not enforced"**, they apply to any vendor, and they are unchanged. `TM_EXTERNAL_DAILY_SPEND_CAP` is a separate, **hard** per-vendor ceiling where **`0` means "no calls"**, so enabling the vendor without choosing a number sends nothing. It is checked **in addition**: the lower of it and `TM_EXTERNAL_DAILY_CAP` wins, each reporting its own trip reason (`vendor_daily_spend_cap` / `budget_daily_cap`). Both read the same durable `external_calls` spend (ok calls only, rolling 24 h), so a restart cannot reset either. It was not folded into `TM_EXTERNAL_DAILY_CAP` because that dial's contract ("0/unset ⇒ not enforced") is the opposite of what a vendor cap needs, and changing it would silently alter every existing deployment.

---

## 4. The mapping

`status` and `sub_status` are matched **exactly and case-sensitively**, and **a sub-status is read only together with its status**. The table is a **whitelist**: a pair not listed (below, "Non-answers") is never turned into a verdict. The source is the live status-codes page as the owner read it on 2026-11-17 (§1); the table has **26** rows: **22** labelled **page** and **4** labelled **SDK-only pair**.

**Adapter result** is what the adapter hands to `Resolve` (a `vocab.Status`); `Resolve` then applies the existing rules and the adapter never produces a final verdict itself:

* **valid** → always `risky` / `external_verified`, capped (anti-laundering, §3.4 — a rented "yes" is never SMTP-grade `valid`).
* **risky** → `risky` / `external_verified`, capped.
* **invalid** → `invalid` / `external_verified` **only** on `retry_exhausted_greylisted` and `gave_up_rate_limited` (independent non-existence is plausible); **downgraded to `risky`** on `catch_all` and `provider_no_smtp` (OQ-P6-3: on an accept-all or :mx-routed residue a vendor "no" is itself a guess, and a false `invalid` irreversibly discards a real address).

The **reason is always `external_verified`** (reserved in P1; no vocabulary change). The **confidence is the policy's `external.confidence_cap`** (60 in `cold-email` v1): ZeroBounce's validate answer carries no confidence of its own, so the cap is the value, and it can never be exceeded. Recommendation follows the existing rule (`risky` → `review`, `invalid` → `skip`; `invalid`+`send` stays forbidden).

Evidence column. **page** = the pair is on the live status-codes page (owner's reading, 2026-11-17; §1). **SDK-only pair** = the pair is **not** on the page; it is kept mapped (the owner's ruling) because an official SDK shows ZeroBounce answering with it. After the label: **pair** = an official SDK shows exactly this pair (Ruby `documentation.md` and/or Go `mocking_validation_content_test.go`, both read 2026-11-16, commits in §1); **sets** = the pair's two words are in the SDK union above (no SDK pairs them); **word in** = the sub-status word is in those SDKs but no response they show pairs it with a status.

| ZeroBounce `status` | `sub_status` | Adapter result | M1 status on catch_all / provider_no_smtp | M1 status on retry_exhausted / gave_up | M1 reason | Confidence | Why | Evidence |
|---|---|---|---|---|---|---|---|---|
| `valid` | `(empty)` | valid | risky | risky | external_verified | cap (60) | A rented "yes". Anti-laundering: never `valid`. | **page**; pair: Ruby, Go, Python/JS/PHP/Java test-address lists |
| `valid` | `leading_period_removed` | valid | risky | risky | external_verified | cap | Vendor validated an altered address; still a "yes". | **page**; pair: Ruby, Go |
| `valid` | `alias_address` | valid | risky | risky | external_verified | cap | Added 2026-11-17 (owner's ruling): `valid` also has this sub-status. Mapped like every other vendor `valid`: a rented "yes", never M1 `valid`. | **page** (added 2026-11-17); word in: Go, Rust, Ruby, Java |
| `valid` | `alternate` | valid | risky | risky | external_verified | cap | Added 2026-11-17 (owner's ruling): `valid` also has this sub-status. Mapped like every other vendor `valid`. | **page** (added 2026-11-17); word in: Go, Rust, Ruby |
| `valid` | `accept_all` | valid | risky | risky | external_verified | cap | Added 2026-11-17 (owner's ruling): `valid` also has this sub-status. Mapped like every other vendor `valid`; on a `catch_all` residue it raises M1's own 40 to the cap without adding information (§7.1). | **page** (added 2026-11-17); word in: Go, Rust, Ruby |
| `valid` | `gold` | valid | risky | risky | external_verified | cap | Meaning undocumented in the SDKs; mapped by its status class only (a "yes"). **Not on the live page; pair in one SDK only.** | **SDK-only pair**; pair: Go; sets: Go, Rust, Ruby |
| `valid` | `role_based_accept_all` | valid | risky | risky | external_verified | cap | A role address on an accept-all domain: weak "yes". **Not on the live page; pair in one SDK only.** | **SDK-only pair**; pair: Go; sets: Go, Rust, Ruby |
| `valid` | `ai_agent_mailbox` | valid | risky | risky | external_verified | cap | Meaning undocumented; mapped by status class. **Not on the live page; pair in one SDK only.** | **SDK-only pair**; pair: Go; sets: Go, Rust, Ruby |
| `catch-all` | `(empty)` | risky | risky | risky | external_verified | cap | The domain accepts everything; cannot prove the mailbox. | **page**; pair: Ruby, Go |
| `catch-all` | `ai_agent_mailbox` | risky | risky | risky | external_verified | cap | As above. **Not on the live page; pair in one SDK only.** | **SDK-only pair**; pair: Go; sets: Go, Rust, Ruby |
| `invalid` | `mailbox_not_found` | invalid | risky | **invalid** | external_verified | cap | Hard negative: positive evidence the mailbox does not exist. | **page**; pair: Ruby, Go |
| `invalid` | `no_dns_entries` | invalid | risky | **invalid** | external_verified | cap | Hard negative: no DNS records for the domain. | **page**; pair: Ruby, Go |
| `invalid` | `failed_syntax_check` | invalid | risky | **invalid** | external_verified | cap | Hard negative: not a valid address. | **page**; pair: Ruby, Go |
| `invalid` | `does_not_accept_mail` | invalid | risky | **invalid** | external_verified | cap | Hard negative: the domain does not accept mail. | **page**; pair: Ruby, Go |
| `invalid` | `mailbox_quota_exceeded` | risky | risky | risky | external_verified | cap | Vendor says `invalid`, but a full mailbox **exists**; M1 itself maps a 552 to `valid/mailbox_full`. Cautious side. See §5 A. | **page**; pair: Ruby, Go |
| `invalid` | `possible_typo` | risky | risky | risky | external_verified | cap | "Possible": a suggestion, not proof of absence. Cautious side. See §5 A. | **page**; pair: Ruby, Go |
| `invalid` | `unroutable_ip_address` | risky | risky | risky | external_verified | cap | The mail host is unroutable. ADR-0021 treats a non-public MX as `risky`, never `invalid`. Cautious side. See §5 A. | **page**; pair: Ruby, Go |
| `spamtrap` | `(empty)` | invalid | risky | **invalid** | external_verified | cap | Hard negative. M1's own policy already maps a spam-trap hit to `invalid:spam_trap`. | **page**; pair: Ruby, Go |
| `abuse` | `(empty)` | risky | risky | risky | external_verified | cap | Meaning not stated in any SDK; "deliverable but unwanted" is the cautious reading (never `invalid`, OQ-7). See §5 A. | **page**; pair: Ruby, Go |
| `do_not_mail` | `disposable` | risky | risky | risky | external_verified | cap | PROJECT_RULES §2 / OQ-7: a disposable address is a real mailbox, so `risky`, **never** `invalid`. | **page**; pair: Ruby, Go |
| `do_not_mail` | `role_based` | risky | risky | risky | external_verified | cap | A role address is deliverable but unwanted. | **page**; pair: Ruby, Go |
| `do_not_mail` | `role_based_catch_all` | risky | risky | risky | external_verified | cap | As above, on an accept-all domain. | **page**; pair: Ruby, Go |
| `do_not_mail` | `toxic` | risky | risky | risky | external_verified | cap | Deliverable-but-unwanted ("don't want", never "won't deliver"). | **page**; pair: Ruby, Go |
| `do_not_mail` | `global_suppression` | risky | risky | risky | external_verified | cap | Suppressed — because it bounced or because it complained? Ambiguous: cautious side. See §5 A. | **page**; pair: Ruby, Go |
| `do_not_mail` | `possible_trap` | risky | risky | risky | external_verified | cap | "Possible": not proof. Cautious side. See §5 A. | **page**; pair: Ruby, Go |
| `do_not_mail` | `mx_forward` | risky | risky | risky | external_verified | cap | Added 2026-11-17 (owner's ruling): `do_not_mail` also has this sub-status. Mapped like the other `do_not_mail` sub-statuses: deliverable-but-unwanted is `risky`, never `invalid` (OQ-7). | **page** (added 2026-11-17); word in: Go, Rust, Ruby |

### Non-answers (the vendor spoke, or failed to; **M1's own verdict stands**)

A non-answer is **not a vendor outcome**: no verdict is formed, the call is recorded as an **error** (cost 0, no vendor status, so the breaker's error window sees it and **no vendor-charged marker appears**), and the residue verdict is returned unchanged.

| ZeroBounce `status` | `sub_status` | Kind | Why |
|---|---|---|---|
| `unknown` | **any** — `(empty)` and the eight the live page lists: `antispam_system`, `exception_occurred`, `failed_smtp_connection`, `forcible_disconnect`, `greylisted`, `mail_server_did_not_respond`, `mail_server_temporary_error`, `timeout_exceeded` (and every other) | `no_answer` | The vendor itself could not tell. M1 keeps `unknown` first-class (PROJECT_RULES §2): the fallback can only improve a residue verdict, never degrade it. The call is recorded as an **error, cost 0, no vendor-charged marker**: ZeroBounce does not charge for `unknown` (live page, §6), which is exactly this rule. |
| any status **other than `valid`** | `accept_all`, `alternate`, `alias_address` | `unrecognised` | On the live page these are sub-statuses of `valid` only; under any other status no page or SDK pairs them, and a sub-status is never read apart from its status. |
| any status **other than `do_not_mail`** | `mx_forward` | `unrecognised` | On the live page `mx_forward` is a sub-status of `do_not_mail` only. |
| any status | `blocked`, `allowed` | `unrecognised` | **Not on the live page**; the SDKs name them but never pair them with a status (§5 B). They stay non-answers until the owner says otherwise. |
| `valid` / `catch-all` / `invalid` / `spamtrap` / `abuse` / `do_not_mail` | any sub-status **not** in the table above (e.g. `invalid` with an empty sub-status; `catch-all` + `gold`) | `unrecognised` | A pair that is neither on the live page nor one of the four SDK-only pairs is not a pair we may read a verdict from. |
| a status or sub-status this build has never seen, or in a different case | — | `unrecognised` | `Valid`, `totally_new_status`, `some_future_sub_status` … |
| (HTTP) any non-200, including `403` from the edge, `5xx`, a refused redirect | — | `http_status` | The answer is not from the API. |
| (HTTP) `429` | — | `rate_limited` | See §6 (UNVERIFIED). A pause follows; during it **no request is made**. |
| (200) a body with `error`, `error_message` or `Error`, or `success: false` | — | `error_body` | The live API answers an invalid key or an empty account with HTTP 200 and `{"error": …}` (Go SDK). |
| (200) not JSON, truncated, an array, `{}`, a bare `{"Message": …}`, no `status`, no `address`, a non-string `status`/`sub_status`, > 64 KiB | — | `malformed` | |
| timeout, connect/TLS failure, a body cut short | — | `transport` | Our deadline, not the vendor's. |

---

## 5. Not honestly mappable

### A. Ambiguous between "unwanted" and "does not exist" — mapped to `risky` (the cautious side)

PROJECT_RULES §2 and OQ-7 win: `invalid` means exactly one thing (positive proof of non-deliverability). Where ZeroBounce's own words leave it open, the pair is `risky`, never `invalid`.

* `invalid` / `mailbox_quota_exceeded` — the mailbox exists (M1 maps a 552 to `valid`/`mailbox_full`).
* `invalid` / `possible_typo` — "possible".
* `invalid` / `unroutable_ip_address` — ADR-0021 treats the same fact as `risky`.
* `do_not_mail` / `global_suppression`, `do_not_mail` / `possible_trap`.
* `abuse` — the SDKs list the status but never describe it.
* **SDK-only pairs** (NOT on the live status-codes page; the owner's 2026-11-17 ruling keeps them mapped, by status class, labelled so the owner can keep or demote them): `valid`/`gold`, `valid`/`role_based_accept_all`, `valid`/`ai_agent_mailbox`, `catch-all`/`ai_agent_mailbox`. The sub-status words are in three SDKs; the *pairs* are shown only in the Go SDK's sandbox responses. To demote one, delete its row from `zbRows` (it becomes a non-answer) and from §4.

### B. No honest mapping — treated as a non-answer in code

* `blocked` and `allowed` (Go, Rust, Ruby only): **not on the live page**, named in the SDKs but **never paired with a status** in any response they show, and their meaning is not stated. `allowed` / `blocked` plausibly report an account-level allow/block rule (the Filters API), which says nothing about the mailbox. A verdict cannot be formed from a word with no status and no meaning.
* `accept_all`, `alternate`, `alias_address` under any status **other than `valid`**, and `mx_forward` under any status other than `do_not_mail`: they are mapped under the status the live page gives them (§4) and **only** there. No page or SDK pairs them with another status, so such a pair is `unrecognised` (a response a test double can produce, never one we have seen from the vendor).
* every `unknown` sub-status (the vendor's own non-answer), and every pair not in §4.

---

## 6. UNVERIFIED (not confirmed by the live status-codes page or any SDK; the code is conservative about each)

**Resolved on 2026-11-17 (owner, live status-codes page): credits for `unknown`.** ZeroBounce does not charge for `unknown`. It was an open item here until then; it is now confirmed, and it is exactly the rule the code already followed (`unknown` and every other non-answer is recorded **cost 0**, no vendor-charged marker). It is no longer in the table below. What the page does not say, and stays open below, is the price of an *answered* call and whether a call that **we** abandon (our deadline, a dropped connection) is charged.

| Item | What the SDKs say | What the code does |
|---|---|---|
| **429 / rate-limit behaviour** | Go names a `429` ("a 429 (rate limit)"); **no** SDK states a `Retry-After` header, a limit, or a window | A `429` is a non-answer. The client pauses **without making any request**: `Retry-After` honoured (seconds or HTTP date), absent or unreadable → 10 s, clamped to 1 s–5 min. Constants `zbDefaultRatePause`, `zbMinRatePause`, `zbMaxRatePause`. |
| **The exact error body** | Go: `{"error": "…"}` on HTTP 200 for an invalid or drained key (also `{"Error": …}`, `{"success": false}`) | Any `error` / `error_message` / `Error` key, or a falsy `success`, is a non-answer. The text is never read or stored. |
| **A call that OUR side abandons (our timeout, a dropped connection, a refused redirect) and whether a price differs per status** | Not stated. The SDKs say only that their *test* addresses are credit-free; the page's "`unknown` is not charged" concerns a ZeroBounce-side `unknown` (including its own `timeout_exceeded`), not a request we cut off. | Every non-answer, ours included, is recorded **cost 0** (the ruling: no vendor-charged marker). **If ZeroBounce bills a request we abandoned, the spend cap under-counts those calls** — check the account's credit balance against `external_calls` after the first live day. |
| **Price per answered call** | Not stated | Assumed **1 credit per answer** (`CostUnits=1`); the daily cap is in these units. |
| **`timeout` parameter semantics** | Postman: 3–60 s; "if met, the API will return unknown / greylisted"; Java: timeout → `unknown`/`timeout_exceeded` | Sent as our timeout minus one second, clamped to 3–60. |
| **`ip_address`** | Postman: "can be blank, but parameter required" | Sent empty. We never send a caller IP. |
| **Regional hosts** | Postman: `api-us`, `api-eu` | Selectable with the base-URL dial; the default is the global host. |
| **The rest of the live documentation** | §4 now follows the live status-codes page (owner, 2026-11-17), apart from its four SDK-only pairs and the page's silence on `blocked` / `allowed`. Everything else in this document (the `validate` request, the answer shape, the error body, the 429 contract, the `timeout` and `ip_address` parameters, the regional hosts) is still from the SDKs and the Postman collection | **The owner must check those items against the live documentation before switching the vendor on** (§8 step 2). |

---

## 7. Behaviours worth knowing before the vendor is switched on

1. **Confidence is the cap, for every answered row.** A vendor `catch-all` on a `catch_all` residue therefore raises M1's own 40 to the cap (60) without adding information, and so does a vendor `valid`/`accept_all` (the vendor itself says the domain accepts everything, yet the row maps like every other vendor `valid`, per the owner's 2026-11-17 ruling). This is the existing design (`Resolve`: vendor risky → `risky`/`external_verified` at the cap), kept as is; a per-reason ceiling would be a policy change.
2. **The spend cap is check-then-call.** With N concurrent callers it can be exceeded by up to N−1 credits before their records land (the existing design has the same property for every ceiling).
3. **Spend-once is exact in-process, bounded across processes.** Concurrent identical lookups share one request and one credit. A queued retry after a lease loss, after the first call has *returned*, is not collapsed (ZeroBounce offers no idempotency key): the design's bounded one-extra-lookup worst case. The `external_calls` record is still written once, inside the settlement CAS.
4. **Non-answers count toward the error-rate breaker.** A residue is by nature the hard tail, so a high share of `unknown` is plausible; if `TM_EXTERNAL_ERROR_RATE_PCT` is set it can trip the breaker on vendor `unknown`s. It ships at 0 (off).
5. **The raw sidecar is a whitelist projection** (`status`, `sub_status`, `free_email`, `mx_found`, `mx_record`, `smtp_provider`, `domain_age_days`, `processed_at`). The response's `address`, `account`, `did_you_mean`, names, gender and location are **dropped**: the raw address is never persisted (Door 3).
6. **The key rides in the query string** (the only documented way) so no error is built from a transport error, a response body or a URL: messages are static (a kind plus an HTTP status integer). Redirects are never followed. `String`, `%+v` and `%#v` of the client and its config are redacted.

---

## 8. Enablement runbook (all owner actions; nothing here is done by the code)

1. **Account.** Open a ZeroBounce account; note the credit balance; choose the regional host (`api`, `api-us`, `api-eu`).
2. **Check the rest against the live documentation.** The status mapping (§4) was checked by the owner against the live status-codes page on 2026-11-17 (the NOTE at the top). Still to check: the items in §6 (the 429 contract, the error body, the price per answer, the `timeout` / `ip_address` semantics, the regional hosts, a request we abandon) and the four SDK-only pairs, which the page does not list. Correct `zerobounce_map.go` and this file together (`TestZB_MapTable_DocInSync` keeps them in step, label and adapter result included).
3. **Key.** Put the key in the environment variable named by `TM_EXTERNAL_ZEROBOUNCE_KEY_ENV` (default `ZEROBOUNCE_API_KEY`), from the secrets manager, never a committed file. (`control-plane/config/secrets.example.env` shows the slot.)
4. **DPA.** Complete the §7.4 checklist (DPA signed, retention understood, TLS, key custody). Only then set `TM_EXTERNAL_DPA_CONFIRMED=true`.
5. **Cap.** Choose `TM_EXTERNAL_DAILY_SPEND_CAP` (credits per rolling 24 h). Start small. Optionally set the owner-OQ `TM_EXTERNAL_DAILY_CAP` / `_MONTHLY_CAP` / `_PER_JOB_CAP` / `_RATE_CEILING` and `TM_EXTERNAL_ERROR_RATE_PCT`.
6. **Switch on.** Set `TM_FALLBACK_MODE=enabled` and restart. The boot log must say `ZeroBounce wired (… daily_spend_cap=N …)`; if it says the cap is 0, nothing will be sent.
7. **Watch the first day.** `external_calls` (`outcome`, `cost_units`, `vendor_status_raw`) against the ZeroBounce account's own balance; the share of non-answers; `/readyz`.
8. **Switch off.** `TM_FALLBACK_MODE=off` and restart (the global-fallback-kill): no vendor egress in one action. Lowering the cap to 0 also stops every call while leaving the wiring in place.

---

## 9. The stand-in and CI

* **What it is.** `control-plane/test/zbstub/main.go`: a small stdlib-only Go server that plays `GET /v2/validate`. The response is chosen by the **local part** of the address: `<status>[--<sub_status>]` serves any pair as written (every row above, the four newly mapped pairs `valid--alias_address`, `valid--alternate`, `valid--accept_all` and `do_not_mail--mx_forward` among them, every `unknown` sub-status, a pair that is not on the live page, a status nobody has seen; whether a pair is an answer is decided by the adapter's table, never by the stand-in); `fail--<mode>` serves the failure modes — an `error` body on HTTP 200 (key, credits, one that echoes the key, capitalised, `success:false`), the bare greylist `Message`, `429` with and without `Retry-After`, `500`, `503`, an HTML `403`, a `302`, a slow response, malformed JSON, a truncated body, an HTML `200`, an empty body, `{}`, `[]`, a non-string status, a missing address, an unknown status. `+dly<ms>` holds an answer back so a test can overlap two calls; `+<tag>` groups requests.
* **Counters.** `GET /__calls` (total and per tag), `GET /__calls?tag=` / `?email=`, `GET /__requests?tag=` (the parameter *names* each request carried and the key's *length*; never the key). Every request is counted on arrival. This is how "disabled sends nothing" and "the N+1th call was refused" are proven: the count for the test's own tag is 0 or N.
* **How it is built and pinned.** `control-plane/test/zbstub/Dockerfile`: a two-stage build, both bases pinned by digest — `golang:1.26.11-alpine3.24@sha256:ecb67f98…` and `alpine:3.24@sha256:f59abe65…` (the same alpine pin the DNS and implicit-MX fixtures run under). The module still has exactly one `go.mod`; the image makes its own inside the build stage. `TestZB_StandInDockerfile_BasesPinnedByDigest` fails if either `FROM` loses its digest (the base-image-pinned gate reads only the verifier node's Dockerfile and the workflow's `image:` lines).
* **How the workflow starts it.** In the `control-plane` job, before the tests: `docker build --pull --no-cache -t zbstub:ci test/zbstub`, `docker run -d … -p 127.0.0.1:18081:8080`, a readiness loop on `/healthz`. `TM_TEST_ZEROBOUNCE_URL=http://127.0.0.1:18081` is given to both the `go test -v ./...` step and the test-execution gate.
* **Missing fixture = failure, not a skip.** `internal/zbtest.URL` fails the test in CI (`GITHUB_ACTIONS` / `CI`) and skips only outside it, as the database tests' gate does. A separate CI step runs a stand-in test with the variable removed and requires it to **fail naming `TM_TEST_ZEROBOUNCE_URL`**.
* **Running it locally.** `ZBSTUB_ADDR=127.0.0.1:18081 go run ./test/zbstub` in `control-plane/`, then `TM_TEST_ZEROBOUNCE_URL=http://127.0.0.1:18081 go test ./...`.
