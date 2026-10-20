# Spynel 0.13.3 release notes

Native npm update handoffs are now published atomically and checked through the shared run-completion boundary before returning update exit code 75. Previously, a failed request-file write could still emit that code, leaving the launcher to stop with an unverified update request. Interrupted private requests can be replaced by the complete current request; unsafe files and publication failures return an explicit retained error. Reading an exit code no longer writes a request.

Process discovery reconciles the kernel PID snapshot with registration files after one bounded two-second transition window before classifying unknown processes. Instances entering or leaving the registry no longer become immediate false legacy-server failures. Live unregistered applications still fail update preflight. Native exit recorders register as diagnostic helpers before reading input and remain outside application restart targets.

This release retains 0.13.2's installed-launcher reload and serving-version status, plus the existing guarded provider cleanup and finite crash-retry policy. These update fixes do not establish the cause of an underlying executor crash.
