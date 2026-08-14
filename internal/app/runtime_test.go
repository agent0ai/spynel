package app

import (
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/agent0ai/spynel/internal/core"
	"github.com/agent0ai/spynel/internal/markdown"
	charmansi "github.com/charmbracelet/x/ansi"
)

func TestRuntimeWriterCapturesLinesAndPublishesLatestCounts(t *testing.T) {
	runtime := NewRuntime()
	if _, err := runtime.Write([]byte("Spynel sta")); err != nil {
		t.Fatal(err)
	}
	if len(runtime.Logs()) != 0 {
		t.Fatal("partial line was published before completion")
	}
	if _, err := runtime.Write([]byte("rted\nsecond entry\n")); err != nil {
		t.Fatal(err)
	}
	runtime.Log("third entry")

	logs := runtime.Logs()
	if len(logs) != 3 || logs[0].Text != "Spynel started" || logs[2].Text != "third entry" {
		t.Fatalf("logs = %#v", logs)
	}
	if output := formatLogs(logs); !strings.Contains(output, "3 entries") || !strings.Contains(output, "Spynel started") {
		t.Fatalf("formatted logs = %s", output)
	}
	select {
	case status := <-runtime.Updates():
		if status.Logs != 3 || status.Jobs != 0 {
			t.Fatalf("latest status = %#v", status)
		}
	default:
		t.Fatal("runtime did not publish counts")
	}
}

func TestRuntimeClearLogsDropsEntriesAndPartialOutput(t *testing.T) {
	runtime := NewRuntime()
	runtime.Log("first")
	attributed := runtime.Writer("fixture")
	if _, err := runtime.Write([]byte("unfinished")); err != nil {
		t.Fatal(err)
	}
	if _, err := attributed.Write([]byte("also unfinished")); err != nil {
		t.Fatal(err)
	}
	count, err := runtime.ClearLogsResult()
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("cleared count = %d, want 1", count)
	}
	if len(runtime.Logs()) != 0 || runtime.Status().Logs != 0 {
		t.Fatalf("logs remain after clear: %#v", runtime.Logs())
	}
	if _, err := runtime.Write([]byte("new\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := attributed.Write([]byte("attributed\n")); err != nil {
		t.Fatal(err)
	}
	logs := runtime.Logs()
	if len(logs) != 2 || logs[0].Text != "new" || logs[1].Text != "attributed" {
		t.Fatalf("partial output survived clear: %#v", logs)
	}
	select {
	case status := <-runtime.Updates():
		if status.Logs != 2 {
			t.Fatalf("latest status = %#v", status)
		}
	default:
		t.Fatal("runtime did not publish latest count")
	}
}

func TestRuntimeReusesSessionJobAndKeepsNumericOrder(t *testing.T) {
	runtime := NewRuntime()
	first := runtime.BeginJob("one", "tui", "local", "first job")
	if duplicate := runtime.BeginJob("one", "tui", "local", "duplicate"); duplicate != first {
		t.Fatalf("duplicate session job ID = %d, want %d", duplicate, first)
	}
	second := runtime.BeginJob("two", "telegram", "42", "second job")
	if first != 1 || second != 2 {
		t.Fatalf("job IDs = %d, %d", first, second)
	}
	jobs := runtime.Jobs()
	if len(jobs) != 2 || jobs[0].ID != 1 || jobs[1].ID != 2 {
		t.Fatalf("jobs = %#v", jobs)
	}
	runtime.EndJob(first)
	if status := runtime.Status(); status.Jobs != 1 {
		t.Fatalf("status after completion = %#v", status)
	}
	started, finished := 0, 0
	for _, entry := range runtime.Logs() {
		if entry.Component == "jobs" && entry.Event == "job_started" {
			started++
		}
		if entry.Component == "jobs" && entry.Event == "job_finished" {
			finished++
		}
		if strings.Contains(entry.Text, "first job") || strings.Contains(entry.Text, "duplicate") {
			t.Fatalf("job description leaked into lifecycle evidence: %#v", entry)
		}
	}
	if started != 2 || finished != 1 {
		t.Fatalf("job lifecycle events = started %d, finished %d; logs=%#v", started, finished, runtime.Logs())
	}
}

