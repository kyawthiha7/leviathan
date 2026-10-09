# Side-Question Long Conversations and Context Budget

Fixed 2026-09-11. The original implementation treated the request JSON's character count directly as tokens and inherited the main task's 32K output reservation, so normal side questions were rejected early whenever there was a lot of HTML, JS, and tool output.

## Open-Source Implementation Review

- [Grok CLI side context](https://github.com/superagent-ai/grok-cli/blob/fb97af83f06dca873281d60168430f06c8de6324/src/agent/agent.ts#L739): extracts fragments from the most recent user/assistant text, with a character budget of about 2000 and at most 400 characters each. It does not maintain continuous side Q&A history on this path.
- [Grok CLI independent request](https://github.com/superagent-ai/grok-cli/blob/fb97af83f06dca873281d60168430f06c8de6324/src/utils/side-question.ts): independent cancel signal; output cap of 2048 tokens when the model supports it; no tools provided.
- [Grok CLI main-session compaction](https://github.com/superagent-ai/grok-cli/blob/fb97af83f06dca873281d60168430f06c8de6324/src/agent/compaction.ts): estimates tokens, keeps recent content, updates new content into the old summary, and handles cross-turn truncation.
- [OpenCode session compaction](https://github.com/anomalyco/opencode/blob/b3f1a96c6dd7adeb28b36dd11add1998fc84d67b/packages/core/src/session/compaction.ts): full-request estimation, output/buffer reservation, recent content plus rolling summary, tool-free summary request; this implementation defaults to a recent budget of 8000 and a summary output cap of 4096 tokens.
- [OpenCode overflow recovery](https://github.com/anomalyco/opencode/blob/b3f1a96c6dd7adeb28b36dd11add1998fc84d67b/packages/core/src/session/runner/llm.ts): overflow recovery is attempted only before assistant output has started, and the recovered call does not re-enter the same overflow-recovery path.

LEVIATHAN borrows the independent output budget, recent content plus rolling summary, and bounded recovery. It keeps norma v0.3.6's structured messages and tool pairing and does not copy Grok's text excerpts; it does not write OpenCode's main-session compaction events into LEVIATHAN's main transcript.

## Request Budget and Execution

- Messages follow norma's per-content-block UTF-8 byte estimate with a 4/3 margin; system prompt, tool schema, and message framing overhead are added on top. The estimate is not an exact model token count.
- Side output defaults to at most 8192 tokens and never exceeds the output cap already set in the main config. The service env var `LEVIATHAN_BTW_MAX_OUTPUT_TOKENS` can set a cap of 256–32768; it does not modify the product's default model or main-task params.
- The input budget is the context window minus the output cap and a safety margin; an unknown window uses the platform default of 200K. The safety margin is 5% of the window, min 128 and max 8192 tokens.
- Successful Q&A is loaded in batches of at most 20 by increasing sequence. At most 20 originals are retained, with a token budget of at most 1/4 of the input budget and no more than 16K.
- Excess Q&A is folded into a rolling summary. The summary carries historical provenance and context time; a historical assistant answer is not equivalent to new tool evidence, and on conflict the latest main snapshot takes precedence.
- When the main context is still too long, only old messages in the side copy are summarized, keeping at most 8K tokens of recent content; the cut point does not split a tool call from its result. An oversized single group goes entirely into the summary.
- Summary input is UTF-8-safe chunked according to the actual remaining window, with an output cap of 2048 tokens; an empty summary, truncation, a tool call, or exceeding the summary budget all skip the cache write. A single side question makes at most 12 summary calls, subject to the same 120-second timeout; hitting the cap fails explicitly and never loops indefinitely.
- If the model first returns a context-length-exceeded error and has not yet output text or a tool call, at most one retry is allowed after further reduction; if the estimated size does not drop, recovery stops immediately. Other model errors and partial streaming output do not trigger this recovery.
- All obtained usage, including summaries, failed attempts, and usage at cancellation, accumulates into the same side request. When the Provider returns no usage it can still only record zero and must not pass off an estimate as actual usage.

## Persistence and Interface

`side_question_sessions.memory` stores the old Q&A summary, the covered ordinal, and the main-context summary cached by snapshot identity. `side_question_requests.context_info` stores the preparation phase, actual replay group count, summary usage, and budget estimate.

A summary is saved only while the original request is still running and the clear version matches; clearing also clears the cache, and a late write will not resurrect cleared data. A new snapshot does not reuse the old snapshot's summary. Summary fields are saved with the v3 task archive; when restoring an old v3 missing the fields an empty object is filled in, and v1/v2 remains compatible.

POST accepts and returns the request first, and preparation and compaction run in the background without holding the admission lock or a database transaction. SSE/history shows the preparation, Q&A consolidation, copy compaction, and answer phases; a compaction failure is saved as the failed terminal state of that side request. The frontend keeps the error and restores the draft of the question that failed, without a floating notification covering the input box; history polling no longer clears the submit error.

## Validation Record

- 19/20/21/50-group replay, retention of old conclusions across the 20-group boundary, and summary-cache reuse across restart: automated checks passed.
- Very long Chinese answers and code context, chunked request budget, tool pairing, snapshot immutability, and new-snapshot cache invalidation: automated checks passed.
- Summary failure/cancel/truncation/oversize/tool return, clear races, call cap, single overflow recovery and no retry on partial streams: automated checks passed.
- Paging, restart, v1/v2/v3 archiving, summary-cache and budget-metadata archive recovery, old v3 missing new fields, and 20 parent sessions sharing four concurrency slots on an independent PostgreSQL: passed.
- Go candidate service build, frontend TypeScript check, Biome check on modified components, and Next.js production build in an independent directory: passed.
- The built-in browser used an independent UI fixture to verify, at 1280×720 and 390×844, the consolidation phase, the summary-range hint, draft recovery after failure, no floating error notification, no horizontal overflow, and no console errors. The temporary fixture has been removed.
- Read-only replay of the machine's existing Worker snapshots passed the new budget check; for example, Worker #3's 293085-character snapshot is no longer wrongly rejected by the local character count. This item made no external model calls.
- The real-conversation test that would send a private Worker snapshot to Grok was rejected by the automated approval review, was not executed, and does not count as a pass.
- At 2026-09-11 00:37 the local backend was restarted at the user's request, reusing the original database, data directory, and login config. The running file matches the candidate binary's SHA-256; `/api/health` on both the backend and the frontend proxy returned normal.

Validation commands (independent test database only):

```sh
go test -race ./sidequestion ./db ./server -run 'TestSide|TestCheckpoint|TestSnapshot|TestBuildRequest|TestService|TestMainSide|TestTaskArchive' -count=1
go build ./cmd/leviathan
npx tsc --noEmit
npm run build -- --webpack
```

The frontend production build uses an independent copy to avoid overwriting the `.next` of the current preview. The candidate service is at `/private/tmp/leviathan-btw-budget-candidate`, copied to `/private/tmp/leviathan-btw-preview/leviathan` and started; the original binary is backed up as `leviathan.before-context-budget` in the same directory.
