# Configuration application matrix

Every setting below is exposed by the shared typed catalog used by the TUI, slash commands, and plain CLI. The structured route array uses JSON as its command/form value while remaining ordinary YAML on disk. All rows use the serialized `app.Service.ApplySettings` path: validate, atomically save private YAML, reload that canonical file into the shared process snapshot before returning, and notify runtime owners. Subsequent operations read the refreshed snapshot; minimal direct hooks refresh cached orchestrator controls. Active channels and in-flight work are preserved while their supervisors consume the new snapshot. The three extension rows are the sole restart exception.

| Setting | Prior behavior | Runtime owner and final effect | Validation / application | Verification boundary |
| --- | --- | --- | --- | --- |
| `harness.name` | Live, idle-only | Harness supervisor replaces the adapter before commit | Old harness restored if persistence fails | harness supervisor and service tests |
| `harness.model` | Live, idle-only | Harness supervisor applies the next-session model before commit | Old harness restored if persistence fails | harness/model service tests |
| `harness.sandbox` | Live, idle-only | Harness supervisor applies provider policy before commit | Old harness restored if persistence fails | harness policy tests |
| `harness.reviews` | Live | Application and orchestrator read the accepted policy for the next decision | Validation/persistence is all-or-nothing | review-policy tests |
| `harness.chat_agent_prefix` | Live | Application snapshots it immediately before chat dispatch | Validation/persistence is all-or-nothing | prompt-prefix tests |
| `harness.developer_agent_prefix` | Live | Orchestrator snapshots it immediately before implementation/planning dispatch | Validation/persistence is all-or-nothing | orchestration prompt tests |
| `harness.reviewer_agent_prefix` | Live | Orchestrator snapshots it immediately before review dispatch | Validation/persistence is all-or-nothing | orchestration prompt tests |
| `harness.heartbeat_agent_prefix` | Live | Heartbeat snapshots it immediately before audit dispatch | Validation/persistence is all-or-nothing | heartbeat prompt tests |
| `harness.acp_command` | Live, idle-only | Harness supervisor replaces custom ACP before commit | Old harness restored if persistence fails | ACP configuration tests |
| `harness.acp_args` | Live, idle-only | Harness supervisor replaces custom ACP arguments before commit | Old harness restored if persistence fails | ACP argument tests |
| `workspace.history_max_messages` | Live | Prompt construction reads the newest bounded-history count | Validation/persistence is all-or-nothing | history prompt-limit tests |
| `workspace.history_char_limit` | Live | Prompt construction reads the newest character limit | Validation/persistence is all-or-nothing | history prompt-limit tests |
| `workspace.attachment_max_mb` | Live | Channel generation fingerprint and outbound parsing use the accepted limit | Invalid values reject before commit; stale adapters are revoked | channel/media limit tests |
| `startup.enabled` | Live | Startup manager registers or removes the workspace service after the shared snapshot reload | Invalid values reject before save; OS registration errors are returned | startup application tests |
| `channels.tui.title` | Live | Shared-state title publication updates attached TUIs | Persistence failure publishes nothing | service title tests |
| `channels.tui.theme` | Live | Palette validates before commit, then publishes to attached TUIs | Unknown/invalid palette rejects before commit | theme service and visual tests |
| `orchestrator.enabled` | Live | Manager fences scheduler state and requests an immediate scan when enabled | Infallible generation publication follows durable commit | heartbeat scheduler tests |
| `orchestrator.interval_seconds` | Restart-bound | Manager now resets its route timer from acceptance; scans remain serialized | Infallible generation publication follows durable commit | live interval wake test |
| `orchestrator.semantic_heartbeat_minutes` | Live | Manager fences the old heartbeat term and schedules from acceptance | Infallible generation publication follows durable commit | heartbeat reschedule/race tests |
| `orchestrator.task_notifications` | Live | Pending work stays dormant for `off`; `decide`/`always` is captured when its single invocation is claimed and survives later changes between those modes; later `off` revokes new delivery but permits audit | Durable invocation state prevents harness replay; outbox IDs deduplicate delivery retries | live pre/post-admission mode-transition and notification tests |
| `orchestrator.max_parallel` | Restart-bound | Resizable gate raises promptly; lowering preserves active jobs and blocks new claims until below bound | Infallible generation publication follows durable commit | live capacity test and race suite |
| `orchestrator.routes` | Restart-bound and absent from the catalog | Serialized manager snapshot supplies the next scan, claim, recovery, status, heartbeat inventory, and communication creation prompt; an admitted job retains the complete route generation, an active goal retains its admitted task route, and a pending notification retains its producing task route | Complete-array validation rejects before commit; acceptance waits for an active scan's admission boundary | structured-route next-scan, live creation-prompt, in-flight goal-cohort, and pending-notification tests |
| `extensions.enabled` | Restart-bound | Process-start hook runner snapshot; takes effect after restart | Typed validation and atomic persistence only | catalog restart-bound test |
| `extensions.directory` | Restart-bound | Process-start trusted extension root; takes effect after restart | Typed validation and atomic persistence only | extension discovery tests |
| `extensions.hook_timeout` | Restart-bound | Process-start per-hook timeout; takes effect after restart | Duration validation and atomic persistence only | extension timeout tests |
| `channels.telegram.token` | Live | Channel supervisor replaces only Telegram; secrets remain masked | Invalid complete config rejects before commit; stale generation is revoked | channel supervisor/security tests |
| `channels.telegram.allowed_users` | Live | Adapter also resolves the newest list at every authorization boundary | Invalid/revoked access fails closed; stale generation is revoked | Telegram authorization tests |
| `channels.telegram.enabled` | Live | Channel supervisor starts or stops Telegram | Invalid enablement rejects before commit | supervisor lifecycle tests |
| `channels.telegram.name` | Live | Replacement Telegram generation uses the new display name | Stale generation cannot publish status | Telegram configuration tests |
| `channels.telegram.token_env` | Live | Replacement resolves the new environment reference | Missing token fails closed | Telegram preflight tests |
| `channels.telegram.mode` | Live | Replacement switches polling/webhook mode | Webhook prerequisites validate before commit | Telegram webhook tests |
| `channels.telegram.webhook_url` | Live | Replacement uses the new public webhook URL | Complete webhook config validates before commit | Telegram webhook tests |
| `channels.telegram.webhook_listen` | Live | Channel supervisor consumes the refreshed snapshot and replaces Telegram | Invalid values reject before save; adapter failures remain isolated | Telegram webhook and supervisor tests |
| `channels.telegram.webhook_secret` | Live | Replacement uses the new verification secret | Missing secret rejects webhook mode before commit | Telegram webhook security tests |
| `channels.telegram.poll_timeout_seconds` | Live | Replacement polling generation uses the new timeout | Range validation rejects before commit | Telegram polling tests |
| `channels.telegram.group_mode` | Live | Replacement and delivery authorization use the new group policy | Validation/persistence is all-or-nothing | Telegram group tests |
| `channels.telegram.welcome_enabled` | Live | Replacement reads the new welcome policy | Validation/persistence is all-or-nothing | Telegram welcome tests |
| `channels.telegram.welcome_message` | Live | Replacement reads the new template | Validation/persistence is all-or-nothing | Telegram welcome tests |
| `channels.telegram.notify_messages` | Live | Replacement emits notices under the new policy | Stale adapter status/notices are fenced | channel notice tests |
| `channels.telegram.attachment_max_age_hours` | Live | Replacement cleanup policy uses the new retention | Range validation rejects before commit | Telegram attachment tests |
| `channels.whatsapp.mode` | Live | Channel supervisor replaces only WhatsApp | Invalid complete config rejects before commit; stale generation is revoked | WhatsApp mode tests |
| `channels.whatsapp.allowed_numbers` | Live | Adapter resolves the newest canonical list at every authorization boundary | Invalid/revoked access fails closed | WhatsApp authorization tests |
| `channels.whatsapp.enabled` | Live | Channel supervisor starts or stops WhatsApp | Invalid enablement rejects before commit | supervisor lifecycle tests |
| `channels.whatsapp.database` | Live | Channel supervisor consumes the refreshed snapshot and opens the selected workspace-relative session database | Invalid values reject before save; adapter failures remain isolated | WhatsApp configuration and supervisor tests |
| `channels.whatsapp.allow_groups` | Live | Replacement and delivery authorization use the new group policy | Validation/persistence is all-or-nothing | WhatsApp group tests |
| `channels.whatsapp.poll_interval_seconds` | Live | Replacement health loop uses the new interval | Range validation rejects before commit | WhatsApp polling tests |
| `speech.enabled` | Live | Channel fingerprint replaces adapters; the next voice event uses the new policy | Stale adapters are revoked | speech/channel tests |
| `speech.language` | Live | Replacement transcriber selection uses the new language | Catalog validation rejects before commit | speech language tests |
| `speech.model_dir` | Live | Replacement uses the new explicit model root on the next transcription | Invalid runtime model use returns a visible event error | speech model tests |
| `speech.num_threads` | Live | Replacement transcriber snapshot uses the new worker count | Range validation rejects before commit | speech configuration tests |
| `speech.max_file_mb` | Live | Replacement rejects subsequent oversized voice files at the new bound | Range validation rejects before commit | speech size-limit tests |
| `speech.max_duration_seconds` | Live | Replacement bounds the next transcription duration | Range validation rejects before commit | speech duration tests |
| `speech.chunk_seconds` | Live | Replacement chunks the next transcription at the new duration | Range validation rejects before commit | speech chunk tests |

`channels.tui.enabled` is intentionally absent. Legacy YAML containing it is normalized during load and a canonical save removes it. Bare `spynel` launches the TUI; `spynel serve` remains headless unless invoked with `--tui`.