func TestRuntimeProjectsOnlyAuthoritativeLiveBackgroundJobs(t *testing.T) {
	runtime := NewRuntime()
	chat := runtime.BeginJob("chat:tui:local", "tui", "local", "foreground")
	first := runtime.BeginJobWithDetails("task:first", "orchestrator", "markdown", "first", JobDetails{Kind: "task"})
	second := runtime.BeginJobWithDetails("task:second", "orchestrator", "markdown", "second", JobDetails{Kind: "task"})
	if status := runtime.Status(); status.Jobs != 3 || status.LiveBackgroundJobs != 2 {
		t.Fatalf("initial runtime projection = %#v, want 3 registered and 2 live background jobs", status)
	}

	runtime.UpdateJob(first, core.ExecutionStatus{State: string(JobAwaitingTransition)})
	if status := runtime.Status(); status.LiveBackgroundJobs != 1 {
		t.Fatalf("one settled background job projection = %#v, want one live", status)
	}
	runtime.UpdateJob(second, core.ExecutionStatus{State: string(JobStalled)})
	if status := runtime.Status(); status.LiveBackgroundJobs != 0 {
		t.Fatalf("explicitly stale job kept background activity alive: %#v", status)
	}

	for _, state := range []JobExecutionState{JobCancelling, JobFinishing, JobError} {
		id := runtime.BeginJobWithDetails("terminal:"+string(state), "orchestrator", "markdown", string(state), JobDetails{Kind: "task"})
		runtime.UpdateJob(id, core.ExecutionStatus{State: string(state)})
		if status := runtime.Status(); status.LiveBackgroundJobs != 0 {
			t.Fatalf("terminal state %q kept background activity alive: %#v", state, status)
		}
	}
	if status := runtime.Status(); status.Jobs != 6 {
		t.Fatalf("registered terminal records were unexpectedly removed: %#v", status)
	}
	runtime.EndJob(chat)
}

func TestDurableTimingSurvivesContinuationAndJobNumberReplacement(t *testing.T) {
	now := time.Date(2026, 8, 8, 12, 27, 0, 0, time.UTC)
	first := now.Add(-3*time.Hour - 27*time.Minute)
	runtime := NewRuntime()
	runtime.Now = func() time.Time { return now }
	id := runtime.BeginJobWithDetails("durable", "orchestrator", "markdown", "task.md", JobDetails{
		Kind: "task", Route: "tasks", DurableFile: "/private/task.md",
		FirstAssignedAt: first, ProviderIterations: 1, ImplementationAttempts: 4,
	})
	if continued := runtime.BeginJobWithDetails("durable", "orchestrator", "markdown", "task.md", JobDetails{
		Kind: "task", Route: "tasks", DurableFile: "/private/task.md",
		FirstAssignedAt: first, ProviderIterations: 2, ImplementationAttempts: 3,
	}); continued != id {
		t.Fatalf("continuation job ID = %d, want %d", continued, id)
	}
	runtime.UpdateJobDurableTiming(id, first.Add(time.Hour), 1)
	if output := formatJobsAt(runtime.Jobs(), now); !strings.Contains(output, "3h27m 2▶ 4↻ · starting · orchestrator") {
		t.Fatalf("durable continuation timing = %s", output)
	}
	runtime.EndJob(id)
	now = now.Add(5 * time.Minute)
	replacement := runtime.BeginJobWithDetails("durable", "orchestrator", "markdown", "task.md", JobDetails{
		Kind: "task", Route: "tasks", DurableFile: "/private/task.md",
		FirstAssignedAt: first, ProviderIterations: 3, ImplementationAttempts: 5,
	})
	if replacement == id {
		t.Fatalf("replacement reused job ID %d", id)
	}
	if output := formatJobsAt(runtime.Jobs(), now); !strings.Contains(output, "3h32m 3▶ 5↻") {
		t.Fatalf("replacement durable timing = %s", output)
	}
}

