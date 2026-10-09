# `/btw` Validation Record

Date: 2026-09-10. Branch: `codex/btw-side-question`. Baseline: `8dae851b9b622f2ff2631f332fde9719d0b16fba`.

Uses an independent PostgreSQL test database and data directory; real model credentials were injected only into the isolated test environment, never written into code or this record, and the product's default model was not changed. Go 1.26.3, norma v0.3.6, Next.js 16.2.9.

The actual model conversations, returned objects, engineering assertions, and the raw Qwen review text are stored in [validation-2026-09-10.json](validation-2026-09-10.json), which contains no API credentials.

## Engineering Checks

| Scope | Result | Evidence |
| --- | --- | --- |
| Structured messages, tool parameter deep copy | Pass | `TestCheckpointDeepCopyAndBoundaries` |
| Summary / compaction requests do not overwrite, complete replies and terminal state are published, half replies are excluded | Pass | `TestCheckpointDeepCopyAndBoundaries`, `TestSnapshotExcludesPartialStreamAndSelectsPoolMember` |
| Actual model pool member identity | Pass | `TestSnapshotExcludesPartialStreamAndSelectsPoolMember` |
| Tool pairing, 20-group replay, budget trimming and over-limit errors | Pass | `TestBuildRequestCompactionToolPairingAndBudget` |
| Main/side concurrency, bidirectional cancellation isolation | Pass | Blocking Provider, `TestMainSideConcurrencyAndIndependentCancellation` |
| No tool execution, streaming / non-streaming, existing usage on failure | Pass | `TestServiceNoToolsAndUsageOnFailure` |
| Real norma ChatAgent + local Read tool, main transcript / activity isolation | Pass | `TestSideActualChatCheckpointToolResultAndTranscriptIsolation`, streaming and non-streaming subtests |
| Persistence, paging, idempotency, retaining partial answers across restart | Pass | `TestSideHistoryIdempotencyPagingAndRecovery` |
| Clear vs. late-write race, parent resource deletion, version comparison | Pass | `TestSideClearLateWritersAndDeletedParent` |
| MainAgent / Worker archive and restore, v1/v2/v3 | Pass | `TestSideTaskArchiveVersions` |
| Three parent interfaces, auth, resource ownership, Worker logical delete | Pass | `TestSideHTTPGlobalLimitTaskWorkerAndDeletion`, `TestSideCheckpointPersistsBeforeAdmissionAndRestart` |
| Busy main session can side-question, isolated SSE reconnect / disconnect, cancel, clear | Pass | `TestSideHTTPBusyIsolationClearAndReconnect` |
| 1 per parent session / 4 global concurrency | Pass | two `TestSideHTTP…` cases |
| Snapshot persisted before submission, resume after restart, old sessions cannot forge a snapshot | Pass | `TestSideCheckpointPersistsBeforeAdmissionAndRestart` |
| Reject continuation after the cached config is deleted or the model changes | Pass | `TestSideRejectsDeletedOrChangedCachedProfile` |
| Cancel before archive and wait for the final answer and usage to persist | Pass | `TestSideTaskDrainPersistsBeforeArchive` |
| Usage recorded only once on early consumer cancellation, with side attribution | Pass | `TestSideUsageRecordedOnceOnConsumerCancellation` |
| A Worker restored after restart / a deadline runtime keeps publishing new snapshots | Pass | `TestSideRestoredWorkerRuntimePublishesNewCheckpoint` |
| Race check on related packages | Pass | the commands below |
| TypeScript and production build | Pass | `npx tsc --noEmit`, `npm run build` |
| Biome on new frontend modules | Pass | `biome check`, 3 new modules |

After configuring `LEVIATHAN_PG_DSN` in a separate disposable database, the automated checks can be reproduced (do not point it at the production database):

```sh
go test -race ./agent ./db ./server ./sidequestion ./llmrec ./llmpool \
  -run 'Test(Side|Checkpoint|Snapshot|BuildRequest|Service|MainSide|CaptureRun|TaskArchive|CompleteForwards|StopIntent|CancelIntent)' -count=1
cd web
npx tsc --noEmit
npx biome check src/lib/side-questions.ts src/hooks/use-side-questions.ts src/components/side-question-workspace.tsx
npm run build
```

The full Go regression is not all green: two pre-existing tests in the `server` package fail during temp-directory cleanup, both reporting `TempDir RemoveAll … directory not empty`:

- `TestInheritedActivityDetailAndRelationDeletion`
- `TestTaskMetadataPatchReturnsRenameAndPin`

