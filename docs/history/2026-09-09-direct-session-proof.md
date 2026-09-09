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

## New message-control implementation

Local PostgreSQL tests cover automatic steering and follow selection, immutable
request replay after state changes, interruption through an attached Steer, and
protection of successor Turns. Native protocol tests cover uncertain interrupt
acknowledgement and exact-target observation. HTTP integration tests cover omitted
intent, accepted Stop replay, missing targets, and a delivered Steer whose answer
is still pending. CLI tests exercise authenticated automatic messaging and Stop.

The deployed API proof for the new default and Stop operation is pending the
0.5.16 deployment. The test Job remains open for that proof; cleanup is pending.