func TestShortDurationUsesCompactStableUnits(t *testing.T) {
	cases := map[time.Duration]string{
		48 * time.Second:               "48s",
		7*time.Minute + 59*time.Second: "7m",
		time.Hour + 4*time.Minute:      "1h04m",
		51 * time.Hour:                 "2d3h",
		-time.Minute:                   "0s",
	}
	for duration, want := range cases {
		if got := shortDuration(duration); got != want {
			t.Errorf("shortDuration(%s) = %q, want %q", duration, got, want)
		}
	}
}

func TestJobExecutionStateMachineRejectsLateAndStaleUpdates(t *testing.T) {
	runtime := NewRuntime()
	id := runtime.BeginJob("one", "tui", "local", "work")
	if job, _ := runtime.Job(id); job.Execution != JobStarting {
		t.Fatalf("initial job = %#v", job)
	}
	runtime.UpdateJob(id, core.ExecutionStatus{State: "running"})
	runtime.UpdateJob(id, core.ExecutionStatus{State: "reconnecting", ReconnectAttempt: 2, ReconnectTotal: 5, Detail: "connection lost"})
	job, _ := runtime.Job(id)
	if job.Execution != JobReconnecting || job.ReconnectAttempt != 2 || job.ReconnectTotal != 5 {
		t.Fatalf("reconnecting job = %#v", job)
	}
	runtime.UpdateJob(id, core.ExecutionStatus{State: "running"})
	runningAt := time.Now().UTC().Add(time.Second)
	runtime.UpdateJob(id, core.ExecutionStatus{State: "running", At: runningAt})
	current, _ := runtime.Job(id)
	runtime.UpdateJob(id, core.ExecutionStatus{State: "reconnecting", At: current.StateChangedAt.Add(-time.Second), ReconnectAttempt: 9})
	if job, _ := runtime.Job(id); job.Execution != JobRunning || job.ReconnectAttempt != 0 {
		t.Fatalf("stale nonterminal event regressed state: %#v", job)
	}
	runtime.UpdateJob(id, core.ExecutionStatus{State: "finishing", At: runningAt.Add(time.Second)})
	runtime.UpdateJob(id, core.ExecutionStatus{State: "running", At: runningAt.Add(2 * time.Second)})
	job, _ = runtime.Job(id)
	if job.Execution != JobFinishing {
		t.Fatalf("late running event regressed terminal state: %#v", job)
	}
	runtime.EndJob(id)
	if runtime.UpdateJob(id, core.ExecutionStatus{State: "error"}) {
		t.Fatal("finished job accepted a stale update")
	}
}

func TestJobExecutionCancellationFailureAndReconnectExhaustion(t *testing.T) {
	runtime := NewRuntime()
	cancelID := runtime.BeginJob("cancel", "tui", "local", "cancel")
	runtime.UpdateJob(cancelID, core.ExecutionStatus{State: "cancelling"})
	runtime.UpdateJob(cancelID, core.ExecutionStatus{State: "running"})
	runtime.UpdateJob(cancelID, core.ExecutionStatus{State: "finishing"})
	if job, _ := runtime.Job(cancelID); job.Execution != JobCancelling {
		t.Fatalf("cancelling regressed: %#v", job)
	}
	failID := runtime.BeginJob("failure", "cli", "build", "fail")
	runtime.UpdateJob(failID, core.ExecutionStatus{State: "reconnecting", ReconnectAttempt: 5, ReconnectTotal: 5})
	runtime.UpdateJob(failID, core.ExecutionStatus{State: "error", Detail: "at /private/work/file.go api_key=secret"})
	job, _ := runtime.Job(failID)
	if job.Execution != JobError || strings.Contains(job.StatusDetail, "secret") || strings.Contains(job.StatusDetail, "/private/") || !strings.Contains(job.StatusDetail, "[REDACTED]") || !strings.Contains(job.StatusDetail, "[PATH]") {
		t.Fatalf("failed job detail/state = %#v", job)
	}
}

