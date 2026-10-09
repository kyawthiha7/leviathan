# Changelog

Notable changes to this project are recorded in this file; the format references [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/).

## [Unreleased]

### Interception

#### Added

- **New built-in interception rule "delete-style API paths"**: The previously built-in HTTP destructive rule only recognized the DELETE **method** (`curl -X DELETE`, `requests.delete(`, `method:'DELETE'`), and the wordlist for path-style rules only had `/clear /wipe /flush /purge /truncate /drop /destroy /factory-reset /reset-all` — yet most applications' delete endpoints can be triggered with GET/POST, so calls like `curl 'http://t/api/user/delete?id=1'` matched no built-in rule and would really delete the target's data. A `deny` rule has now been added to cover `/delete /del /remove /unlink /erase /destroy` (allowing suffix forms like `/deleteAll`, `/delete_user`, `/delete-user`); the verb must be followed by a separator, so `/delivery`, `/details`, `/delta`, `/delegate` are not falsely intercepted. The rule uses a separate seed marker bit and **existing instances also get it after upgrade**; like the other built-in rules, it can be disabled or deleted in "System → Command Interception".
### Vulnerability Notifications

#### Added

- **Finding discoveries can be pushed to IM**: A new "System → Notification Push" page can push scan findings to six channel types: **DingTalk, Feishu, WeCom, generic Webhook, Telegram, and Email**. Any number of bot instances can be configured per type (e.g. one DingTalk bot each for an "incident response group" and a "daily bulletin group"), each with independent start/stop, rate limiting, and filter rules.
- **Push timing has real-time and digest modes**: In real-time mode a hit is sent immediately, one by one; in digest mode a batch of findings is merged into a single message per global period (default 30 minutes), headed by "M new findings in the last N minutes" and the severity distribution, so it is easy to judge at a glance whether immediate action is needed. To do "high severity real-time, the rest digest", create two channels configured separately; the policy is not hard-coded.
- **Filter rules support four dimensions**: minimum severity, task/asset scope, vulnerability-type keyword include and exclude (exclude wins), and whether to receive disposition-status changes (off by default, because most people mean "push" to be about new findings, not a status ledger). A single message carries a "View Details" button that jumps to the finding details; the target address is configured by the global "callback URL", and when left empty no button is included.
- **Delivery history and manual resend**: The notification page lists each delivery's status, attempt count, failure reason, and channel, filterable by channel and status; failed items can be resent with one click (resending resets the retry count, because manually clicking resend means the failure reason has been handled).
- **Channel credential masked echo**: Webhook URLs, signing secrets, Bot Tokens, SMTP passwords, etc. are declared by each channel (`SecretKeys()`), and the API only echoes a masked value with a tail hint; submitting it back unchanged means "no change", while clearing the input deletes the field.

#### Fixed

> This section records the conclusion of a round of security audit after the feature's first version was completed. Each item was first reproduced, then fixed, then covered by a regression test.

