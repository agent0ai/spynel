# Spynel 0.13.0 release notes

Manual standalone and npm runtime launches now use one native supervisor and a private executor from the same binary. The executor owns the UI, primary election, channels and agents. The parent waits for its child, restores terminal state after confirmed death, retains its observed exit and bounded fatal stderr, and permits three cancellable retries for admitted unexpected failures when provider ownership is safe.

Quit, exiting Ctrl+C, explicit stop, startup/configuration failures and update teardown suppress crash recovery. Ctrl+C copying, input clearing and turn cancellation retain their existing behavior. One-shot commands execute once. Generated services use their external restart policy through `--automatic-startup`, while Node retains npm package replacement and explicit update coordination.

Updates discover stable supervisors even without an executor, replace the supervisor image through the current installed entry, and require new verified executor generation/version/readiness. Shared readiness now spans the 30-second primary election fence within a 40-second budget. Older npm launchers require a one-time stop and relaunch before coordinated updates.

Provider admission conservatively suppresses crash retry and creates a private workspace ownership fence. Abrupt executor loss or uncertain provider shutdown prevents every successor from starting another provider until prior work and effects are inspected and reconciled. Ordinary owned shutdown joins direct providers before releasing that fence. This avoids replay over uncertain orphan work; it does not guarantee that arbitrary descendants or remote effects ended.

Go/runtime/native stderr, recovered TUI panic causes and actual native/executor exit results use the existing private redacted log store and retention bounds. Missing clean end records remain unclean or unknown termination. See [troubleshooting](troubleshooting.md#native-recovery-and-provider-ownership) for ownership reconciliation and [process diagnostics](troubleshooting.md#process-failures-and-missing-stop-records) for retained evidence.

Exit supervision does not detect frozen children or guarantee survival after parent, host, container or resource loss. These changes do not reproduce or establish the cause of the previously unexplained incident. macOS and Linux amd64/arm64 remain the four native distribution targets with their companion libraries and licenses.