func TestConcurrentCancellationRollbackPreservesAcceptedReservation(t *testing.T) {
	runtime := NewRuntime()
	id := runtime.BeginJob("cancel", "tui", "local", "cancel")
	runtime.UpdateJob(id, core.ExecutionStatus{State: "running"})
	first, ok := runtime.ReserveJobCancellation(id)
	if !ok {
		t.Fatal("first cancellation reservation failed")
	}
	second, ok := runtime.ReserveJobCancellation(id)
	if !ok {
		t.Fatal("second cancellation reservation failed")
	}
	if runtime.RestoreJobAfterFailedCancellation(second) {
		t.Fatal("one failed request restored state while another cancellation remained reserved")
	}
	if current, live := runtime.Job(id); !live || current.Execution != JobCancelling {
		t.Fatalf("accepted concurrent cancellation was rolled back: %#v, live=%t", current, live)
	}
	runtime.EndJob(id)
	if runtime.RestoreJobAfterFailedCancellation(first) {
		t.Fatal("finished job restored a stale cancellation reservation")
	}
}

func TestJobHealthRequiresStructuredStallEvidence(t *testing.T) {
	runtime := NewRuntime()
	id := runtime.BeginJob("health", "tui", "local", "long tool operation")
	runtime.UpdateJob(id, core.ExecutionStatus{State: "running"})
	job, _ := runtime.Job(id)
	if job.Health != JobHealthHealthy {
		t.Fatalf("running job health = %q, want healthy", job.Health)
	}

	// Last-activity age is diagnostic evidence, not proof that a provider or
	// its active tool has stalled. Rendering must not reclassify this snapshot.
	job.LastActivityAt = time.Now().UTC().Add(-24 * time.Hour)
	if got := formatJobHealth(job); got != "healthy" {
		t.Fatalf("quiet long-running job health = %q, want healthy", got)
	}

	runtime.UpdateJob(id, core.ExecutionStatus{State: "stalled", Detail: "provider watchdog expired"})
	job, _ = runtime.Job(id)
	if job.Execution != JobStalled || job.Health != JobHealthStalled {
		t.Fatalf("explicit stalled signal = %#v", job)
	}
	runtime.UpdateJobFromLease(id, "processing", "implementation", "", time.Now().UTC(), 0)
	job, _ = runtime.Job(id)
	if job.Execution != JobStalled || job.Health != JobHealthStalled {
		t.Fatalf("generic processing lease cleared provider stall = %#v", job)
	}
	runtime.UpdateJob(id, core.ExecutionStatus{State: "running"})
	job, _ = runtime.Job(id)
	if job.Execution != JobRunning || job.Health != JobHealthHealthy {
		t.Fatalf("structured recovery from stall = %#v", job)
	}
	runtime.UpdateJob(id, core.ExecutionStatus{State: "future-provider-state"})
	job, _ = runtime.Job(id)
	if job.Execution != JobDegraded || job.Health != JobHealthDegraded {
		t.Fatalf("unknown structured state = %#v", job)
	}
}

func TestFallbackRunningTransitionCannotOverwriteReconnect(t *testing.T) {
	runtime := NewRuntime()
	id := runtime.BeginJob("fallback", "tui", "local", "work")
	runtime.UpdateJob(id, core.ExecutionStatus{State: "reconnecting", ReconnectAttempt: 1})
	if runtime.SetJobRunningIfStarting(id) {
		t.Fatal("fallback transition overwrote reconnect")
	}
	if job, _ := runtime.Job(id); job.Execution != JobReconnecting {
		t.Fatalf("job = %#v", job)
	}
}

