# Auth Quota Auto Disable Design

## Goal

Add automatic auth-file disabling and recovery for quota exhaustion in the main CLIProxyAPI service.

Scope for this design:

- Detect when an auth file should be treated as quota exhausted.
- Disable that auth file without deleting it.
- Persist enough state to retry recovery later.
- Re-enable the auth file immediately after an active probe confirms quota has recovered.

Out of scope for this design:

- Replacing or removing existing auth-file deletion and cleanup behavior.
- Building a separate cleaner service or external daemon.
- Adding recovery for providers that cannot be actively probed with acceptable confidence.

## User Requirements

- When quota is exhausted, disable the auth file.
- On a later scan, if quota is restored, cancel the disabled state.
- Keep existing deletion and cleanup behavior intact, but it does not need to be part of the feature summary.
- Automatic recovery must behave the same as manual recovery in the management panel: once restored, the auth becomes available immediately.

## Existing Constraints

- The service already exposes `management api-call`, which can make an upstream request on behalf of a selected auth file.
- The service already exposes `auth-files/status`, which toggles `disabled`.
- Disabled auth files are already excluded from normal auth selection.
- The current system already tracks some quota-exceeded behavior at the model level, but not a full auth-file disable and recovery lifecycle.
- Manual disable and enable must remain authoritative. The system must not auto-enable something the user disabled manually.

## Recommended Approach

Implement this as an in-process background scanner inside CLIProxyAPI.

Why this approach:

- It reuses the existing auth manager, watcher reload flow, and management APIs.
- State changes remain consistent with the current management panel behavior.
- Recovery can take effect immediately after the auth record is updated.
- It avoids introducing a second deployment unit and duplicate configuration surface.

## High-Level Design

The feature adds a new lifecycle for auth files:

1. The service identifies quota exhaustion for a specific auth file.
2. The auth file is marked as automatically disabled by the system.
3. A persisted recovery schedule is created for that auth file.
4. A background scanner periodically checks only automatically disabled auth files that are due for probing.
5. The scanner performs an active probe through `management api-call`.
6. If the probe no longer shows quota exhaustion, the auth file is re-enabled immediately.
7. If quota is still exhausted, the auth file remains disabled and the next probe time is pushed forward.

## State Model

The system needs an auth-level persisted marker that distinguishes system-driven disable from user-driven disable.

Required persisted fields:

- `auto_disabled_reason`
  - Expected initial value: `quota_exhausted`
- `auto_disabled_at`
- `auto_recovery_last_checked_at`
- `auto_recovery_next_check_at`
- `auto_recovery_last_result`
- `auto_recovery_probe_provider`

Recommended storage location:

- Persist in auth metadata alongside the auth record, so the state survives process restarts and follows the auth file through the existing management and watcher flows.

Rules:

- `disabled=true` alone is not enough to decide whether the scanner may auto-enable the auth.
- Auto-recovery is allowed only when `disabled=true` and `auto_disabled_reason=quota_exhausted`.
- If a user manually disables an auth file, the auto-disable metadata must be absent or cleared so the scanner never re-enables it.
- If a user manually re-enables an auto-disabled auth file, the auto-disable metadata must also be cleared.

## Quota Exhaustion Detection

Quota exhaustion should be promoted from a transient routing signal to an auth-file state transition.

Detection sources:

- Existing runtime paths that can confidently identify quota exhaustion for an auth.
- Active probe classification using the same quota-oriented response rules used for recovery checks.

First supported providers:

- `codex`
- `openai`
- `chatgpt`

Reasoning:

- These are the providers for which the current project already has the clearest active probe path through `management api-call`.

For other providers:

- Do not auto-recover in the first version unless there is a reliable active probe contract.
- Optionally log or expose that they are not eligible for automatic recovery.

## Disable Flow

When the runtime determines that a specific auth file has hit quota exhaustion:

1. Resolve the auth record.
2. Set `disabled=true`.
3. Set status to disabled with a system-generated message for quota exhaustion.
4. Write the auto-disable metadata fields.
5. Persist the auth update through the auth manager.

Expected behavior:

- The auth immediately stops participating in selection.
- The state survives restart.
- The disable source is machine-readable, not inferred from free-form text alone.

## Recovery Scanner

Add a background scanner running on a configurable interval.

Scanner responsibilities:

- Enumerate auth records.
- Select only auth files that are both:
  - currently disabled
  - marked as auto-disabled for quota exhaustion
