# Spynel 0.13.1 release notes

Harness ownership now recovers automatically after an interrupted executor or delayed provider shutdown. The permanent workspace marker is replaced by a private operating-system lock in the per-user configuration directory, away from shared workspace mounts that may not enforce file locks. Old marker files no longer block startup.

Each provider runs through a private guardian that retains ownership independently of provider descriptor handling. If the executor disappears, the guardian terminates the provider's process group, waits for the direct provider to exit, and releases ownership. A waiting successor reconnects automatically. Native supervisors can now retry executor crashes after provider admission.

Failed provider shutdown remains visibly unavailable until cleanup is confirmed, then retries automatically. Durable task recovery still reconciles interrupted work; this change does not replay tasks, erase leases, or promise cleanup of escaped processes or remote effects. Explicit shutdown failures are reported rather than silently discarded.

The release also makes the diagnostics reader-loss test wait for confirmed reader shutdown before asserting fallback output, avoiding a race with SIGKILL interrupting the reader's output forwarding.