func TestOrchestratorReconnectRestorationAndAwaitingTransition(t *testing.T) {
	runtime := NewRuntime()
	id := runtime.BeginJob("orchestrator", "orchestrator", "markdown", "task.md")
	runtime.UpdateJobFromLease(id, "processing", "implementation", "", time.Now().UTC(), 0)
	runtime.UpdateJob(id, core.ExecutionStatus{State: "reconnecting", ReconnectAttempt: 1, ReconnectTotal: 3})
	runtime.UpdateJobFromLease(id, "processing", "implementation", "", time.Now().UTC(), 0)
	if job, _ := runtime.Job(id); job.Execution != JobReconnecting {
		t.Fatalf("lease falsely restored connection: %#v", job)
	}
	runtime.UpdateJob(id, core.ExecutionStatus{State: "running"})
	if job, _ := runtime.Job(id); job.Execution != JobRunning {
		t.Fatalf("provider restoration not applied: %#v", job)
	}
	runtime.UpdateJobFromLease(id, "awaiting_transition", "implementation", "", time.Now().UTC(), 0)
	if job, _ := runtime.Job(id); job.Execution != JobAwaitingTransition {
		t.Fatalf("awaiting transition not retained: %#v", job)
	}
	runtime.UpdateJob(id, core.ExecutionStatus{State: "running"})
	if job, _ := runtime.Job(id); job.Execution != JobAwaitingTransition {
		t.Fatalf("late provider event resumed awaiting transition: %#v", job)
	}
	runtime.UpdateJobFromLease(id, "processing", "implementation", "", time.Now().UTC().Add(time.Second), 0)
	if job, _ := runtime.Job(id); job.Execution != JobRunning {
		t.Fatalf("fresh continuation lease did not resume transition: %#v", job)
	}
}

func TestJobStatusDetailRedactsCrossPlatformAbsolutePaths(t *testing.T) {
	for _, path := range []string{`/private/work/file.go`, `C:\private\file.go`, `\\server\share\secret.txt`, `\\?\C:\private\secret.txt`} {
		got := boundJobStatusDetail("open " + path)
		if strings.Contains(got, path) || !strings.Contains(got, "[PATH]") {
			t.Fatalf("path %q not redacted: %q", path, got)
		}
	}
}

func TestLeaseStatesMapToCanonicalExecutionStates(t *testing.T) {
	tests := map[string]JobExecutionState{
		"claiming": JobStarting, "processing": JobRunning, "recovering": JobRecovering,
		"awaiting_transition": JobAwaitingTransition, "hook_cancelled": JobCancelling,
		"error": JobError, "audit": JobAudit, "future-state": JobDegraded,
	}
	for lease, want := range tests {
		if got := executionFromLeaseState(lease); got != want {
			t.Fatalf("lease %q = %q, want %q", lease, got, want)
		}
	}
}

func TestLeaseUpdatePublishesOneInternallyConsistentSnapshot(t *testing.T) {
	runtime := NewRuntime()
	id := runtime.BeginJob("lease", "orchestrator", "markdown", "task.md")
	for index := 0; index < 100; index++ {
		runtime.UpdateJobFromLease(id, "recovering", "task_implementation", "", time.Now().UTC(), 4)
		job, _ := runtime.Job(id)
		if job.Execution != JobRecovering || job.LeaseState != "recovering" || job.LeasePhase != "task_implementation" || job.RecoveryCount != 4 {
			t.Fatalf("inconsistent lease snapshot: %#v", job)
		}
		runtime.UpdateJobFromLease(id, "processing", "task_review", "", time.Now().UTC(), 0)
		job, _ = runtime.Job(id)
		if job.Execution != JobRunning || job.LeaseState != "processing" || job.LeasePhase != "task_review" || job.RecoveryCount != 0 {
			t.Fatalf("inconsistent processing snapshot: %#v", job)
		}
	}
	job, _ := runtime.Job(id)
	newestHeartbeat := job.LeaseHeartbeatAt
	runtime.UpdateJobFromLease(id, "recovering", "stale_phase", "stale error", newestHeartbeat.Add(-time.Second), 99)
	job, _ = runtime.Job(id)
	if job.LeaseState != "processing" || job.LeasePhase != "task_review" || job.RecoveryCount != 0 || !job.LeaseHeartbeatAt.Equal(newestHeartbeat) {
		t.Fatalf("late lease update mutated canonical snapshot: %#v", job)
	}
}