Exporting the source from the above unmodified baseline and re-running the `server` package in the same isolated environment also reproduces these two cleanup failures. The baseline run additionally showed a target-node-count assertion failure in `TestCoreTaskLifecyclePG`; the final modified `server` regression does not have that assertion failure. Other packages passed, and this change's side-question-related cases and race checks passed. Baseline issues were not marked as accepted for this change, nor were existing assertions modified to hide problems.

The Next.js build emits the existing multiple-lockfile / workspace-root inference warnings; the build completed and all pages generated successfully.

## Browser Checks

Using Codex In-app Browser, connected to an isolated local Go service and Next.js dev server. The following manual/automated operations were performed on desktop and a 390 × 844 narrow screen, checking screenshots and browser logs:

- Typing `/btw` while normal chat is running shows the main content and the side question at once; the desktop sidebar works.
- Follow-ups in a row; after stopping the side question the already-generated part is retained; the main flow continues.
- The request continues when the panel is closed, and the completed answer is restored on reopen; after a page refresh an empty `/btw` restores history.
- On the narrow-screen Drawer, input, buttons, history, and close operations work, with no horizontal overflow.
- Clearing uses a confirmation dialog; after clearing the history disappears and the main transcript and snapshot are retained.
- The task MainAgent and two Workers were questioned and switched between separately; Agent labels and history did not cross-talk.
- A blocking local model fixture keeps the Worker running; submitting `/btw` from the Worker's main input box, the Worker still shows live running and its own pause button after the side question is stopped, and the side question saves a partial answer.
- The browser error / warning log is empty.

The controllable fixture is used to verify concurrency timing precisely without depending on the real model's output speed. During debugging, two Worker runtime checks did not form a valid concurrency window (the task had already ended / the answer ended early), so the fixture was fixed and redone, and passed; these initial operations are not recorded as valid passes.

## Real Model Conversations

`grok-4.6` was probed first, on the OpenAI-compatible endpoint `http://127.0.0.1:12580/tingly/openai`. The probe returned HTTP 200, with model name `grok-4.6` and `READY`, taking 2.82 seconds. The first choice was available, so the Tingly `glm` or Zhipu `glm-5.3` fallback chain was not enabled; these two fallback services were not validated this time.

| Scenario | Actual result |
| --- | --- |
| Ask for the asset, goal, and marker while the main session is running | Returns `redhaze.top`, the homepage-read and summarize goal, `BTW-REAL-0910`; the side question completes in 16.97 seconds |
| Ask for the tool basis after the main session finishes the homepage read | Correctly cites WebFetch 200, the curl redirect 301 → 302 → 200, and the page title; 7.24 seconds |
| The side question asks Bash to create a test file | Refuses to execute, the target file is not created; 7.74 seconds |
| A completed side question does not change the main context | The main transcript SHA-256 stays consistent with the main activity record; side tool executions = 0 |
| Follow-up after actually stopping / restarting the Go service | Retains the previous 3 side-question history entries and answers the asset, marker, and title directly from the persisted snapshot, without re-running the main Agent |
| A new session using the Grok non-streaming config | Correctly answers the asset and `ATOMIC-0910`; returns and saves usage: input 11734, output 138, cache_read 11520 |

The main session for the asset case uses WebFetch and Bash/curl to read the public homepage; the landing page is `https://id.redhaze.top/home`, with the title "红幕科技 RedHaze Group · 全球综合集团门户". Bash staged the response in a local test file; no write was performed against the remote. This fact is verified separately from "the side question executed no tools".

Main transcript checksum: `e7e61f135a4a120954b539f357e8c4205d7d5cd7460dcaf3dc0fd066463e1d00`.

**Usage limitation:** Tingly's Grok streaming responses do not return usage. A separate direct request with `stream_options.include_usage=true` verified this: HTTP 200, 12 data frames, 0 usage frames. So the 0 in the streaming test means the endpoint did not provide usage, and cannot be interpreted as no billing. Non-streaming usage, as well as the fixture's failure / cancellation usage, are all saved correctly.

## Qwen Review

Review model `qwen-flash`, OpenAI-compatible endpoint `https://dashscope.aliyuncs.com/compatible-mode/v1`, HTTP 200. The first three real side-question conversations, the main-session tool basis, and the engineering assertions were provided; it returned `verdict: accept`, `concerns: []`, considered the answers consistent with the asset, marker, and page-read evidence, and the side tool refusal compliant with the constraints. Review usage: prompt 6625, completion 312, total 6937.

This Qwen review scope does not include the later-added service restart and non-streaming tests. Qwen's characterization of "no write" is too broad: the main session's curl did create a local temporary response file, as clearly recorded above. Concurrency, zero tool executions, and transcript isolation are judged by the engineering assertions; the model review only assists in assessing answer quality.
