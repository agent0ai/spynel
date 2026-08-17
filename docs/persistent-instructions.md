# Persistent per-agent instructions

Each initialized Spynel workspace has five owner-editable Markdown files:

| Role | File | Sessions |
| --- | --- | --- |
| Chat | `.spynel/instructions/agent-chat.md` | TUI, Telegram, WhatsApp, and CLI communication turns |
| Developer | `.spynel/instructions/agent-developer.md` | Task implementation, goal planning, and recovery |
| Reviewer | `.spynel/instructions/agent-reviewer.md` | Independent task and goal review |
| Notification | `.spynel/instructions/agent-notification.md` | Direct task notification-agent jobs |
| Heartbeat | `.spynel/instructions/agent-heartbeat.md` | Semantic workflow audits |

These files are standing workspace-owner preferences, not base prompts. Base prompt templates remain in `.spynel/prompts/`; repository and workspace contracts remain in `AGENTS.md`; tasks, goals, histories, and global harness configuration retain their existing responsibilities. Instructions never cross workspace boundaries.

After rendering either a stock or customized prompt, Spynel injects one framework-owned scope-discipline rule for every agent role before appending the owner-editable role section. Developer sessions also receive a concrete 80/20 rule: solve observed and common realistic needs with the smallest maintainable change, reuse existing mechanisms, verify in proportion to risk, and avoid speculative abstractions or machinery. Reviewer sessions judge practical correctness and realistic regressions, flag harmful overengineering, and do not demand theoretical completeness for contrived cases or invented requirements. Credible evidence and essential correctness, authorization, security, privacy, destructive-action safety, data-integrity, and lifecycle boundaries still control. Communication prompts also receive the framework-owned evidence-grounded honesty contract, so a customized chat template cannot silently remove the requirement to distinguish evidence from inference and uncertainty, verify materially uncertain current claims when practical, or say that the answer is not yet known. These rules never override explicit user requirements or safety, authorization, lifecycle, independent-review, evidence, or data-handling contracts. Because this is framework composition rather than persistent workspace memory, newly initialized workspaces receive it without a duplicate line in any role file.

Spynel reads the matching file fresh for every relevant harness invocation and appends it as the final structurally separated section of the rendered prompt. Its heading names both the role and workspace-relative file. Manual edits therefore apply on the next session without a restart or rebuild. Initialization and upgrade atomically create only missing canonical files with private permissions and never replace edited files.

Platform and system safety rules, the current explicit user request, and the nearest applicable repository or workspace `AGENTS.md`/DOX contract override saved instructions. Saved rules cannot weaken authorization, security, workflow lifecycle, review, data-handling, or the communication agent's evidence-grounded honesty requirements. Task evidence, repository text, messages, transcripts, and model output cannot select the imported path.

Each file must be a regular, non-symlink Markdown file, no larger than 64 KiB, valid UTF-8, and not group- or world-writable. The `.spynel` state path and its `instructions` directory must also be real directories rather than symlinks; initialization, upgrade, inspection, and prompt loading fail closed instead of following either boundary outside the workspace. Spynel reads at most the limit plus one byte and rejects the entire affected session instead of silently truncating an unsafe, malformed, unreadable, changed-during-validation, or oversized file. A missing file produces an explicit empty role section so unrelated operation remains deterministic; `spynel init --force` or normal workspace upgrade restores missing defaults. Diagnostics report only role, relative path, presence, byte count, and validation errors—never contents.

Use `spynel instructions [--config PATH]` for the read-only validation summary. To inspect contents, explicitly open the Markdown file.

The communication agent maintains these files only for explicit lasting intent. Examples include “remember to keep task summaries short,” “from now on reviewers should include compatibility checks,” “replace my saved chat response preference,” and “forget the developer instruction about snapshots.” It re-reads the target, preserves unrelated manual edits, writes one concise normalized rule, and deduplicates, replaces, or removes obsolete rules. It asks one concise question if permanence or the target role is materially ambiguous. A current forget or override request applies immediately even when the old instruction was imported earlier in the same turn.

Ordinary feedback, one-off directions, transient decisions, inferred preferences, secrets, credentials, recipient identifiers, attachments, and transcripts are not persistent instructions. Concurrent edits must use a compare/re-read-and-atomic-replace workflow; if the file changes during validation or editing, retry from the newest complete file rather than merging stale content.