func TestJobsUseStableTwoLineMarkdownForEveryOrigin(t *testing.T) {
	now := time.Date(2026, 8, 8, 9, 0, 0, 0, time.UTC)
	jobs := []Job{
		{ID: 1, Description: "hello *world*\x1b[31m", Channel: "tui", Conversation: "local-a", StartedAt: now.Add(-time.Minute), Execution: JobRunning},
		{ID: 2, Description: "cli work", Channel: "cli", Conversation: "build", StartedAt: now.Add(-2 * time.Minute), Execution: JobStarting},
		{ID: 3, Description: "tg", Channel: "telegram", Conversation: "42", StartedAt: now.Add(-3 * time.Minute), Execution: JobReconnecting, ReconnectAttempt: 1},
		{ID: 4, Description: "wa", Channel: "whatsapp", Conversation: "1555", StartedAt: now.Add(-4 * time.Minute), Execution: JobRecovering},
		{ID: 5, Description: "ignored", Channel: "orchestrator", Conversation: "markdown", DurableFile: "/private/task [x] (1).md", Durable: true, FirstAssignedAt: now.Add(-3*time.Hour - 27*time.Minute), ProviderIterations: 2, ImplementationAttempts: 4, StartedAt: now.Add(-5 * time.Minute), Execution: JobAudit},
	}
	got := formatJobsAt(jobs, now)
	wants := []string{
		"- **Job 1** conversation  \n  1m 1▶ · running · tui",
		"- **Job 2** conversation  \n  2m 1▶ · starting · cli",
		"- **Job 3** conversation  \n  3m 1▶ · reconnecting 1 · telegram",
		"- **Job 4** conversation  \n  4m 1▶ · recovering · whatsapp",
		"- **Job 5** task \\[x\\] (1).md  \n  3h27m 2▶ 4↻ · audit running · orchestrator",
	}
	for _, want := range wants {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q:\n%s", want, got)
		}
	}
	exact := "# Jobs\n\n" + strings.Join(wants, "\n") + "\n\nUse `/job info <number>` to inspect a job.\nUse `/job kill <number>` to stop a job."
	if got != exact {
		t.Fatalf("exact many-job Markdown changed:\ngot  %q\nwant %q", got, exact)
	}
	one := formatJobsAt(jobs[:1], now)
	wantOne := "# Jobs\n\n" + wants[0] + "\n\nUse `/job info <number>` to inspect a job.\nUse `/job kill <number>` to stop a job."
	if one != wantOne {
		t.Fatalf("exact one-job Markdown changed:\ngot  %q\nwant %q", one, wantOne)
	}
	if strings.Contains(got, "\x1b") || strings.Contains(got, "/private/") {
		t.Fatalf("jobs output leaked unsafe content: %q", got)
	}
	for _, forbidden := range []string{"hello", "world", "local-a", "build", "1555", "/42"} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("jobs output exposed conversation detail %q: %q", forbidden, got)
		}
	}
	long := formatJobsAt([]Job{{ID: 6, Kind: "diagnostic", Description: strings.Repeat("雪", 120) + "\x1b]0;owned\aTAIL", Channel: "orchestrator", Conversation: "local", StartedAt: now, Execution: JobRunning}}, now)
	if strings.Contains(long, "TAIL") || strings.Contains(long, "\x1b") || !strings.Contains(long, "…  \n") {
		t.Fatalf("long/control-bearing description was not safely bounded: %q", long)
	}
	for _, line := range strings.Split(charmansi.Strip(markdown.Terminal(long, 24)), "\n") {
		if charmansi.StringWidth(line) > 24 {
			t.Fatalf("narrow long-description row overflow: %q", line)
		}
	}
}

