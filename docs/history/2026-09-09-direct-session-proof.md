# Direct session proof, 2026-09-09

This receipt separates existing session recovery from the new message-control API.
It is evidence, not an additional operating procedure.

## Retained session on the deployed baseline

The enrolled local CLI operated `https://api.dorf.run`, running Dorf 0.5.15 with the
verified Incus Codex profile. All requests below used the same direct Job,
`job-a60dca962286b46eae1e`, and Sandbox `dorf-117cd9599b2b83f524c3`.

| Check | Message | Observed result |
| --- | --- | --- |
| Initial conversation | `message-9518b2bcd3e190855bb52662` | Completed with `READY` after receiving a harmless phrase to remember |
| Worker restart during a running shell-tool turn | `message-4a55776593e09e938af74a77` | Observed running, restarted only the documented Compose worker service, then observed completion with the original phrase |
| Native process termination attempt | `message-9381757c34b90a51999c441b` | Completed normally; not counted as crash-recovery evidence |
| Native Codex crash inside the test Sandbox | `message-571d8d450a223ee2dde9fb49` | Observed terminal `interrupted`, with no replacement request |
| Conversation after the native crash | `message-dad2bcd74263d194f4c84cc8` | A new Follow on the same Job completed with the original phrase, without repeating the crash command |

The native crash test asked Codex to terminate its own native process inside this
test Sandbox. The observed interruption and successful later conversation are the
recovery evidence; agent prose claiming that a command ran is not sufficient.

These checks exercised the existing direct Job session mechanism. No memory file,
external memory service, transcript mirror, backup, or Sandbox replacement was
introduced. This does not establish outcomes for arbitrary commands interrupted
by a crash or recovery after Sandbox disk loss.

## New message-control verification

Local PostgreSQL tests cover automatic steering and follow selection, immutable
request replay after state changes, interruption through an attached Steer, and
protection of successor Turns. An Absurd task drives worker reconciliation against
PostgreSQL, rejecting foreign Turn observations and distinguishing accepted
interrupts from observed completion. Native protocol tests cover uncertain interrupt
acknowledgement and exact-target observation. HTTP integration tests cover omitted
intent, accepted Stop replay, missing targets, and a delivered Steer whose answer
is still pending. CLI tests exercise authenticated automatic messaging and Stop
through the command dispatcher.

Dorf 0.5.16 was published from `fc971cd190445d49fcb07184ce13d0c07315e48a`
after [CI passed](https://github.com/aphronio/dorf/actions/runs/34403922481).
The [release workflow](https://github.com/aphronio/dorf/actions/runs/34404230559)
verified and published the immutable release. The host and enrolled local CLI
installed the published archive through `dorf update`; the host applied its
manifest and migration through `dorf setup --yes`. The public API reported
`0.5.16` and `message_interrupt`.

All live checks used the original Job and Sandbox above, through the enrolled CLI:

| Check | Message | Observed result |
| --- | --- | --- |
| Default message while idle after deployment | `message-0da8f06d395787837fd1451a` | Resolved to Follow and completed with the original phrase |
| Start a native shell-tool turn | `message-3117084e74991b350b5101c0` | Resolved to Follow; observed running before the correction and Stop |
| Default correction during active work | `message-ed2571287432047e17bc44f5` | Resolved to Steer; native delivery completed while the answer remained pending |
| Stop through the delivered Steer | `message-ed2571287432047e17bc44f5` | API recorded `interrupt_requested=true`; the original Follow subsequently reported outcome `interrupted` |
| New turn after Stop | `message-49e5747d6e57db9905523295` | Resolved to Follow and was observed running |
| Replay old Stop and old automatic message during the successor | Original Steer above | Returned the original Message; the successor remained running with `interrupt_requested=false` |
| Reuse the old message key with a changed requested intent | Original Steer above | Rejected with HTTP 409 `idempotency_conflict` |
| Successor completion | `message-49e5747d6e57db9905523295` | Completed normally with the original phrase |

Final host `dorf doctor` checks all reported `ready`. Requested cleanup of only
this proof Job reached the public terminal cleanup state `complete`, with
admission closed. No proof Sandbox was retained.