- **Fixed masking being bypassable by "change the target address, keep the credential" (critical)**: The masking mechanism's purpose is "credentials are not echoed back to the browser", but the target address and the credentials are two independent sets of fields, and config merging always keeps the existing DB value for "unmentioned keys". So **changing only the address while not mentioning the credential** let the server send the real DB credential to any address — the generic Webhook's `Authorization` header, Telegram's Bot Token (which goes into the request path), and the email channel's SMTP password (handed to the peer after STARTTLS) were all leaked, completely silently and without relying on a redirect. This path was verified to be feasible; all four channels were reproduced one by one. It is now specified: **whenever the target address changes, the caller must explicitly state a value for every credential field** (give a new value, or explicitly leave it empty to indicate it is no longer needed) — echoing the masked value unchanged means "reuse the old credential", which is exactly the attack shape, and is rejected as well. Deliberately not doing "automatically drop credentials", because for optional credential fields (such as `headers`) that would become "authentication silently gone but the API returned success", which is harder to troubleshoot than an error.
- **Fixed markdown-family channels doing zero escaping of untrusted content**: The DingTalk, WeCom, and Feishu channels previously did no escaping at all (Telegram and email both did). A finding's title and summary come from model output (the model reads the target's response), and an asset's `url` is the full scanned URL containing a target-controllable query string. A finding whose title is `Login SQL Injection\n[Urgent: verify account here](http://attacker.tld)` renders as a **clickable external link** in a security engineer's DingTalk/Feishu; `![](http://attacker.tld/beacon)` is fetched by the client when rendered — equivalent to announcing "this finding has been viewed" and leaking the reader's IP. Now single-lining and markdown metacharacter escaping are applied uniformly. At the same time a related design mistake was corrected: escaping must happen at each rendering exit, and must not be put into the title function shared by four contexts (markdown escaping leaking into Telegram's HTML would leave visible backslashes).
- **Fixed the SSRF surface of the delivery address**: Previously only the scheme and host were validated, so `169.254.169.254` (cloud metadata, from which instance credentials can be read), loopback addresses, and internal addresses were all deliverable; and on delivery failure the first 200 bytes of the response body went into `last_error` and were echoed by the delivery-history API, forming a semi-blind read primitive (able to read the first 200 bytes of any internal HTTP endpoint's response). Defense is now set at the **dial stage** (rather than only validating when saving config) — that is the point where it finally takes effect — also covering DNS rebinding and same-host redirects, and rejecting cross-host redirects (these vendors' credentials are in the URL, so following a redirect means handing them to the redirect target). Loopback and link-local addresses require explicitly setting `LEVIATHAN_NOTIFY_ALLOW_LOCAL=1` to be allowed (a local SMTP relay is a legitimate configuration and cannot be blanket-blocked); **RFC1918 private networks are deliberately allowed**, because self-hosted Mattermost / SMTP relays on the internal network are common, and defending to that degree would disable normal deployments too.
- **Fixed push credentials leaking through error messages**: On delivery failure, the `*url.Error` returned by `http.Client.Do` puts the **full URL** into the error text, and DingTalk's `access_token`, WeCom's `key`, Feishu's hook id, and Telegram's `/bot<token>/` are **all in the URL**. That string therefore flowed to four exits: `notification_deliveries.last_error` stored in plaintext, the delivery-history API's unchanged echo (bypassing the channel config's masking), server logs, and the error shown to the frontend by the test-send API. Now sanitized uniformly — the error message keeps only `scheme://host` and the underlying reason (enough to locate DNS/connectivity/certificate problems), discarding path and query. The address-validation's own error text (`url.Parse` failure) likewise carried the full address and is handled the same; **the previous round fixed only the former and missed the latter, and all regression cases at the time went through the scheme branch and never covered the parse-failure path — a false guarantee** — cases truly covering that branch have now been added.
- **Fixed silent loss when a digest message was truncated but the whole batch was marked delivered**: Channels all have length limits (WeCom's 4096 bytes is the tightest); when a batch does not fit, the message is truncated, yet after a successful send the whole batch of deliveries was marked delivered — the truncated ones are **neither in the message nor in the failure list**, and the delivery history still shows success, so the findings simply vanish. It now packs by **whole items**: only those included in this message are marked delivered, and the rest return to the queue to continue in the next message, and the message header truthfully states "this message shows the first N items; the remaining M will continue in the next message". Deferred items **do not consume a retry** (the optimistic +1 at claim time is subtracted back), otherwise a backlog of 500 would declare the tail items failed by the third segment — though they never went wrong at all.
- **Fixed a typo in the minimum severity threshold making the filter silently ineffective**: When `min_severity` was mistyped as something like `hgih`, an unknown severity has ordinal 0 in the ranking table, so the check degrades to `rank >= 0`, always true — the user thought they had limited it to "high severity only" but actually flooded the group with all findings, and it was completely indistinguishable from a correct configuration in the UI. The value is now validated on the write path, with the error listing the allowed values (the read path remains lenient: bad existing values in the DB won't make the whole channel unreadable).
- **Fixed `rate_per_min = 0` (unlimited) being unreachable**: The documentation, UI hints, and token bucket all interpret 0 as "unlimited", but the DB-write layer alone wrote `if RatePerMin <= 0 { use default }`, silently turning an explicit 0 into 20 (DingTalk/WeCom/Telegram) or 100 (Feishu) — the operator thought they had lifted the rate limit but was actually throttled with no hint. The distinction between "unspecified" and "explicit 0" can only be expressed by the request body, so the default was moved to the API layer to fill in when the field is absent.
- **Fixed the digest channel bypassing the token bucket entirely**: `allow` was deducted by `takeTokens` but nobody used it, so `rate_per_min` had no effect in digest mode. Now the claimed count is constrained by both "this tick's quota" and an in-memory upper bound.
- **Fixed failure handling judging by the batch's maximum attempt count, dragging old deliveries down with new ones**: Attempt counts within a batch are not the same; an old delivery that has already retried twice would drag brand-new deliveries in the same batch into failed — a new finding was permanently lost without a single retry, exactly the opposite of the intent "don't let old rows sink new rows". It now decides per item: permanent failures are killed immediately, those that have exhausted their own retry counts are killed, and the rest are rescheduled according to their own backoff tier.
- **Fixed deliveries whose snapshot cannot be parsed within a digest batch being silently marked successful**: Such items were skipped by the rendering stage (without sinking the batch) but were then counted as successful when the whole batch was marked delivered. They are now explicitly marked failed with a reason, findable in the delivery history.
- **Fixed a single channel's per-round delivery count possibly exceeding the lease duration**: The lease is 3 minutes, and if a round could serially deliver enough items that the worst-case time exceeds the lease, then in a multi-instance deployment the peer would re-claim and resend rows whose lease had expired, duplicating sends and double-incrementing attempt counts. The per-round cap is now derived backward from "lease / single timeout", and an assertion pins down the relationship among these three constants (writing this assertion immediately revealed that the original value 6 used the lease exactly to the full with zero margin, and it was adjusted to 5).
- **Fixed Telegram truncation possibly cutting an HTML entity**: Truncation avoided half tags but not entity fragments like a cut `&amp`, and the parser could therefore reject the **entire** message — oversized digest messages are common, so the cost was too high. It now avoids both unclosed tags and entity fragments.
- **Fixed the email channel treating temporary SMTP failures as permanent**: SMTP 4xx (such as greylisting `450`) is a temporary rejection and the proper approach is to retry later, but it was always treated as permanent failure — an email server with greylisting enabled would make **every** push fall into failure after its first attempt, which is exactly the scenario where automatic retry should matter most. It now distinguishes by the first digit of the reply code: 4xx is retryable, 5xx is permanent failure, and when no code can be obtained it is treated as retryable.
- **Fixed nested-structure submission actually writing the masked literal into the DB**: Object fields like `webhook.headers` can only be masked or submitted as a whole; stuffing a mask sentinel inside the object can neither express "keep unchanged" nor avoid being stored as a real value, causing subsequent authentication to fail silently with no error at all. Such submissions are now explicitly rejected.

#### Design Notes

- **The finding-write transaction does only one blind INSERT**: `notification_events` is written by `RecordFindingTx` in the **same transaction**, so committing guarantees that "the finding is persisted" and "the push task exists" are atomically consistent. This INSERT deliberately does not read the channel table or run the user's filter rules — otherwise one misconfigured filter could pollute or even abort the transaction, preventing a high-severity finding from being stored. To isolate failure to this one statement (in PostgreSQL a statement error within a transaction invalidates the whole transaction, and even `COMMIT` fails), it is wrapped in a `SAVEPOINT`, so on failure it only logs and does not affect the finding write.
- **Delivery claims with a lease rather than a long transaction**: After claiming with `FOR UPDATE SKIP LOCKED`, the row is set to `sending` and `next_attempt_at` is pushed into the future as the lease; the network delivery happens after committing the transaction, so no DB lock is held during delivery. `sending` rows left by a crashed process are re-claimed by the next round after the lease expires, self-healing without causing infinite retries.
- **Rate limiting does not consume the retry budget**: The engine first computes how many items a channel's token bucket can still send this round, then claims exactly that many. Reversing the order (claim first, drop later) would waste an attempt count on deliveries blocked by rate limiting; three attempts' budget consumed purely by waiting would then fall into failure. Exceeding the limit does not drop messages, it only defers delivery to the next tick.

### Test Infrastructure

#### Fixed

- **Fixed permanent asset residue caused by the cleanup order of the company ICP attribution test**: That test registered cleanup in `t.Cleanup`, but closed the connection with `defer d.Close()` — `defer` runs first on function return and `t.Cleanup` after it, so all cleanup statements hit **an already-closed connection**, and the errors were discarded by `_, _ =`, leaving the test asset and company permanently in the DB. It used `MAX(companies.id)+1` as a fake TaskID to tag the asset; once that number collided with another test's task id, the test asserting "exactly N assets" would fail inexplicably and be extremely hard to locate. Closing the connection now also goes through `t.Cleanup` and is registered first (LIFO guarantees cleanup runs first), and cleanup failures are surfaced rather than swallowed. The same pattern (`defer d.Close()` + DB writes in `t.Cleanup`) appears in a dozen other places in the `db` package; this change fixed only the one proven to trigger.

### Traffic

#### Added

- **Traffic list gains "Clear All"**: Deletes all traffic records at once, ignoring the current filter, and additionally cleans up historical host directories no longer recorded by the index, leaving no residue. After clearing it also runs a full compaction (`optimize` + `VACUUM` + `wal_checkpoint(TRUNCATE)`), returning the disk space used by the index to the system, and then reports how much was actually freed. Traffic evidence already bound to findings is stored in a separate evidence store and is unaffected. `VACUUM` on an empty DB costs almost nothing, so this is also **the way for existing instances to convert the index DB to incremental-reclaim mode** — after clearing once, daily per-host deletion reclaims space on its own.

#### Fixed

- **Fixed disk space not being freed after deleting traffic**: Deletion only makes data invisible; the space stays in the index file. SQLite's row delete merely hangs pages on the freelist, and the index DB was created without `auto_vacuum`, so the file never shrinks; meanwhile `ex_fts` is a `contentless_delete` full-text index, and `DELETE` only writes tombstones without reclaiming the original postings, so without merging they accumulate forever — deleting traffic actually makes the index bigger. Since bodies under 256KB are all inlined in this DB, plus a trigram index of roughly twice the body size, a capture-heavy instance would hold several times its disk for long-deleted traffic (measured: 6MB of bodies captured → 16MB index, still 16MB after deleting everything). New index DBs now enable `auto_vacuum=incremental`, and after every delete commit a background chunked process runs "full-text incremental merge + `incremental_vacuum` + `wal_checkpoint(TRUNCATE)`", gradually returning space to the OS (in the same scenario it drops to 104KB after deletion). Reclaim runs in chunks and releases the write lock between chunks, without blocking traffic recording; on process exit it yields immediately and the remaining work resumes on the next delete.

> Upgrade note: `auto_vacuum` can only be set at DB creation, so **an existing instance's index DB is still in the old mode**, and `incremental_vacuum` is a no-op on it — a hint line is printed at startup. The continuous tombstone growth in such a DB has already stopped after upgrade (the full-text merge runs as usual), and the already-occupied size can be reclaimed once with the traffic list's "Clear All" — that step converts the DB to incremental-reclaim mode, after which daily deletion takes effect on its own.

### LLM

#### Fixed

- **Fixed custom session headers not taking effect on the "one-shot LLM call" path**: The session header's value comes from the session id on the request context, and that id is written into the context by agentcore only when a transcript store is attached. Goal decomposition (round 0) and cold-node compaction (the §4 body call) are both one-shot calls without a store, so there is no session id on the context and the gateway side cannot read the header — for endpoints like opencode zen that "return 400 MissingSessionID when `x-opencode-session` is missing", this appeared as "round 0 goal decomposition fails with 400, yet subsequent planner rounds work fine", and a compaction failure left only a single log line and was extremely hard to correlate. These two paths now explicitly attach an exploration-stable session id (`exp<N>-goals` / `exp<N>-compactor`): the header is sent properly, and llmrec can correctly attribute the token usage of these two paths to the corresponding exploration (previously not recorded at all).

### Findings

#### Fixed

- **Fixed the asset list in the "By Asset" view having no scrollbar when it overflows**: The left asset tree was already wrapped in a scroll container, but the outer card only set `max-height` and no definite height, so the scroll viewport could not resolve a height from `height:100%` (in CSS, when only `max-height` is set and `height` remains `auto`, a percentage height has no effect), so with many assets the list either burst the card or was truncated and unscrollable. The height cap is now placed directly on the asset tree's native scroll container (`overflow-y-auto` + `max-h`, adapting to the window height), so with few assets the card shrinks to content and with many it caps and shows a scrollbar.

## [0.3.14] - 2026-09-24

### Task List

#### Added

- **Task list gains a "Findings" column**: Shows the counts of Critical / High / Medium / Low by severity tier, with non-zero tiers colored by severity, so each task's finding scale and distribution are clear at a glance.

#### Changed

- **Narrowed the width of the "Description / Goal" columns**: Overlong content is ellipsized with the full text on hover, reducing long text crowding the list's horizontal space.

### Exploration Graph and Planning Posture Overview

#### Changed

- **Streamlined the planning posture overview (graph_overview) and set count caps for each list**: Recent facts, finished intents, todo intents, cold-zone summaries, and confirmed-finding details all changed to "latest window + count fallback", with undisplayed items queried on demand; this significantly reduces the per-round LLM context size and avoids context bloat on long tasks (the related tasks' overview cold zones are throttled too).

#### Fixed

- **Eliminated the reverse duplicate edges between digest and finding in the exploration graph after folding cold nodes**.

### Contributors

- [@Autumn-27](https://github.com/Autumn-27)

## [0.3.13] - 2026-09-19

### Asset Interception

#### Added

- **New global asset interception rules (blacklist)**: Supports entering exact and fuzzy-matched domains / IPs / URLs and CIDR ranges, with rule CRUD and enable/disable; the management page is at "System → Asset Interception"; fuzzy interception of government (`.gov` / `.gov.cn`) and education (`.edu` / `.edu.cn`) sites is built in by default.
- **Asset interception wired into the execution chain**: Before dispatching intents (`add_intent`) and inserting assets (`insert_assets`), the Agent first makes an interception decision on the target asset — an intercepted intent is not dispatched and an intercepted asset is not inserted, and the asset info and interception reason are returned to the Agent.
- **New task-level interception/allow (whitelist) rules**: Independent of global rules and effective only for this task, with an "intercept before allow" judgment order — a hit on interception forbids it; if interception is not hit but the task has allow rules configured and none of them match, then "testing not allowed"; when no allow rules are configured the whitelist is not enabled. They can be entered when creating a task, and added/edited/enabled/disabled in the task details' "Overview".
- **Task templates support preset categories and task-level intercept/allow rules**: A template can save a task category and a set of task-level rules, which are carried into the new-task form when the template is applied.

### Operation Review

#### Fixed

- **Tightened the model-judge output protocol to reduce false allowances caused by truncation**: The verdict explanation (comment) cap was lowered from 500 Chinese characters to 120 and forced to "output only JSON, with no preamble or code block", to avoid the verdict being truncated by `MaxTokens` and becoming unparseable, thereby being allowed under the model-failure policy (fail-open).

### Task Archiving

#### Fixed

- **Skip symlinks on archive rather than failing the whole archive**: The archive format end-to-end supports only regular files and directories; previously any symlink in the working directory caused the entire task archive to fail; it now skips the symlink and logs it, archiving the rest normally (not following links, not escaping the directory tree).

### Account and Compliance

#### Added

- **New "Terms of Use and Disclaimer" dialog before login**: Agreement must be checked before logging in.

### License and Dependencies

#### Changed

- **The project adopts the AGPL-3.0 open-source license**, and the README's license and disclaimer notes are improved.
- **Upgraded norma to v0.4.1**.

### Contributors

- [@Autumn-27](https://github.com/Autumn-27)

## [0.3.12] - 2026-09-17

### Exploration Chain Broadcast Board

#### Added

- **Task details page gains an "Exploration Chain Broadcast Board"** (#144): presents the task's exploration nodes (start/goal/intent/fact/finding/hint/compaction) from a timeline-stream perspective, supporting type filtering, keyword search, ascending/descending order, paging, and auto-refresh; when on the "newest-first first page" it is the live position and refreshes every round, and when leaving that position it only accumulates an unread count without disturbing current reading, with a "N new broadcasts · back to latest" one-click return. Grouped by day, so even when paging through a long task you can tell "which day this is from".
- **Each broadcast row shows the node id** (#145), making it easy to cross-reference the exploration chain graph and locate a specific node.
- **Broadcast node details expand to show upstream/downstream and anchored assets** (#147): expanding a broadcast shows the node's upstream (came from) / downstream (produced) relations, and hovering a related entry pops up the corresponding node's card (type/status/source/time/summary/payload snippet); it also lists the assets the node anchors (type label + identifiable text). The related data is delivered together with the broadcast page, so expanding issues no extra request.
- **Broadcast search supports filtering by node id** (#150): the search box adds node id matching in addition to "content / source"; entering pure digits or the displayed "#41" form precisely locates the corresponding node.

### Intent Management

#### Added

- **Intent deletion supports two modes, "soft delete / hard delete"** (#149): todo, running, and paused intents can all be deleted, and deletion requires a reason; the deletion mode is chosen in the confirmation dialog.
  - **Soft delete (default)**: The intent is set to "deleted" and the reason is recorded in a separate field, retaining the intent node and all its outputs and lineage.
  - **Hard delete**: Physically removes the intent and the exclusive descendant nodes "supported only by it" (cascading along the output/intent chain to the leaves), avoiding orphaned data; the deleted intent's token accounting is archived and retained under its original date; shared nodes (still referenced by other intents), goals, and the task root fact are always retained, and the hard-delete dialog shows the estimated number of nodes affected by the cascade.

  Both modes notify the planner "this intent was deleted by the user + reason" and it replans accordingly.

### Operation Review

#### Added

- **Approval records support filtering by status and decision source** (#139): approval records can be filtered by approval status and decision source to quickly locate target records.

#### Fixed

- **Pending approval requests load independently of history paging** (#133): pending requests are no longer affected by history-list paging and remain fully visible while browsing history records.

### Agent

#### Changed

- **Worker role description changed to a generic cybersecurity-platform wording** (#138).

#### Fixed

- **Fixed cold-node compaction never triggering**: The per-task planner was wired to the Compactor; cold-node compaction (cold-digest) had never run because it was not wired in, and it is now restored.

### Chat

#### Added

- **Chat supports @ references to multiple record types and scroll paging** (#135): various record types can be @-referenced in chat, and the reference candidates support scroll-paged loading.

#### Fixed

- **Long message bubbles are constrained within the conversation panel** (#137): overlong message bubbles no longer overflow the conversation area.
- **Fixed "missing upload file" when uploading a file before a new conversation is created**: The Composer's file selection snapshots the `FileList` into an array before clearing the input and then calls back; previously the draft state required first asynchronously creating the conversation, and when execution resumed the `FileList` live-bound to the input had already been cleared, causing the upload to lack the file field and the backend to return 400.

### Traffic

#### Fixed

- **The traffic-recording proxy listens only on 127.0.0.1 by default** (#129, #130): avoids exposing an open proxy on other network interfaces under the default configuration.

### Web

#### Fixed

- **Fixed the static-export task list page losing its global header**.
- **Added the missing related-traffic mock to the demo finding details page, fixing the blank screen**.

### Contributors

- [@Autumn-27](https://github.com/Autumn-27)
- [@RuoJi6](https://github.com/RuoJi6)
- [@dingpotian](https://github.com/dingpotian)

## [0.3.11] - 2026-09-15

### Operation Review

#### Added

- **Approval records support source localization** (#125): clicking an approval record's "source" jumps back to the tool execution that triggered that approval — opening the original full conversation, auto-paging to the target position, expanding the command and result in place, and highlighting it centered in the scroll area, keeping surrounding messages readable; auto-correction stops when the user scrolls manually. Across normal chat, task Worker, planner, and main Agent segmented conversations, precise localization is done through the persisted `tool_use_id` and task mapping (a new scoped call-ID index); when missing, duplicated, or ambiguously associated, it explicitly prompts rather than jumping to another execution. When the conversation was deleted or the task was archived/record is missing, a clear prompt is given.
- **Approval records support paging** (#115).

#### Changed

- **Streamlined model-review input** (#124, #125): The model review (the LLM judge after a rule miss) input is converged to a versioned JSON of "this one complete tool call + explicitly selected short background + the local working directory". The background takes only real user messages (the current user message of chat/task main Agent); the Worker no longer carries an intent summary and has the background inherited from a superior Agent cleared; the planner and auto-triggered conversations do not fabricate a user message. Task description, goal, operation constraints, global exploration posture, historical calls, and the Worker's full intent are no longer included — these are still used as before for Agent execution and independent-conversation auditing, just not in operation review. All verdicts require outputting JSON `decision`/`comment`, and the explanation must include "the actual operation, the consequence if successful, the rule hit"; instructions in parameters or background cannot change the review policy. The input snapshot actually sent to the model and its fingerprint are saved; older snapshots are retained with a version label, and current data is not used to fabricate old input.

#### Fixed

- **A model verdict wrapped in a code block is no longer silently allowed** (#126): if the model's returned verdict JSON was wrapped in a ``` code block, parsing previously failed and was silently allowed via the model-failure policy; now the code-block fence is stripped before parsing, and only if parsing still fails is the configured failure policy applied.

### Agent

#### Added

- **New experimental noa context compaction** (norma upgraded to v0.4.0): an experimental feature that can be enabled in system settings, off by default. When enabled, noa (model-driven context compaction) takes over context compaction for the four Agent types — main Agent, planner, Worker, chat — replacing the built-in compaction; the compacted original is persistently archived, collected under `<workDir>/noa/<sessionID>/` (not scattered across task directories), uniquely separated by session ID globally. If wiring fails it automatically falls back to built-in compaction, without interrupting real tasks; the switch is read once per run, and toggling only affects runs started afterward.

#### Fixed

- **Exploration-graph tools reject rather than nil-crash when there is no task context**: calling exploration-graph-related tools outside a task context returns a clear error instead of crashing on a nil dereference.

### MCP

#### Added

- **Support for legacy SSE MCP servers** (#117): compatible with MCP servers that only offer the legacy SSE transport.

### Traffic

#### Fixed

- **Traffic search matches records' hosts in a port-aware way** (#114): `traffic_search`'s host matching now includes the port, avoiding cross-talk between records of the same host on different ports.
- **`traffic_search` description migration errors no longer interrupt subsequent reporter migrations**: a single migration failure is isolated and does not affect subsequent migrations.

### Web

#### Changed

- **The task retest finding selector gains a divider** (#122): a separator is added between retest finding options for clearer visuals.

#### Fixed

- **Fixed the demo task details page crashing entirely**: In mock mode the task details' "Conversations" tab fetches `GET /api/tasks/<id>/side-questions` (side-question history), but the mock handler had no such route, so the read fallback treated it by "a path ending in s is a collection" and returned `[]`, making `data.items` `undefined`, and the side-question hook's `merge()` iterating over it threw `TypeError: t is not iterable`; that exception occurred inside `setItems`'s updater and was re-thrown by React deferred to the render phase, which the caller's `catch` could not catch, taking over the whole page with the error boundary showing "This page couldn't load". The mock handler now explicitly returns empty side history, and `sideAPI.history` also normalizes the return value (any non-array `items` becomes `[]`) as defense in depth.

### Contributors

- [@Autumn-27](https://github.com/Autumn-27)
- [@RuoJi6](https://github.com/RuoJi6)

## [0.3.10] - 2026-09-13

### Network

#### Added

- **Web search gains a DeepSeek official source**: It directly reuses the currently active LLM config. It differs in nature from the other three sources — DeepSeek has no directly callable search API, and search exists only inside its Anthropic-compatible API (the `web_search_20250305` server-side tool), executed by DeepSeek's server. It therefore **supports only the DeepSeek official endpoint + anthropic protocol** (an OpenAI-protocol endpoint directly rejects server-side tools), and each search **consumes an extra model call**, the request **does not go through the search egress proxy**, and **is not counted in traffic capture**; the returned result **has only the title and link** (no summary; use WebFetch to fetch the body when needed). The settings page describes these limitations but **does not validate/block**; whether they are met is for the user to confirm, and a "Test Search" button can run one for real to verify.

### Agent

#### Added

- **Worker gains cross-work retrospective ability**: Adds `search_all_worker_traces` (globally search all of this task's works' execution processes for matching steps by keyword, without needing to know the intent_id first) and `get_worker_trace` (after locking onto a work, list its step stream, search in place by keyword, and fetch full content by step_id), to reuse observations another work has seen but not written into a fact, avoiding duplicated effort.
- **`node_detail` is released to the worker**: together with the retrospective tools above, the worker can directly query a node's full details after obtaining the intent_id / node id.
- **Planning rounds triggered by `add_hint` are explicitly announced**: previously a new hint was only folded into the posture overview and left for the planner to discover; now an `add_hint` records a "human added N strategic hints: …" trigger for the planner (batched as one, not spamming line by line), and the planner is explicitly told "this round was triggered by newly added hints" and sees the hint content directly.

#### Changed

- **Global posture hints changed to a "loosen" tone**: encouraging divergent exploration and timely reporting of cross-intent clues, rather than converging too early.
- **Streamlined worker boundary wording**: being blocked the first time does not mean fully explored; focus on "exhaust the bypass methods within this intent before concluding".
- Removed the per-asset `related` parameter from `insert_assets`: its only purpose was to decide whether to add this asset to the task scope, and the value is not persisted anyway (registering the same asset again invalidates it, and the UI cannot tell who was judged irrelevant); it only made the model do an extra judgment that could not be kept.
- **Test scope (`task_scope`) is no longer affected by the asset-coverage switch**: `insert_assets`'s auto-scoping (`source='auto'`) runs regardless of whether coverage is on, and `add_task_scope` is always provided to the planner / in-task main Agent / goal-decomposition Agent. Scope is the task's authorization boundary and the filter basis for asset queries; the coverage switch only decides whether to use it as a denominator for metrics, not whether to accumulate the scope itself. Previously turning off coverage disabled both the `auto`/`agent` write paths, leaving `task_scope` with only manually added UI rows. `list_untested_assets` is still hidden when coverage is off (it is itself a pure coverage perspective).

#### Fixed

- **Fixed asset cross-contamination between tasks** (#59): `list_assets` previously hard-coded the task id to 0 and queried the whole shared asset library, and the model had no way to write a scope filter, so it treated other tasks' assets (especially IPs) as this task's targets and skewed the testing direction. It now returns only assets within the test scope (`task_scope`) of [this task and its directly related tasks], matched by **attribution** rather than literal value: with a root domain in scope, all its subdomains/services/endpoints are found; with a range, the hosts and services within it are found; directly fetching an out-of-scope asset by id also fails. Non-task context (Auto/pentest) has no scope to rely on and still falls back to the whole library. It also fixes the attribution blind spot where IP-direct hosts (like `http://1.2.3.4/api`) could not be hit by a range scope. The UI's "Test Assets" view (filtered by task producer) is unaffected.
- **Fixed the worker cross-work retrospective tools being wrongly removed**: After `search_all_worker_traces` / `get_worker_trace` (along with `node_detail`) were added to the worker's default toolset, an old migration "converging the worker's tool surface" unbound them again at startup, so the worker actually could not get these tools. They have been removed from that unbind list, and for DBs that already ran the old migration they are re-bound to the worker once.
- **Fixed occasional persist failures of `/btw` side questions**: Before the side_question checkpoint is persisted, JSONB-unsupported NUL (`\u0000`) escapes are stripped, avoiding a write error for content containing that character.

### Triggers

#### Fixed

- **Merged trigger sessions deduplicate task descriptions/goals by task**: When multiple tasks merge-trigger, the same task's description/goal is no longer repeatedly concatenated into the message, avoiding long goals piling up and blowing up the context.

### Contributors

- [@Autumn-27](https://github.com/Autumn-27)

## [0.3.9] - 2026-09-11

### Agent

#### Added

- The in-task main Agent supports **multiple sessions**: create, switch, and independently reset context, with each session interactive.
- Supports persistable `/btw` **side questions**: ask follow-ups without interrupting the main thread; the Q&A is persisted and remains after restart.
- `spawn_task` gains `source_task_ids`, so a new task can **read-only inherit** the source task's assets and findings.

#### Changed

- Disabled the worker's cross-engagement memory and converged its default tool surface: reading context and cross-work review are the planner's job, and the worker only handles a single intent's execution and write-back.
- Streamlined the worker's default prompt; removed the `terminated` and `worker_name` fields from `get_worker_output`; streamlined the descriptions of `insert_assets` / `list_assets`.

#### Fixed

- Fixed works being interrupted early due to **idle rounds**: the model sometimes uses a whole round to output only thinking, with neither text nor tool calls, and the harness sees this as a natural end (`end_turn` with no `tool_use`), ending directly with `completed` + an empty summary — an unfinished intent breaks off halfway, appearing as "the task ends normally but with no textual summary". None of the five layers of LLM retry can reach it: it is not an error, the same-provider safe-window retry entry point is "stream failure", the circuit breaker directly judges success when `err == nil`, and intent rerun only recognizes `model_error`; the SDK's empty-response retry also cannot reach it, because that layer judges emptiness by "whether any event was yielded", and the thinking delta is itself an event. The work's Stop hook now recognizes such a round and injects a continue instruction, letting the model carry the thinking it has already produced into the next step — deliberately not choosing "resend as-is", because this kind of idle round is usually determined by the prompt and context shape, a stable behavior rather than a random flake, and resending would only make the model think again.
- Reuses the LLM page's "Retry and Backoff" **empty-response retry count** (both answer "the model finished but produced no substantive content", just with different criteria and means), default 2, set `-1` to turn it off and fall back to the original "idle means end" behavior. It is an intent's **total** cap rather than a consecutive count: the harness itself already limits consecutive idle rounds to one retry, and the quota only refreshes when a tool round actually occurred, so this number guards against loops like "tool → idle → push → tool → idle" exhausting the intent budget. On trigger the log records `[work <worker> · #<intent>] idle round…injecting continue instruction (n/N)`. See `docs/llm-retry-design.md` §1.1.
- Fixed the `/btw` context budget and input layout under long conversations; non-secure contexts (non-HTTPS access) degrade to generating a side-request ID.

### Traffic

#### Added

- Findings support **linking multiple pieces of traffic evidence**, which can be ordered, annotated, and distinguished by role (request/response/corroboration).
- The report Agent **automatically links** related traffic evidence before writing the report.
- New Agent **traffic-binding switch**, and completed evidence hand-off between Agents.

### Tasks

#### Added

- Supports **session-level finding retests**: retests run in an independent Agent session, and the list and details show the running status.

### Interception

#### Added

- Approval records gain **details and execution audit**: can view the tool request context, the model/rule initial judgment, execution output, and the parameter fingerprint.

### Assets

#### Added

- Task test assets support **DSL search**.

### LLM

#### Fixed

- Fixed the connection test not resending the custom session header, causing opencode zen to return 400.

### Network

#### Added

- MCP's HTTP transport supports **skipping TLS certificate verification**, easing integration with services using self-signed certificates.

### UI

#### Added

- The conversation list is grouped by Agent, supporting expand/collapse and independent pinning; conversations support filtering by Agent.
- The attack-chain graph renders compaction (digest) nodes and collapses their members.

#### Fixed

- Fixed the blank screen caused by out-of-sync login credentials.
- The interception prompt wording now clearly labels "platform governance", avoiding misreading it as target-side defense.

### Deployment and Updates

#### Added

- Supports **one-click page updates**: A "Version and Update" card is added to the system config page, and the top bar shows a hint when a new version is available. The flow is: download the release package → verify `SHA256SUMS` → smoke test → stage → exit and let the supervisor script re-launch to complete the swap; the page auto-refreshes.
- New supervisor startup scripts `start.sh` / `start.bat` as the official startup entry point (included in both the release package and the Docker image; `install.sh` is unchanged); they decide whether to re-launch based on the exit code and forward SIGTERM to leviathan. The verification and swap logic is all in Go, keeping the scripts foolproof.
- Automatic fallback on failure: if verification or smoke test fails, discard and keep running the current version; if a new version fails to start 3 times in a row, roll back to the previous version. There is also manual rollback in the settings page (note that the database schema is not rolled back).
- Updates only recognize the GitHub domain and force HTTPS; the release source is not configurable; one-click update is disabled for development builds. GitHub query results are cached for 30 minutes to avoid the top-bar hint exhausting the API quota.

#### Known Limitations

- Under Docker only the program is swapped, not the image: the toolchain is not upgraded along with it, and rebuilding the container reverts to the image's bundled version; use `docker compose pull leviathan` when needed.
- The `skills/` in the release package is not synced, so newly added built-in skills do not take effect automatically.
- Updating restarts the service, interrupting running tasks.

### Dependencies

#### Changed

- Upgraded norma to v0.3.7.

### Contributors

- [@Autumn-27](https://github.com/Autumn-27)
- [@RuoJi6](https://github.com/RuoJi6)

## [0.3.8] - 2026-09-09

### LLM

#### Added

- The LLM page gains a "Retry and Backoff" tab: the **count** and **interval** of the five retry layers can all be configured. A model call's failure passes from inside to outside through five layers: connection retry (SDK, connection reset / timeout / 429 / 5xx before the stream starts), empty-response retry (SDK, ended normally with no content, openai format only), same-provider safe-window retry (replaying a broken stream before anything was delivered to the caller), round-robin circuit breaking (cool down and skip after consecutive failures reach the threshold), and intent rerun (rerunning the whole intent after the worker ends with `model_error`); an outer layer is only reached when the inner one is exhausted. Each layer has two knobs with unified semantics: leave blank = use the original default count and exponential backoff; enter a count to use that count; enter an interval to replace exponential backoff with a fixed interval; enter -1 to disable that retry layer. The first three layers follow the endpoint and can override the global default field-by-field in each model config (those pinning only the interval still inherit the global count); circuit breaking and intent rerun are process-level semantics with only one global copy. Saves take effect hot without a restart. Leaving everything blank is the current behavior, and an upgraded old DB is byte-for-byte unchanged (new columns default to 0, and a missing settings key means all defaults). The connection test deliberately does not carry these parameters — it has a hard 30s timeout, and stacking the user's retries on it would only test a usable endpoint into a timeout failure. See `docs/llm-retry-design.md`.
- Each LLM config supports a custom **session header** (`session_header_key`): when non-empty, every LLM request carries that HTTP header, with the value being the current run's session id (chat sessions are `conv-<id>`, workers are `exp<x>-worker-i<intent>`, etc.), for gateways that do prompt caching / sticky routing by session-id header. The implementation reads the session id from the request context and injects it, without changing norma, and the same shared provider can also emit different header values per session. Old DBs have a migration, and the frontend LLM config dialog gains an input box.

#### Fixed

- Fixed `23502` being reported directly when saving an LLM config because the session-header field is empty: that column is `NOT NULL DEFAULT ''`, and previously `NULLIF($n,'')` was mistakenly applied, writing an unfilled session header as NULL and triggering the non-null constraint; it now passes the parameter with empty-string semantics.

### Agent

#### Added

- Wall-clock timeout changed to **in-place wrap-up**: relying on norma v0.3.6, `MaxDuration` on expiry interrupts the running tool and continues on the live ctx for the wrap-up rounds (writing back recognized content + a summary, terminal state timeout), no longer relying on worker/planner's respective external `maxDur+90s` hard ctx to kill a stuck run directly into `aborted_tools`. Chat goes through the same harness and automatically gets the same behavior; mainagent has no `MaxDuration` and is unaffected.
- Stagnation fallback: when the planner wakes on a heartbeat / no-change and the whole graph has no open or running intents, the opening message changes to a stagnation alert, clearly stating that no worker is running and no direction is queued, and that this round must produce one or more mutually distinct new intents (0 intents is not allowed).

#### Changed

- Worker prompt refactor: the raw JSON of intent / startup instruction / anchored asset is moved into the system prompt, re-assembled every round and never crushed by compaction, and continuation no longer depends on the transcript's first message being retained; the startup user message is slimmed down to only the global posture overview (degradable, tolerating stale). The cost is that system mixes in per-intent data and loses cross-intent cache reuse — a deliberate tradeoff for "not losing the intent".
- Streamlined the planner / worker default text and fixed several real-world problems: the planner distinguishes `recent_done` by status (blocked/exhausted check the trace first before deciding, neither treating them as a dead end nor blindly rerunning), negative conclusions changed to "observation / doubt, not a verdict" and evidence is checked before believing them, an unmet goal with no open/running intents must produce one (hard bottom line), and depth takes priority over coverage; the worker writes only "observation + tentative reading" for negative-type conclusions, leaving the verdict to the planner, and writes cross-intent clues into the fact's summary for the planner without chasing them itself. Reseed appends the new default as a new version and switches to it, with user customizations / old versions retained in history and rollback-able.
- Converged the work agent's default toolset and asset write-back hints: the worker is only responsible for a single intent's execution and write-back, while reading context / cross-work review is the planner's job, so `list_facts` / `node_detail` / `list_companies` and cross-work retrieval (`search_all_worker_traces` / `list_worker_traces` / `get_worker_trace`) are removed from its default tools, keeping only `list_findings` (dedup check before reporting a finding) + `add_finding` / `record_fact` + `insert_assets` / `list_assets`; the prompt removes stale field descriptions conflicting with the `insert_assets` schema (`type=tech`/`on_url`/`props`). Old DBs have a one-time migration stripping the corresponding bindings; the planner/main bindings of the same names are untouched.

### Exploration Graph

#### Added

- **Exploration-graph cold-node compaction (cold-digest)**: Folds old and long-inactive intents/facts into digest nodes shown in `graph_overview`, with the original nodes permanently retained and fully restorable by id (lossless storage, presentation-only compression, reversible folding). Hot/cold determination is by reverse reachability + any live branch makes it hot, with R=6 rounds of debounce and connected-component grouping; background compaction (minor folds uncovered cold blocks, major re-sources and re-compresses merged fragments) has activity re-checks and cooldown mutual exclusion, never touching the hot path and never overwriting a revived node. The overview side provides `cold_digests` and an asset-indexed `cold_index`, with `expand_digest` / `expand_index` handling restoration. Related/inherited tasks' overviews likewise reuse their own folded view, and `expand_digest` supports cross-task read-only restoration. `expand_digest` / `expand_index` are given only to the planner and main agent, not the worker. All come with old-DB migrations.
- `graph_overview` fully outputs `finding_list`: findings are a task's highest-value product and a single task usually has few, so they are carried in full in the overview (unlike facts, which only give a recent window), letting the planner/worker see all confirmed findings at a glance every round without calling `list_findings` again. Each item is streamlined to `{id, summary, evidence?, from_intent?, assets?}`, where the affected assets are given directly as readable content (url / domain / ip:port) rather than bare ids.

#### Changed

- `graph_overview` no longer flattens `hosts`: the coverage block drops the host list (a large-scope task would carry up to 500 host strings per round, of limited value to planning decisions), keeping only `host_count`, and specific hosts are queried on demand with `list_assets`. A new top-level `done_intents_total` (total finished intents) is added, parallel to `recent_done_intents` truncated to ≤15, letting the planner know whether anything was truncated when deduplicating.

### Tools

#### Changed

- `add_company_scope`'s default binding changed from worker to planner: defining enterprise asset scope is the job of planning / main control / Auto, and the worker only executes exploration. New DBs seed the default binding as mainagent/planner/auto, and old DBs have a one-time migration.
- The planner is now bound to `list_assets` by default: it now has both `list_assets` (DSL full-library search) and `list_untested_assets` (untested within scope); old DBs are backfilled once, without overriding a user's unbinding.

### Tasks

#### Added

- The task list gains a "Running Workers" column: counts the number of intent nodes with `state='running'` under the task, with exactly the same definition as the task details overview's "Running Workers"; shows 0 when no worker is running.

### Network

#### Added

- New **global egress proxy** config: all target traffic can egress through a unified global proxy (http/https/socks5, supporting `user:pass`). When traffic capture is on, it acts as the upstream of the MITM recording proxy (traffic is still recorded and then egresses through the proxy, with both the interception and passthrough paths going through the upstream and not leaking the source IP); when capture is off, it is directly injected into the agent's bash environment and WebFetch (`proxyEnv` gains `ALL_PROXY` support for socks5). The config is stored in the settings KV table (no migration needed), and the frontend system config page gains a "Global Proxy" card, independent of the web-search proxy and the LLM proxy.

### UI

#### Fixed

- Corrected semantic errors in intent status labels: `exhausted` "Fully exhausted" → "Budget exhausted" (it actually means the step/time budget was cut off mid-way with only partial results written back, not that the direction has been fully explored), `blocked` "Blocked" → "Execution error" (it actually means model / API / network failures exhausted retries and the intent was essentially not explored, not a target/WAF interception), and added the previously missing `stopped` "Stopped" (work stopped manually by the user).

### Dependencies

#### Changed

- Upgraded norma to v0.3.4: MCP tool output gains truncation and spilling (see `651b961`), and the subsequent v0.3.6 supports wall-clock in-place wrap-up.

### Contributors

- [@Autumn-27](https://github.com/Autumn-27)

## [0.3.7] - 2026-08-31

### LLM

#### Added

- Model config gains an "Output Limit", and the request field name to use for the limit can be chosen: the limit caps how many tokens a single reply generates at most, sent with each request; 0 (default) = do not send the field, letting the server default decide. It is a different thing from "context window" — the latter is the model's total capacity, used locally only to compute the compaction threshold, not present in requests. The field-name switch is meaningful only for the `openai` (Chat Completions) format: empty (default) sends `max_tokens`, which almost all compatible gateways accept; OpenAI's official reasoning models (o series / GPT-5) conversely accept only `max_completion_tokens` and directly report `unsupported_parameter` on receiving `max_tokens`, so such endpoints must be manually switched to the new field. The two formats each pin their field name (Anthropic is `max_tokens`, the Responses API is `max_output_tokens`), so for non-openai formats the switch is greyed out and cleared on save. It also completes a link that was never connected: the planner / worker / chat / main Agent / goal decomposition five places never set an output limit, so openai-family endpoints simply did not send one and Anthropic fell back to the SDK's 8192; merely adding the config item without wiring this line would mean entering a number had no effect. The value is now resolved per round like the "streaming output" setting, taking effect from the next round after a failover switch. Behavior is completely unchanged after upgrading an old DB (new column defaults to 0 and empty field name).
- When clicking "Test" in the backend, the status code of each HTTP attempt and the gateway's raw response body are written to the server log (response body truncated to 4K), easing diagnosis of 401, quota messages, empty frames, returned HTML pages, etc., no longer limited to the UI's collapsed ok / err. This logging does not depend on the "LLM Records" switch.

#### Changed

- The task LLM config chain can now be modified in any state, no longer limited to "running / paused / chain exhausted": after a task ends (done / failed / timeout) the main Agent conversation still goes through this chain, and when a chain model had a problem it previously could neither be changed nor allow continued interaction. The terminal-state interceptions in both the backend HTTP and the DB transaction are removed, and the frontend dialog opens editing and saving for terminal-state tasks too. Saving on a terminal-state task no longer reopens quota-blocked intents (those intents would be moved to open, which has neither a worker to execute them nor meets the "rerun intent" condition); to keep running, still use rerun intent / add goal, which pull the task back to running.

### Agent

#### Changed

- "Sending a message to a running Worker to adjust direction" changed to reuse the existing pause / resume + transcript resume mechanism, behaving the same as main Agent conversation: it previously used a self-built intervention persistence protocol, involving the scheduling barrier, the resume flow, and a dozen activity-query filters. Now the message is injected into the next round's input, and the intent runs directly in a dedicated goroutine, not subject to the 3-slot worker pool, running as soon as sent; the frontend keeps the message box and restores the "continue directly" button, and sending is carried by SSE. No DB schema changes. Tradeoff: messages are kept only in memory with no crash recovery, and sending a message can transiently exceed concurrency by one worker for that task (a low-frequency scenario, acceptable).

### Tasks

#### Fixed

- Fixed out-of-memory (OOM) crashes when archiving large tasks: archiving now writes snapshots in a streaming way instead of reading the whole task into memory at once; it also completes three recovery gaps on the cold-archive path — traffic for modern installations exists only in SQLite, and previously a crash between the PostgreSQL commit and the SQLite commit would strand an archived task's traffic in hot storage with no way to recover it; now a staging journal is written whether or not there is a historical directory, and a missing `journal.json` is treated as discardable.
- Fixed system lag when there are many tasks: loading conversations and the UI had noticeable delays. The task list, task context, and exploration-record queries are optimized together, and the frontend dashboard, conversation page, and task details page reduce duplicate requests accordingly.

### Assets

#### Changed

- Removed the "at most 256 rules" limit on enterprise asset scope: enterprises entering scope IP-by-IP / domain-by-domain easily hit this cap, and after hitting it they could only be split into multiple enterprises, while the scope itself is not slower for having more entries. The cap is removed in the frontend, backend, and demo mock alike; a single rule is still limited to 1024 characters, and the request body's 2 MiB limit is retained as a fallback (roughly forty to fifty thousand rules).

### Skills

#### Fixed

- Fixed "Upload failed: zip: unsupported compression" when uploading a skill archive: the Go standard library only has Store / Deflate decompressors built in, so bzip2 and Zstandard archives written by compression software at non-default settings could never be unpacked. These two decompressors are now added (pure-Go implementations, no new external dependencies); for formats that genuinely cannot be unpacked (Deflate64 / LZMA / XZ / PPMd) and encrypted archives, a clear message is reported before extraction, naming exactly which file used which compression method and how to repackage, rather than dumping the low-level English error at the user.
- Fixed skill filename validation rejecting Chinese filenames as illegal: path validation was previously an ASCII whitelist of `[A-Za-z0-9-_./]`, so a single Chinese-named file in the archive (like `参考/说明.md`) made the whole upload fail with "archive contains illegal path". It is changed to a Unicode blacklist: filenames in various languages and spaces are allowed, while control characters, invalid UTF-8, zero-width and bidirectional control characters (RLO filename spoofing), `\ % # ? * : " < > |`, and `..` / absolute paths / empty path segments are still rejected, with directory-traversal prevention unchanged. Skill names are likewise opened up — ASCII is still limited to lowercase letters, digits, and hyphens (the agentskills.io spec), while Chinese and other non-ASCII letters can be used directly as skill names, but spaces, dots, and path separators are not accepted.
- Fixed garbled filenames or whole-archive rejection after extracting Chinese archives produced by Windows compression software: such zips do not set the UTF-8 flag and write filenames in GBK; they are now decoded with a GBK fallback before path validation. Also `name: "中文技能"`-style quoted frontmatter now correctly retrieves the skill name.

### UI

#### Added

- The Findings page gains an "All Findings" flat view and sets it as default, switchable via a header Tab with the original "Group by Task" view: the grouped view requires expanding task by task to see findings, which is a detour when you just want to scan a cross-task list. The flat view is one large cross-task table (10/20/50/100 per page), behaving exactly like the grouped view — checkbox export, inline expansion to see evidence and the detailed report, inline editing of name/category/severity, changing disposition status, drill-down, delete. The two views share the stats cards and filter bar, switching does not lose filters, and the current view and filter items are both remembered locally; polling only hits the current view, and inline edits sync to both caches so switching never shows stale data.
- The Findings page gains a "By Asset" view: on the left an asset tree (enterprise → root domain / IP → subdomain → service → endpoint, with the enterprise level appearing only when assets actually have an attribution), and on the right the findings under the entire subtree of the selected node, sharing the same table and the same filter conditions as the other two views. The tree only includes assets that have findings, and the ancestor chain is filled in on demand (when findings hang only on the deepest endpoint, the upper levels are still assembled), with node counts aggregated over the subtree and deduplicated by finding; findings whose asset was deleted or that never had a related asset go into the "Unrelated Assets" bucket. Unlike the other two views, the asset view does not poll — it queries when entering the view, changing filters, adding/editing/deleting findings within the page, or clicking the tree's refresh button; the left tree is a navigation structure and need not be recomputed every 5 seconds. Each level of the tree shows only the increment relative to the level above (subdomains drop the root-domain suffix, services show `https :443`, endpoints show only the path), with the full value in the hover tooltip and breadcrumb; when there are too many nodes the whole level drops the endpoint/service levels with a hint, and counts are still included in the upper level.

#### Fixed

- Fixed conversation records being squeezed and unreadable in mobile conversation details: on narrow screens the conversation list collapses, giving the width to the records themselves.

### Dependencies

#### Changed

- Upgraded norma v0.3.2 → v0.3.3: added `Config.MaxTokensField`, so the OpenAI Chat Completions output limit can be sent as `max_completion_tokens` (reasoning models accept only this key). The two keys are mutually exclusive, only one is sent, and the default remains `max_tokens`.
- Upgraded `golang.org/x/mod` v0.37.0 → v0.40.0, fixing two dependency security advisories.

### Contributors

- [@Autumn-27](https://github.com/Autumn-27)
- [@neouks](https://github.com/neouks)
- [@begininvoke](https://github.com/begininvoke)

## [0.3.6] - 2026-08-27

### LLM

#### Fixed

- Fixed the "non-streaming" switch reverting to "streaming" after saving and reopening (#69): the config list API's DTO was missing the `streaming` field, so the response never returned it, and the frontend read `undefined` and always fell back to the default streaming; the actual write and DB storage were fine, it just could not be read back. The DTO now includes the field (without `omitempty`, so `false` also appears in the response).
- The connection test now verifies the model actually replied and tests according to the config's real send/receive mode (#65): previously it only checked whether HTTP succeeded, so a request that went through but got zero reply from the model (thinking burned the budget / body swallowed by safety policy / compatibility layer dropping `content`) was also reported as "connection succeeded", split from the in-conversation "no reply" behavior; and the test always used streaming, so a non-streaming config was actually testing a different channel. Now an empty reply is directly judged a failure, a success shows the model's reply in the prompt, and the current streaming switch is carried into the test, keeping "test passes" and "conversation runs" consistent.

### Agent

#### Changed

- `list_facts` changed to paging + keyword filtering, avoiding a single call blowing up the context when there are many facts (#74): returns the latest 20 by default, supports `limit` (max 100) / `before` cursor / `q` summary keyword, returning `{facts, total, has_more, next_before}`; an overlong single summary is truncated by character count (the full text still goes through `node_detail`). The worker / planner prompts are updated to paging semantics accordingly. Old DBs have a one-time migration refreshing the new parameter schema into the tool catalog table (`SeedTool` insert-only, otherwise the tool management page shows "no parameters").

### Tools

#### Added

- The "Tool Execution" page gains tool call-count statistics (#72): the toolbar's "Statistics" button opens a dialog showing call count, proportion, and failure count per tool (descending by count); it reuses the list's task/keyword filters and counts the entire result set rather than the current page, fetched only when the dialog is opened.

### Dependencies

#### Changed

- Upgraded norma v0.3.1 → v0.3.2: fixes reasoning/refusal being silently dropped in OpenAI-family responses (Responses adds `reasoning_text`), three-name dedup for the reasoning field, bounded retry for empty responses (zero content blocks), and gateway 400s caused by compaction boundaries cutting tool pairings (also fixing existing orphans already baked into transcripts).

### Contributors

- [@Autumn-27](https://github.com/Autumn-27)

## [0.3.5] - 2026-08-25

### LLM

#### Added

- Each LLM config supports streaming/non-streaming switching (streaming by default): on uses streaming SSE; off uses true non-streaming (`stream:false`, a single complete JSON), which can bypass some gateways' poor SSE implementations (empty frames, dropped thinking-field frames), at the cost of losing in-progress real-time progress and real-time token counting. The worker / planner / mainagent / chat / goals all resolve dynamically from the currently active config; the three Provider wrappers `llmpool` / `llmrec` / task runtime are all compatible with non-streaming; `llm_profiles` gains a `streaming` column with an `ALTER` for old DBs (default `true`, old configs unaffected).
- Support for OpenAI Responses API format LLM configs: each config gains a third format, `openai-responses` (hitting `POST /v1/responses`), alongside Chat Completions / Anthropic; `BaseURL` normalization, default model `gpt-5`; the `format` constraint on `llm_profiles` adds `openai-responses` with an idempotent migration for old DBs; the frontend format dropdown adds "OpenAI (Responses API)". Dependency upgraded to norma v0.3.1 (including a `reasoning_content` return fix).
- Record the raw LLM request/response HTTP: captures the real wire body at the HTTP transport layer, retaining the tool schemas, `tool_use` blocks, and raw SSE frames that the normalized view cannot see; norma's internal multiple retry attempts are retained one by one; `llm_records` gains `raw_request` / `raw_response` columns with an `ALTER` for old DBs; the recording page's detail panel adds a "Raw" view switch and request/response copy buttons (compatible with the `execCommand` fallback for non-secure contexts).

### Conversations

#### Changed

- The conversation list's multi-select changed to a "Multi-select" mode toggle: the default list no longer keeps a checkbox on every row (cleaner appearance), the header changes to "N total + Multi-select" button; clicking "Multi-select" enters selection mode (checkboxes, select all, batch delete), "Done" exits and clears the selection, and batch delete automatically exits after all succeed (with failures it stays in selection mode for retry); single-item rename / pin / delete still go through each row's ⋯ menu.

### UI

#### Changed

- Enhanced task management, conversation actions, and traffic-viewing experience (#57): task list column-sort preferences are persisted and remembered, inline rename is triggered directly by an icon (no longer via the menu), and the drawer (sheet) interaction and traffic-viewing details are optimized.

#### Fixed

- Worker asset tags show only domains and IPs, and correctly handle the empty-tag case.

### Agent

#### Added

- The planner gains "quantitative acceptance check" when judging goals: when a goal has quantifiable acceptance conditions (asset test coverage reaching X%, obtaining N flags, gaining a certain privilege), `prove_goal` must first check the measured values in `graph_overview` (`coverage.pct`, findings counts, etc.); if the measured values fall short, `prove_goal` is forbidden and instead intents are dispatched to close the gap, and it may not mark met early on the grounds that "the main part is done / largely achieved". This fixed the problem where a goal requiring 100% coverage with a measured 40% was still judged complete.

#### Changed

- Rewrote the basis for the "0 intents this round" decision in the planner prompt: the original text described 0 intents as "the most common and most important principle", which would make the planner stop early when the goal is unmet and untested surfaces remain in scope. It is now changed so that 0 intents should only be produced in two specific cases — ① all thought-of directions are already covered by open/running/recent_done intents; ② the next step depends on a currently running work's output that has not yet appeared (should wait for it to finish and plan on the next wake-up after the graph updates). It also adds a reverse constraint: when there really are new directions not covered and not depending on a running work, or the goal is unmet with untested surfaces remaining, do not stop because "0 intents is common".
- Strengthened the "evidence threshold for negative conclusions" in the worker prompt: for negative conclusions that could make the planner abandon a whole direction (like "not injectable / port closed / no login entry"), the worker must exhaust the reasonable means within the intent (switch encoding/parameter/path/method) before concluding; when the means are not exhausted or the evidence is weak, it must mark `confidence=inferred`, avoiding a hasty `observed` negation that welds the whole route shut (an early wrong negation especially skews the direction and is hard to self-heal).
- The above planner / worker default prompt changes are appended as new versions and switched to current via one-time migrations (`reseedPlannerPrompt` / `reseedWorkerPrompt`, each guarded by a settings flag); old versions are retained in version history, and users who customized prompts can restore them from the version record.

### Operations

#### Added

- New `reset-password.sh` to reset the admin (username fixed as `LEVIATHAN`) password: supports local / docker deployment, with connection info either explicitly specified or auto-read from `--dsn`/`$LEVIATHAN_PG_DSN`/`config.json`; generates a bcrypt hash compatible with backend login using `pgcrypto` in the DB and writes it back to `settings.auth.password_hash`, needing no service restart after reset. The password is passed via environment variable, not into the process argv, with escaping to prevent injection.

### Contributors

- [@Autumn-27](https://github.com/Autumn-27)
- [@neouks](https://github.com/neouks)

## [0.3.4] - 2026-08-24

### UI

#### Added

- After checking multiple tasks in the task list, categories can be batch-modified, with the target category supporting "Uncategorized" to move tasks out of the current category; the whole batch writes in one transaction, and the only failures are tasks already deleted after checking.

#### Changed

- The "Task Category" in new-task creation changed from a dropdown to a searchable input: typing filters existing categories, a name not in the DB can be created and selected immediately by pressing Enter (or clicking "Create" in the dropdown), and the selection is shown as a removable tag; still limited to a single category.

### Conversations

#### Fixed

- Fixed messages being intercepted by "LLM not configured, cannot chat" when a conversation has an LLM config selected but there is no globally active config. The pre-send check only looked at the globally active config, while actual execution prioritized the conversation-selected config; the two resolution logics were inconsistent. They are now unified into the same set (conversation/Agent binding first, global fallback).
- The "cannot chat" prompt is now broken down by state: with no config at all it prompts "add a config", with a config but none active it prompts "activate one or specify one for this conversation", instead of always showing "not configured", making it easier to locate.

### Agent

#### Added

- For tasks whose goals are all met, intents dispatched by the main Agent via `add_intent` can be picked up and executed directly by a Worker, stopping when done: the Planner no longer runs (a task with no open goals does not enter planning), avoiding it re-judging the goal as met and cancelling the just-dispatched intent; after the frontier is drained the task returns to completed. Before dispatching an intent, the main Agent asks the user, based on the intent content, whether to register it as a formal goal; if registered, the task resumes normal autonomous planning.

#### Changed

- `get_worker_trace` / `get_task_worker_trace`'s `step_ids` no longer directly error when exceeding the one-time cap (5), instead returning the full content of the first 5 steps and informing via `returned_step_ids` / `omitted_step_ids` / `notice` which were taken this time and which were not, with the hint "if this is enough, no need to fetch more". Passed duplicate and illegal ids are deduplicated and removed before counting.

#### Fixed

- The task list changed to descending task id (newest created first), replacing the original creation-time sort. Multiple tasks created at the same moment have identical timestamps and unstable ordering, and combined with the 10-second polling and the random traversal of an in-memory map, they frequently swapped places in the list.

### Assets

#### Fixed

- Fixed enterprise attribution recalculation being broken by a single row of dirty data: when `assets.ip` stored a hostname (written by the Agent or asset API), `a.ip::inet` threw `22P02`, causing all of adding/modifying enterprise asset scope and deleting an enterprise to fail and roll back. Changed to use the safe conversion `try_inet()`, skipping illegal values instead of aborting the whole statement.
- Unparseable `ip`s are no longer silently skipped: after saving an enterprise scope it clearly reports how many assets' `ip` is not a valid IP (with specific ids and values), reminding that these assets will not be attributed by IP/CIDR rules; the same information is also written to the server log, covering paths with no frontend response (enterprise deletion, scopesentry sync, Agent writes).
- The task test scope's IP/CIDR matching likewise changed to `try_inet()`, replacing the previous regex guard that only checked the character set (all-hex-letter hostnames like `abc.def` previously slipped through and triggered the same error).

#### Changed

- The `ip` field of `ip`, `service`, `endpoint` assets no longer accepts hostnames: `insert_assets` and the asset API return a per-item error with an `index` and directly give the correction (use `type=subdomain` with `domain`, or resolve A/AAAA records first), so the Agent can fix and re-insert accordingly. Other assets in the same batch are unaffected and stored as usual.

### Contributors

- [@neouks](https://github.com/neouks)

## [0.3.3] - 2026-08-23

### Worker

#### Added

- A running Worker supports individual pause, resume, and cancel; pause retains intents, facts, and findings, and cancel transactionally cleans up the current intent and its direct outputs after execution exits.
- Added the `paused` intent state, an execution fence, and named termination reasons, preventing late blackboard writes after pause, cancel, or task deletion.
- Added an optional task concurrency cap; new creation, resume, finding drill-down, and queue backfill all go through a persisted FIFO admission path.

#### Changed

- The Worker's single-run wall-clock default changed from 600 seconds to 1200 seconds; a timed-out task re-times on its next real execution after revival.
- The real execution state of Worker, Planner, and main Agent uniformly drives the task status badge, fixing the problem where the Worker still showed "idle" while running.
- The Worker session retains individual controls and the current session's token summary, with the model name moved to display to the right of the current session title.

#### Removed

- Removed the Worker multi-select, select-all, and batch pause/resume UI, along with the Worker batch-control API and Mock contract.
- Removed the token badge and Tooltip from Worker list rows; the underlying full token ledger and task aggregation API are retained.

### LLM

#### Added

- Tasks support an ordered LLM config chain; when a vendor's insufficient quota is clearly identified, it automatically switches to the next config and persists the current config, exhaustion state, and error summary.
- Running and paused tasks support editing the full config chain, adjusting the order, and manually switching the current config, taking effect from the next LLM call.
- Automatic switching, manual switching, and full-chain exhaustion are written into structured system activity, with a deduplicatable alert popped via the task activity stream.
- Added a task role-model resolution API, uniformly resolving the config and model used by the Main Agent, Planner, and Worker for their next call.
- Added an optional global LLM Pool, supporting configured call order, designated-model fallback, health status, cooldown recovery, and manual reset.
- Added an always-on LLM usage ledger, aggregating input, output, and cached tokens by task, session, model, and config.

#### Changed

- Goal decomposition, Planner, Worker, and main Agent share the task-level LLM runtime (resolution priority below).
- Failover responds only to clear quota, balance, or billing errors; ordinary rate limiting, authentication, network, server, and context errors do not wrongly switch.
- The LLM config page changed to a config card list with right-side drawer editing, providing Pool round-robin order, priority, exclusions, health status, and recovery actions.
- Config-chain exhaustion info supports narrow-screen wrapping; the current model changed from a conversation-list icon to a text label and full Tooltip beside the current session title.
- The LLM resolution order for each role within a task is adjusted to "Agent binding → task config chain → global": a role with an explicitly bound model always runs on that model, an unbound role falls to the task chain, and when the task chain is empty it falls to global.
- The outbound proxy being empty means direct connection, no longer falling back to `HTTP_PROXY`/`HTTPS_PROXY` environment variables (an explicit `LEVIATHAN_LLM_PROXY` is unaffected); the proxy input supports `socks5://user:pass@host:port` with credentials.

#### Fixed

- A transient streaming failure before submission (with no output yet produced) is safely retried with backoff on the same provider, significantly reducing the cases of "interrupted after only one or two tools, no summary, terminal state `model_error`"; quota exhaustion, overlong context, and deterministic 4xx errors are not retried and are still handled by failover and compaction recovery respectively.

#### Removed

- Removed the LLM icon-style marking from the conversation list, avoiding confusion with the Worker status icon.

### UI

#### Added

- Tasks support global category creation, renaming, deletion, and filtering; new tasks can directly select a category.
- Added global task template CRUD with a right-side management drawer; new tasks can load preset descriptions and goals, or save the current content as a template.
- New tasks support associating multiple source tasks and multiple enterprise asset scopes; the enterprise scope auto-recognizes domains, URLs, IPs, CIDRs, ICP, and enterprise keywords through a single multiline text box.
- Task test assets support real-time addition and deletion, recording sources such as manual, enterprise, inherited, or Agent-discovered; the current test asset and source summary are shown beside the Worker session title.
- The Findings page groups by task with independent paging within each group, supporting filling in a description for a finding and creating a high-priority drill-down intent.
- Conversations support rename, pin, unpin, and delete; the task list supports batch pause and resume on the current page.
- Traffic details and tool execution details changed to a right-side drawer; HTTP messages complete the `Host` and highlight the request line, status code, headers, JSON, and markup-language bodies.
- The task list's action column gains "Details" and "Pause/Resume" buttons; the conversation send key combination can be configured in system settings (`Enter` / `Cmd+Enter`, etc.), and the Web search area gains a proxy input box.
- The badge to the right of the session title changed to show the LLM config name, with the model ID moved to a hover Tooltip; tasks can have an optional name, and the task list gains a name column and search (falling back to the description when empty).
- The Skills page adds usage count, last-used time, and missing-dependency statistics, easing location of skills that are not taking effect or not installed.

#### Changed

- The task title changed to a focusable details link; task assets, enterprise assets, finding task groups, and findings within groups use real server-side paging, stable sorting, and exact totals.
- Adding an enterprise changed to a right-side drawer, with scope entry unified into multiline text providing real-time recognition, validation, and type preview.
- The main Agent input box supports auto-grow, `Enter` to send, `Shift+Enter` for a newline, and avoids mis-sending during a Chinese IME composition state.
- Agent preview, task reports, and related details uniformly use the shared Markdown rendering; delete confirmations, long errors, and mobile drawer width are uniformly fixed.
- The task template selector displays and searches by template name and submits by template ID; the system's original font size is restored, and the app version is uniformly shown as `0.3.3`.
- The task total and current session tokens only highlight input, cache-read, and output values; the send button uses a cleaner up-arrow icon.
- The task overview's test scope shows enterprise names rather than IDs.

#### Fixed

- Ordinary text containing dots is no longer misjudged as an ICP filing number.
- Fixed a name collision between the variable directory and global runtime variables (like `{{.Now}}`), causing the Agent editor's variable list to render duplicate keys.

#### Removed

- Removed the task card's separate "Enter" button, unifying entry to details by clicking the task title.
- Removed the dashboard's "New Task" button, the new enterprise's Logo URL input, and the finding-count Badge at the top of the Findings page and in task groups.
- Reverted the global 10% font-size increase, and removed the Worker selector, model icons, and row-level token statistics from the conversation list.

### Agent

#### Added

- New tasks can directly associate multiple existing tasks, inheriting in real time and read-only the goals, facts, findings, completed intents, asset scope, and blackboard context of the direct source tasks.
- Blackboard read tools support on-demand querying of the source task's nodes, facts, findings, and execution traces; inherited nodes carry a source marker and all write tools refuse to modify them.
- Enterprise scope tools support domains, URLs, IPs, CIDRs, ICP, and enterprise keywords; keywords serve only as an Agent scope hint and do not participate in automatic asset attribution.
- The finding drill-down operation creates a high-priority manual Worker intent with an asset anchor and a `derived_from` edge in the original task.
- The overview gains a "Goal Management" card, supporting manual viewing, adding, modifying, and deleting goals; adding or modifying notifies the Planner and revives the task, while deleting hard-deletes the goal node (cascading edges and anchors) but does not revive.
- The main Agent gets the `steer_work` tool by default, allowing real-time injection of steering instructions into a running Worker without interrupting it or losing progress (still validating that the intent belongs to this task).

#### Changed

- Source-task intents do not enter the new task's frontier; the new task continues to use an independent exploration, execution queue, working directory, and conversation history.
- Deleting a running intent no longer destroys data: it changes to stopping at `stopped`, requires a deletion reason, hangs the reason as a fact on the intent and writes it into the payload, and the planner perceives it as triggered by `cancelled` (retaining the intent content and reason).
- Main Agent conversation changed to be driven by the server-side activity stream, with input recoverable on send failure; opening a session auto-scrolls to the bottom and keeps the last reply visible after details are lazy-loaded.
- Pausing a task terminates the current Main Agent, Planner, and Worker calls, but does not prevent the user from actively starting a new main Agent orchestration session during the pause.
- Cancel, close, and stream interruption uniformly retain the real termination reason, generated content, rounds run, elapsed time, tokens, and uncommitted tool calls.

#### Removed

- Removed the main Agent client's optimistic message echo, avoiding duplicate messages and cross-session streaming under pause, failure, or concurrent activity.

### Constraints

#### Added

- The goal-decomposition stage first extracts operation constraints (allow/deny) and then decomposes goals, and the main Agent can supplement them at runtime; constraints are injected into the Planner and Worker system prompts at the highest priority, and after de-biasing, diversity and surface-expansion exploration explicitly defer to the constraints.
- The overview gains a "Constraint Management" card with add/edit/delete APIs; the constraint injection scope can be toggled separately for Planner / Worker (both on by default, read each round, effective immediately). The new table `task_constraints` uses `CREATE TABLE IF NOT EXISTS`, auto-created for old DBs.

### Interception

#### Added

- Command interception adds model-fallback approval beyond regex/string rules: when no rule matches, the model makes an `ALLOW`/`ASK`/`DENY` semantic judgment, and the failure fallback action and approval-timeout action are configurable; the interception page is split into "Interception Rules / Model Config" tabs, and the model judgment result is labeled with a `[model]` prefix and reason.

#### Fixed

- Hardened the judge output parsing, avoiding a correct `DENY` verdict being mistaken for an allowance.

### Traffic

#### Changed

- Traffic recording changed to storing the whole exchange in SQLite (large bodies spill to a hash-deduplicated blob bucket), with text bodies getting a trigram full-text index, supporting arbitrary substring and Chinese search; deletion degrades to a single SQL transaction, dropping from hours to milliseconds without pausing recording during it. Added large-body streaming download and `traffic_search` full-text parameters; old file-tree data needs no migration and remains readable, searchable, and deletable.

### Tasks and Assets

#### Added

- Task creation gains an "asset coverage feature" switch (on by default): when off, coverage is not computed or shown, the posture graph shows only assets, `task_scope` is not automatically accumulated, and related tools are removed from the Planner / main Agent; enterprise association is unaffected by this switch.
- `insert_assets` gains a `related` marker per asset (default `true`): effective only when coverage is on; `false` only enters the shared asset library and is not counted in this task's coverage (like an incidentally found side site or irrelevant asset).
- Enterprise scope and task test assets uniformly support domain, URL, IP, CIDR, ICP, and keyword text recognition; domains/IPs can create or reuse global assets, while CIDR/ICP/keywords remain as task scope context.

### Build

- `build.sh --release` supports building Linux amd64/arm64, macOS amd64/arm64, and Windows amd64 in one go, and generates zip release packages with `skills/`, config examples, and README.
- Uses the Go linker to strip debug info and generate zip release packages; UPX is changed to explicitly optional, avoiding segfaults at startup for its self-extracting ELF in some Linux environments.
- The Release Workflow runs a startup smoke test on the Linux amd64 binary and provides `SHA256SUMS` with the release package.

### Contributors

- [@neouks](https://github.com/neouks)

## [0.3.2] - 2026-08-20

### Added

- New-task creation supports associating multiple existing tasks, inheriting in real time and read-only the facts, findings, completed intents, asset scope, and blackboard context of the direct source tasks; the new task still uses an independent execution queue, working directory, and conversation history.
- Tasks support an ordered LLM config chain. When a vendor's insufficient quota is clearly identified, it automatically switches to the next config and persists the current config, exhaustion state, and structured audit activity.
- Running and paused tasks support editing the LLM config chain, adjusting order, and manually switching the current config; automatic switching, manual switching, and full-chain exhaustion are shown on the task details page.
- A running Worker supports pause, resume, and cancel. Pause retains blackboard data; cancel transactionally cleans up that intent and its directly produced facts, findings, and execution records after the Worker stops writing.
- Deleting a task can optionally also clean up related assets, traffic, findings, and task landing files, with a deletion barrier, concurrency protection, and auditable deletion statistics.
- Added an optional task concurrency cap; new tasks at the cap queue by FIFO and start automatically when a run slot is freed.
- Added task-asset paging, company-asset paging, task-category statistics, and a paged-intent Mock contract.
- Added the `build.sh` single-binary build script, supporting static frontend export, asset embedding, cross-platform targets, and build-version injection.

### Changed

- The task-level explicit LLM config chain coexists with the existing global LLM Pool; the explicit chain continues to use strict quota failover semantics, and without an explicit chain it follows the Agent-binding and global config rules.
- The main Agent input box supports auto-growing multiline input, `Enter` to send and `Shift+Enter` for a newline, and avoids mis-sending during a Chinese IME composition state.
- Planner, Worker, and main Agent share the task-level LLM runtime; the task chain uses the smallest context window among candidate models as the safe compaction threshold.
- The task details page shows running, idle, and paused states according to the real LLM call state; the source task's facts, findings, intents, asset references, and graph nodes are uniformly marked with their source and kept read-only.
- Agent preview, task reports, and related detail views uniformly use the shared Markdown rendering component.
- The LLM model config uses a card and drawer interaction, and the model list supports independent scrolling within the drawer.
- Pausing a task no longer blocks main Agent conversation: the main Agent orchestration session is independent of the task pause, and new messages can still be sent during a pause (pause only terminates the currently in-progress round).
- The Worker's single-run wall-clock default changed from 600 seconds to 1200 seconds.

### Fixed

- Removed the main Agent console's optimistic echo, switching to pure server-side data rendering, fixing message confusion and content bleeding into other sessions under pause or send failure.
- Opening a main Agent session now scrolls to the bottom by default and fully shows the last reply: the last reply's full content is lazily expanded and auto-scrolled to the bottom, no longer pushed off screen.
- Fixed the main Agent conversation still running after pausing a task, and the orchestration Agent not synchronously stopping the main Agent when pausing a task.
- Fixed the task status badge and action-button state being out of sync when a task is completed, or the Planner, Worker, or main Agent is actually running.
- Fixed possible late blackboard writes or leftover files between Worker cancel, task deletion, and concurrent writes.
- Fixed Markdown preview failing, delete-confirmation long-text overflow, mobile width, and some task-details buttons being misaligned.
- Added named termination reasons to all Agent cancel paths; activity details can show the canceller, terminal state, rounds run, elapsed time, token usage, and uncommitted tool calls.
- Fixed a race where the parent context could preempt the named `shutdown` reason when the backend shuts down, and the garbled text caused by byte-truncating a Chinese activity summary.
- Fixed the cancel event with partial streaming output losing the real termination reason, while retaining the content generated before cancellation.

### Contributors

- [@Autumn-27](https://github.com/Autumn-27)
- [@neouks](https://github.com/neouks)

[Unreleased]: https://github.com/Autumn-27/LEVIATHAN/compare/v0.3.10...HEAD
[0.3.10]: https://github.com/Autumn-27/LEVIATHAN/compare/v0.3.9...v0.3.10
[0.3.9]: https://github.com/Autumn-27/LEVIATHAN/compare/v0.3.8...v0.3.9
[0.3.8]: https://github.com/Autumn-27/LEVIATHAN/compare/v0.3.7...v0.3.8
[0.3.7]: https://github.com/Autumn-27/LEVIATHAN/compare/v0.3.6...v0.3.7
[0.3.6]: https://github.com/Autumn-27/LEVIATHAN/compare/v0.3.5...v0.3.6
[0.3.5]: https://github.com/Autumn-27/LEVIATHAN/compare/v0.3.4...v0.3.5
[0.3.4]: https://github.com/Autumn-27/LEVIATHAN/compare/v0.3.3...v0.3.4
[0.3.3]: https://github.com/Autumn-27/LEVIATHAN/compare/v0.3.2...v0.3.3
[0.3.2]: https://github.com/Autumn-27/LEVIATHAN/compare/v0.3.1...v0.3.2
