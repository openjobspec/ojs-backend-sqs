# AUDIT — SQS reliability finalization

Branch: `refactor/clean-code-srp`

All changes remain unstaged. No commit was created and no sibling repository was
modified. Public HTTP routes, protobuf definitions, JSON schemas, DynamoDB
table/GSI names, existing PK/SK records, SQS queue names, environment variables,
and service identity remain unchanged. New DynamoDB item types and internal
attributes are additive.

## Resulting architecture

- **Transactional job state:** jobs carry a version, delivery generation,
  receipt owner, worker owner, and delivery deadline. DynamoDB conditions fence
  every available→active claim and terminal/requeue transition.
- **Durable delivery outbox:** job creation/promotion/requeue/replay commits a
  stable delivery intent with the state mutation. FIFO deduplication uses
  `<job-id>-<delivery-generation>`; standard-queue duplicates are rejected by
  the conditional claim.
- **OJS-managed retries/DLQ:** native SQS redrive is removed. Expired worker
  leases are reaped through tracked state and stable delivery intents, honoring
  `max_attempts`; exhaustion uses the OJS DynamoDB DLQ marker.
- **Durable orchestration:** workflow/cron side effects are persisted as stable
  job effects. Terminal workflow accounting is also persisted transactionally
  with the job transition, then deduplicated and applied after restarts.
- **Multi-replica scheduling:** cron occurrences compare-and-swap
  `next_run_at`, write the occurrence lock, advance the schedule, and persist
  the job effect in one transaction.

## Review findings completed

1. **Unique replace**
   - One transaction swaps the unique mapping, creates the replacement and its
     scheduled marker/delivery intent, cancels an eligible predecessor, removes
     stale markers/intents, and persists workflow cancellation accounting.
   - Active predecessors are rejected. Mapping TTL and configured states are
     honored. A short replacement guard plus mapping CAS prevents concurrent
     replacement cascades.
   - Ambiguous transaction responses are reconciled before returning failure.

2. **Conditional delivery claims**
   - Fetch conditionally performs only `available → active`, increments
     `attempt`, and records generation, receipt, worker, message ID, and
     deadline.
   - Stale, duplicate, active, cancelled, and terminal SQS messages are deleted
     without reactivation.
   - ACK/NACK/cancel/heartbeat/reaper transitions match the active generation
     and receipt (and worker for heartbeat); only the transition winner performs
     follow-up effects.

3. **Native SQS DLQ**
   - Removed the fixed `maxReceiveCount=3` redrive policy from runtime queue
     configuration and Terraform.
   - Existing queue redrive is cleared where AWS/LocalStack supports it.
     Compatibility DLQ queue names remain unchanged.

4. **Durable delivery outbox**
   - Initial push/batch, scheduled and retry promotion, explicit requeue,
     visibility-timeout recovery, DLQ replay, workflow jobs/callbacks, and cron
     jobs all use stable durable intents/effects.
   - Scheduled/retry source markers are retired only after confirmed delivery
     or state-based reconciliation proving an ambiguous send arrived.

5. **Partial `SendMessageBatch`**
   - Each response entry is mapped back to its stable intent. Only successful
     entries are retired; failed or omitted entries remain pending with failure
     metadata.

6. **Workflow atomicity**
   - Completion IDs are transactionally deduplicated.
   - Counter updates, chain-step election, final-state election, results,
     callback IDs, and next-step effects commit together.
   - A durable workflow-advance intent is written with every terminal job
     transition, closing the ACK/crash gap. Cancellation fences later effects.

7. **gRPC workflow DAGs**
   - Dependency-free steps map to `group`.
   - A strict single-path DAG maps to an ordered `chain`, preserving enqueue
     options.
   - Empty, duplicate-ID, missing-dependency, cyclic, branching, converging, and
     otherwise unsupported DAGs are rejected before persistence.

8. **DynamoDB pagination**
   - Due queries/fallback scans, all cron scans, job/worker admin scans,
     state/queue counts, workflow jobs, and workflow results follow
     `LastEvaluatedKey`.
   - Existing offset/limit API semantics remain unchanged.

9. **Cron timezone and ownership**
   - One parser applies the stored timezone at registration and every
     advancement; omitted timezone is UTC.
   - Per-occurrence transaction ownership prevents double fire across replicas.

10. **Worker heartbeat records**
    - Heartbeats use `UpdateItem`, preserving directives and unrelated metadata.
    - Directives are returned and atomically consumed once.

