# Spynel 0.14.1 release notes

Fixes startup after first launching Spynel without an installed harness. Installing the selected executable changed its resolved command, while the retained worker still held the unavailable selection. Startup rejected that difference before opening the configuration interface.

The primary now reconciles its saved selection through the worker’s existing authenticated settings transaction. It also retries an unavailable adapter when its command has not changed, covering custom ACP executables installed at the same path. Correcting an unavailable harness selection can use the same path. A failed initial harness handshake remains visibly unavailable while the configuration interface stays reachable; correcting the executable and relaunching retries it.

The worker keeps its native identity, original provider client version and ownership checks, including when retained from 0.14.0. Recovery dispatches no provider turn. Retained executions still block structural replacement, and unknown process identity, foreign environments and incompatible protocols remain protected. Harness installation, authentication and a valid saved selection are still required.
