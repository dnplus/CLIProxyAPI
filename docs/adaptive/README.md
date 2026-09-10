# Adaptive quota router v1

This fork adds an executable observation → estimate → shadow decision → opt-in execution path. It optimizes accepted work under an estimated budget, not token volume or inferred provider compute capacity.

Upstream: https://github.com/router-for-me/CLIProxyAPI
Fork: https://github.com/dnplus/CLIProxyAPI
Baseline: `d1a024e9400bc65bd78ccd908945cf2eacc2835e` (`v7.2.156`). The upstream MIT license remains unchanged.

## Run the offline experiment

From the repository root, with Go 1.26 or newer:

```sh
go build -o quota-router ./cmd/quota-router
./quota-router -mode replay -input examples/adaptive/replay.json
./quota-router -mode replay -input examples/adaptive/replay-adverse.json
./quota-router -mode shadow -input examples/adaptive/replay.json
go test ./internal/adaptive
go test ./...
go build -o cli-proxy-api ./cmd/server
```

The fixtures contain no real accounts or credentials. Their dates are fixed so the experiments are repeatable. Offline mode uses event time. Live mode uses the current clock, so these old snapshots cannot authorize live work.

`replay` gives each policy a separate copy of the same initial state and the same time-ordered events. `shadow` produces explanations without reservations or execution. The replay baseline uses configured fixed order with pre-execution transport failover and identical admission constraints. It is a controlled simulator of that policy, not an empirical measurement of the production scheduler. Existing CPA native failover remains unchanged outside the adaptive entry point.

| Synthetic scenario | Fixed accepted | Adaptive accepted | Cost per policy |
| --- | ---: | ---: | ---: |
| Both declared-capable routes pass the external fixture acceptance oracle | 2 | 4 | 4 USD |
| Economy route fails the independent acceptance oracle | 2 | 0 | 4 USD |

Outcomes are fixture inputs independent of routing estimates. A successful HTTP response is not an acceptance oracle. Token counts do not enter the accepted-work metric. These examples demonstrate behavior, including a losing case; they do not establish real savings or model quality.

## Observation and decision contract

An observation requires `provider`, opaque `account`, `pool`, stable `window`, `at`, `source`, and `identity_evidence: account-bound`. `used_percent: null` means unknown. The producer must establish the binding from an authorized account-specific source. The string is an explicit assertion at this trusted input boundary, not cryptographic verification or a provider identity lookup.

A route binds an exact CPA `auth_id` to a provider/account/model and every limiting quota window. Models sharing a pool reference the same keys. Separate account or provider percentages are never summed. All windows must have fresh known usage and a future reset. Unknown/stale/expired observations fail closed. All routes must declare `official-api` or `permitted-api`; the live v1 adapter additionally requires a CPA API-key credential. It does not load subscription OAuth credentials.

Snapshots have a bounded history of 64 observations per configured key. At least three observations spanning a minute in the same reset segment produce an observed rate: the largest nonnegative adjacent slope. Reset changes, counter decreases and freshness gaps end a segment. This conservative short-history estimate is not TokenBar's multi-cycle curve model. A missing rate is reported as `snapshot_only`, not learned zero consumption.

The output separates:

- `reset_at`: the producer's observed future reset timestamp.
- `duration_seconds` and `duration_source`: a provider/contract period, or a learned adjacent rollover. Observed learning requires a stable old reset, samples within 15 minutes on either side, and a second confirming new-reset observation. Sliding, backward or missed resets do not manufacture a period.
- `estimated_empty_at`: a projection from current remaining quota and the observed segment rate; omitted without a positive rate or beyond the bounded 400-day projection horizon.

Admission requires declared capabilities, context capacity, a future deadline, sufficient estimated budget and sufficient capacity in every window after projected background use during the task. Unknown per-route dollar, duration or per-window consumption estimates exclude that route. A task expected to cross a reset is rejected in v1; no queue or HTTP wait is created.

Eligible routes are ordered by lower estimated dollars, then higher reset-expiring surplus measured in estimated task equivalents per second, then greater bottleneck headroom in task equivalents. No raw percentage is added across providers. Every candidate has reason codes, source, observation time and projection confidence. This deterministic ranking is deliberately small and may lose when cost/quality estimates are wrong.

A session pins route, model and credential across turns and restarts. An unavailable pinned route rejects the task. There is no mid-session failover. New task IDs are reserved once; repeating one is rejected, including after restart. The caller must submit its complete Chat Completions message/tool history; v1 does not migrate provider-owned response IDs or WebSocket state.

## Dedicated local server

Copy the policy, state and CPA configuration into a new working directory. Replace synthetic route IDs, account bindings, models and quota observations with authorized, verified inputs. Keep the input filenames outside version control.

```sh
mkdir -p work/adaptive
cp examples/adaptive/policy.json work/adaptive/policy.json
cp examples/adaptive/state.json work/adaptive/state.json
cp examples/adaptive/cpa.example.yaml work/adaptive/cpa.yaml
./quota-router -mode serve -config work/adaptive/cpa.yaml -policy work/adaptive/policy.json -state work/adaptive/state.json
```

The server binds only to explicit loopback addresses and requires a frontend API key. It exposes only `/adaptive/*`; standard model and management endpoints are blocked in this entry point. Plugins and Home must be disabled. It uses a new empty auth directory beside its state file. Do not reuse a running proxy configuration or edit this dedicated configuration while the server is running.

Endpoints, all authenticated with the configured frontend API key:

| Endpoint | Input | Effect |
| --- | --- | --- |
| `GET /adaptive/accounts` | none | Lists API-key auth IDs and provider only; never credential values. Use these IDs to prepare the policy before execution. |
| `POST /adaptive/observe` | one normalized observation | Validates and atomically persists the observation. No provider fetch. |
| `POST /adaptive/shadow` | task JSON | Explains selection without reserving or calling the upstream. |
| `POST /adaptive/execute` | `{"task":...,"request":...}` | Disabled unless the process was started with `-enable-execute`. Accepts explicit Chat Completions messages and optional streaming. |

Example request body: [`request.json`](../../examples/adaptive/request.json). Set the Authorization header with your configured client key; do not put it in committed example files. `/adaptive/observe` is an operator endpoint: clients sharing access to this local server are trusted to provide policy inputs.

Adding `-enable-execute` is the explicit live switch. It may spend real API credit. This implementation has only been exercised against local synthetic HTTP upstreams. No paid experiment, subscription reset, credit purchase or external account access was performed.

The task's cost, duration and context values are planning estimates supplied by the trusted caller. The ledger bounds **reserved estimated dollars**, not provider billing; it is not a billing enforcement system, exact tokenizer, quality classifier or hard deadline guarantee. Provider-native spend caps remain authoritative. Actual upstream usage is observed through CPA's usage observer (task ID, account, tokens, latency, TTFT, failure); it is not converted into quota or acceptance automatically.

## Persistence and recovery

The server serializes adaptive work and returns `409 router_busy` for concurrent work instead of waiting in a queue. Before execution it saves the reservation and session pin using file sync, atomic rename and directory sync. Write failure latches execution closed until restart. A policy digest prevents changing route/account bindings or the budget behind an existing ledger.

Reservations stay charged even if execution fails or is cancelled. New quota snapshots may include the same consumption; v1 deliberately retains the conservative reservation until a provider/contract period or confirmed adjacent rollover proves a new cycle. Merely changing a reset timestamp does not release it. There is no automatic refund or top-up. The 10,000 reservation/session cap rejects new work rather than evicting session pins. Dollar reservations cover the lifetime of the state file.

The exclusive `.lock` file prevents two server processes from sharing a ledger. A crash leaves it behind. Verify that its recorded PID is no longer running before removing that lock and restarting. Preserve the ledger; do not reset it to escape a quota or budget denial. Reconcile uncertain execution outcomes explicitly. v1 does not cache completed responses for idempotent response replay or automate recovery/refunds.

## TokenBar read-only assessment

Reviewed TokenBar baseline `a87ea69bb8b963ae460fe6963cd0406312c5d0eb` plus its existing local changes. No TokenBar files were modified and its live quota CLI was not invoked.

The existing `tokenbar-linux quota` calls `tb_agent_usage`, unwraps the FFI envelope and emits JSON. It is a usable data transport, but the local CLI is untracked in that checkout and is not a pinned published dependency. `AgentUsageSnapshot` deliberately skips serializing `account_scope` and `history_scope`; presentation `identity.email` and `accountKey` do not establish CPA credential ownership. `resolve_history_scope_with` uses a per-provider installation constant when authoritative identity is absent. Thus neither raw history's `accountScope` field nor provider-only pace data is a safe account-routing balance.

Use read-only inspection of an exported JSON file:

```sh
./quota-router -mode tokenbar-inspect -input /path/to/exported-quota.json
```

The inspector accepts raw CLI payload, FFI envelope or the cross-check fixture wrapper. It reports reset, period, ETA, state and timestamp separately and marks every row `routable: false`. It omits display identity and account paths. It never polls providers, reads credentials, imports history into an account or modifies TokenBar. The first version deliberately has no automatic TokenBar-to-CPA account binding.

Source paths reviewed: `AGENTS.md`; `docs/knowledge/README.md`, `architecture.md`, `verification.md`, `plans/provider-quota-pace.md`, `plans/codex-historical-pace-v2.md`; `crates/tb_core_ffi/src/agent_usage.rs`, `agent_quota_duration.rs`, `agent_quota_history.rs`, `agent_account_scope.rs`, `window_usage.rs`, `bin/tokenbar-linux.rs`; `Fixtures/CrossCheck/provider-quota-pace-v3.json`. Executable source and tests distinguish provider/contract/observed duration, require rollover confirmation, and keep post-reset zero usage in the new segment. This Go implementation uses those evidence boundaries, not copied Rust code or a claim of curve-model parity.

## Verification and next experiment

`TestNativeHTTPJourneyStreamingToolsAffinityAndRestart` uses actual localhost HTTP on both sides, CPA's native auth manager and OpenAI-compatible executor. It checks the selected API-key identity, model, tool-call ID, SSE completion, shadow non-mutation, disabled execution, restart/deduplication, unavailable pinned credentials and durable-write failure before any additional upstream call. Both credentials advertise both models, so losing the pin selects the wrong account; reverting to the upstream context behavior makes this test fail. The native usage record is correlated by task ID, selected auth ID and observed token count. This is local synthetic integration evidence, not a real provider or quality test.

A separate local test runs CPA's existing fixed-model, fill-first selection: the first credential returns 429 and CPA successfully retries the second credential. The full upstream suite and original server build are retained. The only existing runtime file changed is the OpenAI handler: it now inherits request context when constructing execution context, preserving native auth pins and the per-request model route through the existing machinery.

A future authorized real-work experiment should freeze model capability profiles, monetary caps, deadline classes and independent acceptance tests; randomize comparable tasks between fixed+native failover and adaptive routing; log account-bound quota snapshots and actual billing; compare accepted tasks completed by deadline per dollar, failures/retries, quota rejections and forecast error by confidence. Include tasks where the cheap model loses. Do not infer quality from HTTP 200, token volume or the current synthetic result.