func TestJobsRenderAsTwoDistinctTerminalRowsAtNormalAndNarrowWidths(t *testing.T) {
	now := time.Date(2026, 8, 8, 9, 0, 0, 0, time.UTC)
	jobs := []Job{{ID: 1, Description: "Unicode 雪 job", Channel: "tui", Conversation: "local", StartedAt: now.Add(-time.Minute), Execution: JobRunning}}
	raw := formatJobsAt(jobs, now)
	for _, width := range []int{80, 24} {
		rendered := charmansi.Strip(markdown.Terminal(raw, width))
		lines := strings.Split(rendered, "\n")
		jobRow, metadataRow := -1, -1
		for index, line := range lines {
			if strings.Contains(line, "Job 1") {
				jobRow = index
			}
			if strings.Contains(line, "running") {
				metadataRow = index
			}
			if charmansi.StringWidth(line) > width {
				t.Fatalf("width %d overflow %q", width, line)
			}
		}
		if jobRow < 0 || metadataRow != jobRow+1 {
			t.Fatalf("width %d did not render adjacent job rows:\n%s", width, rendered)
		}
	}
	for name, rendered := range map[string]string{"telegram": markdown.TelegramHTML(raw), "whatsapp": markdown.WhatsApp(raw)} {
		jobAt, metadataAt := strings.Index(rendered, "Job 1"), strings.Index(rendered, "running")
		if jobAt < 0 || metadataAt < 0 || !strings.Contains(rendered[jobAt:metadataAt], "\n") {
			t.Fatalf("%s collapsed the two logical rows: %q", name, rendered)
		}
	}
}

func TestRuntimeLogIsBoundedAndKeepsNewestEntriesInOrder(t *testing.T) {
	runtime := NewRuntime()
	for index := 0; index < maxLogEntries+7; index++ {
		runtime.Log(fmt.Sprintf("entry-%04d", index))
	}
	logs := runtime.Logs()
	if len(logs) != maxLogEntries || logs[0].Text != "entry-0007" || logs[len(logs)-1].Text != fmt.Sprintf("entry-%04d", maxLogEntries+6) {
		t.Fatalf("bounded logs = %d entries, first %q, last %q", len(logs), logs[0].Text, logs[len(logs)-1].Text)
	}
	runtime.Log(strings.Repeat("x", maxLogEntryRunes+100))
	logs = runtime.Logs()
	if got := len([]rune(logs[len(logs)-1].Text)); got != maxLogEntryRunes || !strings.HasSuffix(logs[len(logs)-1].Text, "…") {
		t.Fatalf("bounded log entry has %d runes: %.20q", got, logs[len(logs)-1].Text)
	}
}

func TestSanitizeLogTextRemovesTerminalCommandsAndUnsafeControls(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{name: "CSI styles", input: "\x1b[2m\x1b[31merror\x1b[0m", want: "error"},
		{name: "OSC hyperlink", input: "\x1b]8;;https://example.com\x1b\\click\x1b]8;;\x1b\\", want: "click"},
		{name: "C1 CSI", input: "\u009b1;32mgreen\u009b0m", want: "green"},
		{name: "raw byte C1 CSI", input: string([]byte{0x9b, '3', '1', 'm', 'r', 'e', 'd', 0x9b, '0', 'm'}), want: "red"},
		{name: "control strings", input: "left\x1bPprivate\x1b\\right\x1b]title\aright", want: "leftrightright"},
		{name: "BEL inside DCS", input: "left\x1bPprivate\a[31mpayload\x1b\\right", want: "leftright"},
		{name: "Unicode and lines", input: "Ошибка 🧪\r\nsecond\tline\x00\b", want: "Ошибка 🧪\nsecond    line"},
		{name: "incomplete CSI", input: "visible\x1b[31", want: "visible"},
		{name: "CSI aborted by newline", input: "before\x1b[31\nafter", want: "before\nafter"},
		{name: "incomplete OSC", input: "visible\x1b]unfinished", want: "visible"},
		{name: "cancelled CSI", input: "before\x1b[31\x18after", want: "beforeafter"},
		{name: "lone escape", input: "visible\x1b", want: "visible"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := sanitizeLogText(test.input); got != test.want {
				t.Fatalf("sanitizeLogText(%q) = %q, want %q", test.input, got, test.want)
			}
		})
	}
}