- Skip records whose `auto_recovery_next_check_at` is still in the future.
- Probe eligible auth files one by one or in a controlled small concurrency window.

Default strategy for the first version:

- Keep concurrency conservative.
- Prefer correctness and stability over aggressive probing volume.

## Active Probe Contract

The scanner will use `management api-call` to make a provider-appropriate request with the target auth.

Probe result mapping:

- If the result still matches quota exhaustion, keep the auth disabled and schedule the next probe.
- If the result is successful or otherwise clearly no longer quota-limited, clear the auto-disable metadata and re-enable the auth immediately.
- If the result is an auth-invalid signal such as an unrecoverable authentication failure, do not silently convert this feature into deletion logic. Keep deletion and cleanup behavior on its own existing path.
- If the result is ambiguous, keep the auth disabled and retry later.

Important boundary:

- Recovery should not depend on regular business traffic.
- Recovery must depend on an explicit active probe, because the auth is disabled and will not naturally receive traffic.

## Immediate Availability After Recovery

This is a hard requirement.

Once the scanner confirms recovery and writes the auth back to enabled state:

- the auth should become selectable immediately
- the behavior should match manual enable from the management panel

This implies:

- use the same auth update path as manual status changes whenever practical
- clear the disabled status and related system-generated message in the same update
- clear auto-disable metadata in the same update so the scanner does not race and disable it again

## Manual Management Semantics

Manual management actions must remain authoritative.

Rules:

1. Manual disable wins over automatic recovery.
2. Manual enable clears any quota auto-disable markers.
3. The scanner may only act on auth files that are explicitly marked as system auto-disabled for quota exhaustion.

This prevents the system from surprising the user by re-enabling something that was disabled intentionally from the management panel.

## Configuration

Add a small dedicated config block for this feature.

Recommended fields:

- `auth-quota-auto-disable.enabled`
- `auth-quota-auto-disable.scan-interval`
- `auth-quota-auto-disable.initial-recovery-wait`
- `auth-quota-auto-disable.retry-interval`
- `auth-quota-auto-disable.max-concurrent-probes`
- `auth-quota-auto-disable.providers`

Suggested default behavior:

- Enabled by explicit config, not silently on for all deployments.
- First recovery check should wait for a configurable cooldown after the auth is disabled.
- Later retries should follow a separate retry interval.

This mirrors the reference repository behavior without requiring an external state file.

## Error Handling

If a disable update fails:

- keep the auth unchanged
- log the failure with auth identity but without secrets
- retry only when a new quota exhaustion event is observed

If a probe fails due to network or upstream instability:

- keep the auth disabled
- record the probe failure summary
- schedule the next retry

If metadata is partially missing or malformed:

- treat the auth as not eligible for auto-recovery unless it can be safely reconstructed
- never guess that a user-disabled auth was system-disabled

## Observability

Add structured logs for:

- automatic disable triggered
- recovery probe started
- recovery probe result classification
- auth automatically re-enabled
- probe deferred because next check is not due

Optional future management visibility:

- show whether an auth is user-disabled or quota-auto-disabled
- show next recovery probe time
- show last recovery result

This can be added later without changing the core lifecycle.

## Testing Strategy

Required tests:

1. Quota exhaustion marks the auth as disabled and writes auto-disable metadata.
2. Disabled auth with quota auto-disable metadata is picked up by the scanner when due.
3. Successful active probe clears metadata and re-enables the auth immediately.
4. Probe that still indicates quota exhaustion keeps the auth disabled and schedules another retry.
5. Manual disable is never auto-recovered.
6. Manual enable clears auto-disable metadata.
7. Restart persistence works: metadata survives reload and scanning resumes correctly.
8. Unsupported providers are skipped safely.

Test style:

- Prefer focused unit tests for lifecycle decisions.
- Add integration coverage where the service path must prove immediate availability after enable.

## Rollout Notes

Phase 1:

- Support `codex`, `openai`, and `chatgpt`.
- Keep the feature behind explicit config.
- Reuse existing management request paths and auth persistence.

Phase 2:

- Expose richer management UI state if needed.
- Expand to more providers only after a reliable active probe contract exists.

## Decision Summary

- Implement inside the main service, not as an external cleaner.
- Disable on confirmed quota exhaustion.
- Recover only through active probing.
- Re-enable immediately after a successful recovery probe.
- Keep manual disable and enable authoritative.
- Do not couple this feature to existing deletion behavior.