11. **Visibility and receive errors**
    - Fetch and heartbeat share millisecond→second conversion: positive values
      round up to at least one second and cap at 12 hours.
    - Stored per-job visibility is applied after claim.
    - `ReceiveMessage` failures are returned instead of being reported as an
      empty successful fetch.
    - Fetch now has deterministic partial-success semantics: once at least one
      job is conditionally claimed, it returns all claimed jobs with a nil
      error so HTTP/gRPC transports cannot discard them.
    - Per-message generation, visibility-change, and conditional-store failures
      are isolated while other messages continue. The failed message is not
      deleted and remains eligible for visibility recovery/redelivery.
    - A later receive or queue failure never drops earlier claims. Suppressed
      partial failures are emitted through structured warning logs and
      `ojs_fetch_partial_failures_total{stage=...}`.

12. **Fault and shutdown coverage**
    - Added corrupt-attribute/WRONGTYPE-equivalent tests, transaction
      cancellation mapping, partial batch, throttled/ambiguous send recovery,
      scheduler in-flight cancellation, and realtime middleware interface
      preservation.
    - Added unit and LocalStack fault coverage for success followed by corrupt
      delivery generation, visibility-change throttling, conditional claim
      store failure, second-receive failure, and a later multi-queue failure.
      Tests verify successful jobs are returned exactly once and failed
      messages are recovered/redelivered.

13. **Documentation/infrastructure**
    - README now documents conditional ownership, managed retries/DLQ, and the
      outbox.
    - `make lint` invokes CI-pinned `golangci-lint v1.64.8`, avoiding host v2
      config incompatibility.
    - SSE/WebSocket-compatible middleware now preserves `Flusher`/`Hijacker`.

## Verification gates

| Gate | Command | Result |
|---|---|---|
| Changed Go formatting | `gofmt -l <changed .go files>` | pass |
| Diff validation | `git diff --check` | pass |
| Standalone build | `GOWORK=off go build ./...` | pass |
| Make build | `make build` | pass |
| Vet | `GOWORK=off go vet ./...` and `make lint-vet` | pass |
| Lint | `make lint` (`golangci-lint v1.64.8`) | pass, 0 issues |
| Make test | `make test` | pass |
| Full race + coverage + LocalStack | `OJS_LOCALSTACK_ENDPOINT=http://localhost:4566 GOWORK=off go test ./... -race -cover -count=1` | pass |
| LocalStack stress | reliability suite, `-race -count=5` | pass in 64.193s |
| HTTP lifecycle | enqueue→fetch(attempt 1)→ack→completed | pass |
| Realtime SSE | queue subscription + enqueue event | pass |
| gRPC/realtime | generated client: manifest, enqueue/fetch/heartbeat/ack, DAG workflow, streamed event | pass |

Coverage from the final LocalStack race gate:

- `internal/api`: **13.9%**
- `internal/grpc`: **31.4%**
- `internal/scheduler`: **77.4%**
- `internal/server`: **6.8%**
- `internal/sqs`: **49.2%**
- `internal/state`: **24.2%**

LocalStack reliability coverage includes 16-way unique replacement, concurrent
claiming, ACK/cancel racing, scheduled promotion, requeue generation, crashed
worker exhaustion and DLQ replay, concurrent workflow completion, restart-safe
workflow advancement, callback/cancellation fencing, cron multi-replica
ownership, directive consumption, heartbeat ownership, and native-redrive
absence, including stored per-job visibility overriding the fetch default.
Partial-fetch coverage injects corrupt generations, visibility throttling,
conditional-store failures, and later receive failures against real LocalStack
messages, then verifies failed deliveries retry and successful claims are not
returned twice.

## Conformance

Targeted original-suite results against a fresh LocalStack-backed server:

- **10 / 13 pass:** `L0-LC-003`, `L0-LC-004`, `L2-TZ-001`,
  `L3-GRP-002`, `L3-CHN-001`, `L3-BAT-002`, `L4-UNIQ-003`,
  `L4-UNIQ-005`, `L4-UNIQ-007`, `L4-BULK-001`.
- The three selected Level-1 fixtures (`L1-RTR-013`, `L1-VIS-002`,
  `L1-VIS-001`) fail at enqueue because their fixture job types contain hyphens
  (`attempt-counter`, `heartbeat-extends`, `timeout-requeue`), which violate the
  suite's own normative type regex
  `^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*)*$`.
- Temporary local copies changing only those type segments to underscores all
  pass: retry attempt increments, heartbeat extension, and timeout requeue.
  The temporary files were removed; the sibling conformance repository was not
  edited.

## Genuine blockers

1. **Three invalid upstream conformance fixtures** described above prevent the
   original files from reaching backend behavior.
2. **Full-suite isolation:** the runner can reset Redis or an HTTP reset
   endpoint, but this backend has neither a DynamoDB/SQS reset endpoint nor a
   runner-native reset. Targeted tests used a fresh table/prefix.
3. **Terraform CLI unavailable:** `terraform fmt -check -recursive` could not be
   executed (`terraform not installed`). The only HCL change is removal of the
   redrive block plus a formatted comment.

There are no known product-code blockers in the implemented review scope.