func TestRuntimeWriterSanitizesRawByteC1AtRenderTime(t *testing.T) {
	runtime := NewRuntime()
	raw := []byte{0x9b, '3', '1', 'm', 'f', 'a', 'i', 'l', 'u', 'r', 'e', 0x9b, '0', 'm', '\n'}
	if _, err := runtime.Write(raw); err != nil {
		t.Fatal(err)
	}
	logs := runtime.Logs()
	if len(logs) != 1 || logs[0].Text != "failure" {
		t.Fatalf("writer did not sanitize captured source log: %#v", logs)
	}
	output := formatLogs(logs)
	if !strings.Contains(output, "failure") || strings.Contains(output, "31m") || strings.ContainsRune(output, utf8.RuneError) {
		t.Fatalf("rendered raw C1 formatting unsafely: %q", output)
	}
}

func TestFormatLogViewsSanitizeAtSharedRenderingBoundary(t *testing.T) {
	entries := make([]LogEntry, 21)
	for index := range entries {
		entries[index] = LogEntry{At: time.Unix(int64(index), 0), Text: fmt.Sprintf("ordinary-%02d", index)}
	}
	entries[0].Text = "\x1b[31mold failure\x1b[0m"
	entries[20].Text = "\x1b[2mnew failure\x1b[0m\n原因 🧪\x00"

	outputs := map[string]string{
		"ordinary": formatLogs(entries),
		"page":     formatLogPage(entries, 2),
		"range":    formatLogPages(entries, 1, 2),
		"search":   formatLogSearch(entries, "failure"),
	}
	for name, output := range outputs {
		if strings.ContainsRune(output, '\x1b') || strings.Contains(output, "[31m") || strings.Contains(output, "[2m") || strings.Contains(output, "[0m") || strings.ContainsRune(output, '\x00') {
			t.Errorf("%s output retained terminal formatting or controls: %q", name, output)
		}
	}
	if output := outputs["ordinary"]; !strings.Contains(output, "new failure\n  原因 🧪") {
		t.Fatalf("ordinary page lost readable multiline Unicode: %q", output)
	}
	if output := outputs["page"]; !strings.Contains(output, "old failure") {
		t.Fatalf("older page lost sanitized content: %q", output)
	}
	if output := outputs["range"]; !strings.Contains(output, "old failure") || !strings.Contains(output, "new failure") {
		t.Fatalf("page range lost sanitized entries: %q", output)
	}
	if output := outputs["search"]; !strings.Contains(output, "2 matches") || !strings.Contains(output, "new failure\n  原因 🧪") {
		t.Fatalf("search lost sanitized matches or line breaks: %q", output)
	}
	if entries[0].Text != "\x1b[31mold failure\x1b[0m" {
		t.Fatalf("rendering mutated the source entry: %q", entries[0].Text)
	}
}

func TestFormatLogSearchIncludesStructuredAttribution(t *testing.T) {
	entries := []LogEntry{{At: time.Now(), Level: "fatal", Component: "telegram", Event: "connect_failed", Instance: "instance-a", Text: "connection stopped"}}
	for _, query := range []string{"fatal", "telegram", "connect_failed", "instance-a"} {
		if output := formatLogSearch(entries, query); !strings.Contains(output, "1 matches") {
			t.Errorf("search %q missed attribution: %q", query, output)
		}
	}
}

func TestFormatLogPagesRendersRangeInNewestFirstPageOrder(t *testing.T) {
	entries := make([]LogEntry, 45)
	for index := range entries {
		entries[index].Text = fmt.Sprintf("entry-%02d", index+1)
	}

	output := formatLogPages(entries, 1, 2)
	if !strings.Contains(output, "45 entries. Pages 1-2 of 3, showing 40.") || strings.Contains(output, "entry-05") || strings.Count(output, "# Log") != 1 {
		t.Fatalf("range output = %s", output)
	}
	if strings.Index(output, "entry-45") > strings.Index(output, "entry-06") {
		t.Fatalf("older page appeared before newest page: %s", output)
	}
}
