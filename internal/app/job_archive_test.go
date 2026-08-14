package app

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agent0ai/spynel/internal/core"
)

func writeJobCounterForTest(t *testing.T, directory string, number int, generation uint64) {
	t.Helper()
	data, err := json.Marshal(jobCounter{Number: number, Generation: generation})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, ".counter.json"), append(data, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestJobNumbersAllocateAtomicallyAcrossRuntimes(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "jobs")
	const count = 128
	ids := make(chan int, count)
	var group sync.WaitGroup
	for index := 0; index < count; index++ {
		group.Add(1)
		go func(index int) {
			defer group.Done()
			runtime := NewRuntime()
			runtime.ConfigureJobArchive(directory)
			handle := runtime.BeginJob("concurrent-"+strconv.Itoa(index), "cli", "private", "conversation")
			job, _ := runtime.Job(handle)
			ids <- job.Number
		}(index)
	}
	group.Wait()
	close(ids)
	seen := map[int]bool{}
	for id := range ids {
		if id < 1 || id > maxJobNumber || seen[id] {
			t.Fatalf("invalid or duplicate allocation %d (seen=%t)", id, seen[id])
		}
		seen[id] = true
	}
	if len(seen) != count {
		t.Fatalf("allocated %d unique numbers, want %d", len(seen), count)
	}
}

func TestJobNumberRestartWrapAndNewestGenerationLookup(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "jobs")
	writeJobCounterForTest(t, directory, maxJobNumber-1, 40)
	first := NewRuntime()
	first.ConfigureJobArchive(directory)
	handle9999 := first.BeginJob("last", "cli", "private", "conversation")
	job9999, _ := first.Job(handle9999)
	first.RecordJobEvent(handle9999, core.Event{Kind: core.EventFinal, Text: "last before wrap", Done: true})
	first.EndJob(handle9999)
	if job9999.Number != maxJobNumber {
		t.Fatalf("last number = %d", job9999.Number)
	}

	restarted := NewRuntime()
	restarted.ConfigureJobArchive(directory)
	handle1 := restarted.BeginJob("wrapped-old", "cli", "private", "conversation")
	restarted.RecordJobEvent(handle1, core.Event{Kind: core.EventFinal, Text: "older generation", Done: true})
	old, _ := restarted.Job(handle1)
	restarted.EndJob(handle1)
	if old.Number != 1 {
		t.Fatalf("wrapped number = %d", old.Number)
	}

	writeJobCounterForTest(t, directory, maxJobNumber, 10040)
	newest := NewRuntime()
	newest.ConfigureJobArchive(directory)
	if handle := newest.BeginJob("wrapped-new", "cli", "private", "conversation"); handle < 1 {
		t.Fatalf("invalid execution handle = %d", handle)
	} else {
		newest.RecordJobEvent(handle, core.Event{Kind: core.EventFinal, Text: "newest generation", Done: true})
		newer, _ := newest.Job(handle)
		newest.EndJob(handle)
		if newer.Number != 1 {
			t.Fatalf("reused number = %d", newer.Number)
		}
		if newer.StableID == old.StableID {
			t.Fatal("private archive identity was reused")
		}
	}
	item, output, err := newest.ArchivedJob(1)
	if err != nil || item.Generation != 10041 || !strings.Contains(output, "newest generation") || strings.Contains(output, "older generation") {
		t.Fatalf("newest numeric lookup = %#v %q %v", item, output, err)
	}
	items, err := newest.RecentArchivedJobs(20)
	if err != nil {
		t.Fatal(err)
	}
	seenOne := 0
	for _, item := range items {
		if item.Number == 1 {
			seenOne++
		}
	}
	if seenOne != 1 {
		t.Fatalf("recent list exposed %d generations for reused number", seenOne)
	}
}

func TestReusedLiveNumberKeepsGenerationSafeExecutionHandles(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "jobs")
	writeJobCounterForTest(t, directory, maxJobNumber, 20)
	runtime := NewRuntime()
	runtime.ConfigureJobArchive(directory)
	oldHandle := runtime.BeginJob("old-live", "cli", "private", "conversation")
	old, _ := runtime.Job(oldHandle)
	writeJobCounterForTest(t, directory, maxJobNumber, 40)
	newHandle := runtime.BeginJob("new-live", "cli", "private", "conversation")
	newer, _ := runtime.Job(newHandle)
	if oldHandle == newHandle || old.Number != 1 || newer.Number != 1 || old.StableID == newer.StableID {
		t.Fatalf("handles/generations old=%#v new=%#v", old, newer)
	}
	resolved, ok := runtime.JobByNumber(1)
	if !ok || resolved.ID != newHandle {
		t.Fatalf("numeric live lookup resolved %#v, %t", resolved, ok)
	}
	runtime.RecordJobEvent(oldHandle, core.Event{Kind: core.EventFinal, Text: "old late event", Done: true})
	runtime.RecordJobEvent(newHandle, core.Event{Kind: core.EventFinal, Text: "new event", Done: true})
	runtime.EndJob(oldHandle)
	if resolved, ok := runtime.JobByNumber(1); !ok || resolved.ID != newHandle {
		t.Fatalf("ending old generation removed newer lookup: %#v, %t", resolved, ok)
	}
	runtime.EndJob(newHandle)
	item, output, err := runtime.ArchivedJob(1)
	if err != nil || item.Generation != newer.Generation || !strings.Contains(output, "new event") || strings.Contains(output, "old late event") {
		t.Fatalf("newest archive after overlapping reuse = %#v %q %v", item, output, err)
	}
}

func TestOlderRecoveredGenerationCannotShadowNewerWrappedLiveJob(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "jobs")
	details := JobDetails{Kind: "task", WorkID: "release", Phase: "task_implementation", PhaseAttempt: 1}
	runtime := NewRuntime()
	runtime.ConfigureJobArchive(directory)
	oldHandle := runtime.BeginJobWithDetails("old", "orchestrator", "markdown", "old recovery target", details)
	old, _ := runtime.Job(oldHandle)
	runtime.RecordJobEvent(oldHandle, core.Event{Kind: core.EventStatus, Text: "old generation output"})
	runtime.EndJob(oldHandle)

	writeJobCounterForTest(t, directory, maxJobNumber, 9999)
	newHandle := runtime.BeginJob("new", "cli", "private", "newest wrapped job")
	newer, _ := runtime.Job(newHandle)
	if old.Number != 1 || old.Generation != 1 || newer.Number != 1 || newer.Generation != 10000 {
		t.Fatalf("unexpected wrap identities: old=%#v newer=%#v", old, newer)
	}
	runtime.RecordJobEvent(newHandle, core.Event{Kind: core.EventStatus, Text: "new generation output", Done: true})

	recoveredHandle := runtime.BeginJobWithDetails("recovered", "orchestrator", "markdown", "old recovery target", details)
	recovered, _ := runtime.Job(recoveredHandle)
	if recovered.Generation != old.Generation || recovered.StableID != old.StableID {
		t.Fatalf("recovery did not retain old identity: recovered=%#v old=%#v", recovered, old)
	}
	if resolved, ok := runtime.JobByNumber(1); !ok || resolved.ID != newHandle {
		t.Fatalf("older recovery shadowed newer numeric lookup: resolved=%#v ok=%t", resolved, ok)
	}
	listed := runtime.NumericJobs()
	if len(listed) != 1 || listed[0].ID != newHandle {
		t.Fatalf("numeric live listing = %#v, want only newer generation", listed)
	}
	if output := formatJobs(listed); strings.Count(output, "**Job 1**") != 1 || strings.Contains(output, "old recovery target") {
		t.Fatalf("numeric live list exposed the wrong generation:\n%s", output)
	}
	item, output, err := runtime.ArchivedJob(1)
	if err != nil || item.Generation != newer.Generation || !strings.Contains(output, "new generation output") || strings.Contains(output, "old generation output") {
		t.Fatalf("numeric output while both generations live = %#v %q %v", item, output, err)
	}

	runtime.EndJob(recoveredHandle)
	if resolved, ok := runtime.JobByNumber(1); !ok || resolved.ID != newHandle {
		t.Fatalf("ending older recovery changed newer numeric lookup: resolved=%#v ok=%t", resolved, ok)
	}
	runtime.EndJob(newHandle)
	if _, ok := runtime.JobByNumber(1); ok {
		t.Fatal("completed newest generation remained in live numeric lookup")
	}
	item, output, err = runtime.ArchivedJob(1)
	if err != nil || item.Generation != newer.Generation || !strings.Contains(output, "new generation output") || strings.Contains(output, "old generation output") {
		t.Fatalf("newest archive after recovery ordering = %#v %q %v", item, output, err)
	}
}

func TestRecoveringDurableJobKeepsNumberAndArchiveAcrossRestart(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "jobs")
	first := NewRuntime()
	first.ConfigureJobArchive(directory)
	details := JobDetails{Kind: "task", WorkID: "depfix-release", Phase: "task_implementation", PhaseAttempt: 1}
	id := first.BeginJobWithDetails("old-session", "orchestrator", "markdown", "release task", details)
	first.RecordJobEvent(id, core.Event{Kind: core.EventStatus, Text: "preserved release output", Done: true})
	old, _ := first.Job(id)
	first.archive.close() // simulate process exit without terminal archive finalization

	restarted := NewRuntime()
	restarted.ConfigureJobArchive(directory)
	recoveredID := restarted.BeginJobWithDetails("new-session", "orchestrator", "markdown", "release task", details)
	recovered, _ := restarted.Job(recoveredID)
	if recovered.Number != old.Number || recovered.StableID != old.StableID {
		t.Fatalf("recovered identity = number %d stable %q, want %d %q", recovered.Number, recovered.StableID, old.Number, old.StableID)
	}
	restarted.RecordJobEvent(recoveredID, core.Event{Kind: core.EventFinal, Text: "recovery complete", Done: true})
	restarted.EndJob(recoveredID)
	_, output, err := restarted.ArchivedJob(old.Number)
	if err != nil || !strings.Contains(output, "preserved release output") || !strings.Contains(output, "recovery complete") {
		t.Fatalf("recovered output = %q, %v", output, err)
	}
}

func TestOverlappingSameDispatchAllocatesDistinctOwnedGenerations(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "jobs")
	details := JobDetails{Kind: "task", WorkID: "release", Phase: "task_implementation", PhaseAttempt: 1}
	first := NewRuntime()
	first.ConfigureJobArchive(directory)
	firstID := first.BeginJobWithDetails("first", "orchestrator", "markdown", "release task", details)
	firstJob, _ := first.Job(firstID)
	first.RecordJobEvent(firstID, core.Event{Kind: core.EventStatus, Text: "first runtime output", Done: true})

	second := NewRuntime()
	second.ConfigureJobArchive(directory)
	secondID := second.BeginJobWithDetails("second", "orchestrator", "markdown", "release task", details)
	secondJob, _ := second.Job(secondID)
	if firstJob.Number == secondJob.Number || firstJob.Generation == secondJob.Generation || firstJob.StableID == secondJob.StableID {
		t.Fatalf("overlapping dispatch reused active identity: first=%#v second=%#v", firstJob, secondJob)
	}
	second.RecordJobEvent(secondID, core.Event{Kind: core.EventStatus, Text: "second runtime output", Done: true})
	first.EndJob(firstID)
	second.EndJob(secondID)

	_, firstOutput, firstErr := second.ArchivedJob(firstJob.Number)
	_, secondOutput, secondErr := second.ArchivedJob(secondJob.Number)
	if firstErr != nil || !strings.Contains(firstOutput, "first runtime output") || strings.Contains(firstOutput, "second runtime output") {
		t.Fatalf("first archive corrupted: %q, %v", firstOutput, firstErr)
	}
	if secondErr != nil || !strings.Contains(secondOutput, "second runtime output") || strings.Contains(secondOutput, "first runtime output") {
		t.Fatalf("second archive corrupted: %q, %v", secondOutput, secondErr)
	}
}

func TestCleanupProtectsArchiveOwnedByAnotherRuntime(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "jobs")
	first := NewRuntime()
	first.ConfigureJobArchive(directory)
	id := first.BeginJobWithDetails("first", "orchestrator", "markdown", "release task", JobDetails{
		Kind: "task", WorkID: "release", Phase: "task_implementation", PhaseAttempt: 1,
	})
	job, _ := first.Job(id)

	second := NewRuntime()
	second.ConfigureJobArchive(directory)
	removed, _, protected, failed := second.CleanupArchivedJobs(time.Now().UTC().Add(time.Hour))
	if removed != 0 || protected != 1 || failed != 0 {
		t.Fatalf("cleanup of remotely owned archive = removed %d protected %d failed %d", removed, protected, failed)
	}
	if _, _, err := second.ArchivedJob(job.Number); err != nil {
		t.Fatalf("remotely owned archive was removed: %v", err)
	}

	first.EndJob(id)
	removed, _, protected, failed = second.CleanupArchivedJobs(time.Now().UTC().Add(time.Hour))
	if removed != 1 || protected != 0 || failed != 0 {
		t.Fatalf("cleanup after owner release = removed %d protected %d failed %d", removed, protected, failed)
	}
	if _, err := os.Stat(filepath.Join(directory, job.StableID+".owner.lock")); !os.IsNotExist(err) {
		t.Fatalf("retired owner lock remains: %v", err)
	}
}

func TestRecoveringTerminalizedDurableJobKeepsNumberAndArchiveAcrossRestart(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "jobs")
	first := NewRuntime()
	first.ConfigureJobArchive(directory)
	details := JobDetails{Kind: "task", WorkID: "depfix-release", Phase: "task_implementation", PhaseAttempt: 1}
	id := first.BeginJobWithDetails("old-session", "orchestrator", "markdown", "release task", details)
	first.RecordJobEvent(id, core.Event{Kind: core.EventFinal, Text: "provider completed before recovery", Done: true})
	old, _ := first.Job(id)
	first.EndJob(id)

	restarted := NewRuntime()
	restarted.ConfigureJobArchive(directory)
	recoveredID := restarted.BeginJobWithDetails("new-session", "orchestrator", "markdown", "release task", details)
	recovered, _ := restarted.Job(recoveredID)
	if recovered.Number != old.Number || recovered.Generation != old.Generation || recovered.StableID != old.StableID {
		t.Fatalf("recovered identity = number %d generation %d stable %q, want %d %d %q", recovered.Number, recovered.Generation, recovered.StableID, old.Number, old.Generation, old.StableID)
	}
	restarted.RecordJobEvent(recoveredID, core.Event{Kind: core.EventFinal, Text: "recovery completed", Done: true})
	restarted.EndJob(recoveredID)
	item, output, err := restarted.ArchivedJob(old.Number)
	if err != nil || item.ID != old.StableID || !strings.Contains(output, "provider completed before recovery") || !strings.Contains(output, "recovery completed") {
		t.Fatalf("recovered archive = %#v %q, %v", item, output, err)
	}
}

func TestDurableRecoveryDoesNotReuseDifferentJobKind(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "jobs")
	first := NewRuntime()
	first.ConfigureJobArchive(directory)
	taskDetails := JobDetails{Kind: "task", WorkID: "depfix-release", Phase: "task_implementation", PhaseAttempt: 1}
	taskID := first.BeginJobWithDetails("task", "orchestrator", "markdown", "release task", taskDetails)
	task, _ := first.Job(taskID)
	first.EndJob(taskID)
	notificationID := first.BeginJobWithDetails("notification", "orchestrator", "markdown", "task notification agent", JobDetails{Kind: "notification", WorkID: "depfix-release", Phase: "notification", PhaseAttempt: 1})
	notification, _ := first.Job(notificationID)
	first.EndJob(notificationID)
	if notification.Number == task.Number {
		t.Fatalf("notification reused task number %d", task.Number)
	}

	restarted := NewRuntime()
	restarted.ConfigureJobArchive(directory)
	recoveredID := restarted.BeginJobWithDetails("recovered-task", "orchestrator", "markdown", "release task", taskDetails)
	recovered, _ := restarted.Job(recoveredID)
	if recovered.Number != task.Number || recovered.Generation != task.Generation || recovered.StableID != task.StableID {
		t.Fatalf("recovered task identity = %#v, want %#v", recovered, task)
	}
}

func TestDurableRecoverySeparatesWorkflowPhaseAndLaterAttempt(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "jobs")
	first := NewRuntime()
	first.ConfigureJobArchive(directory)
	implementationDetails := JobDetails{Kind: "task", WorkID: "release", Phase: "task_implementation", PhaseAttempt: 1}
	implementationID := first.BeginJobWithDetails("implementation", "orchestrator", "markdown", "release task", implementationDetails)
	first.RecordJobEvent(implementationID, core.Event{Kind: core.EventFinal, Text: "implementation output", Done: true})
	implementation, _ := first.Job(implementationID)
	first.EndJob(implementationID)

	reviewDetails := JobDetails{Kind: "task", WorkID: "release", Phase: "task_review", PhaseAttempt: 1}
	reviewID := first.BeginJobWithDetails("review", "orchestrator", "markdown", "release task", reviewDetails)
	first.RecordJobEvent(reviewID, core.Event{Kind: core.EventFinal, Text: "review output", Done: true})
	review, _ := first.Job(reviewID)
	first.EndJob(reviewID)
	if review.Number == implementation.Number || review.StableID == implementation.StableID {
		t.Fatalf("review reused implementation identity: implementation=%#v review=%#v", implementation, review)
	}

	restarted := NewRuntime()
	restarted.ConfigureJobArchive(directory)
	reworkDetails := JobDetails{Kind: "task", WorkID: "release", Phase: "task_implementation", PhaseAttempt: 2}
	reworkID := restarted.BeginJobWithDetails("rework", "orchestrator", "markdown", "release task", reworkDetails)
	rework, _ := restarted.Job(reworkID)
	if rework.Number == implementation.Number || rework.StableID == implementation.StableID || rework.Number == review.Number || rework.StableID == review.StableID {
		t.Fatalf("later attempt reused prior phase identity: implementation=%#v review=%#v rework=%#v", implementation, review, rework)
	}
	restarted.EndJob(reworkID)

	recovery := NewRuntime()
	recovery.ConfigureJobArchive(directory)
	recoveredID := recovery.BeginJobWithDetails("recovered-rework", "orchestrator", "markdown", "release task", reworkDetails)
	recovered, _ := recovery.Job(recoveredID)
	if recovered.Number != rework.Number || recovered.Generation != rework.Generation || recovered.StableID != rework.StableID {
		t.Fatalf("same-dispatch recovery identity = %#v, want %#v", recovered, rework)
	}
	_, implementationOutput, err := recovery.ArchivedJob(implementation.Number)
	if err != nil || !strings.Contains(implementationOutput, "implementation output") || strings.Contains(implementationOutput, "review output") {
		t.Fatalf("implementation archive corrupted: %q, %v", implementationOutput, err)
	}
	_, reviewOutput, err := recovery.ArchivedJob(review.Number)
	if err != nil || !strings.Contains(reviewOutput, "review output") || strings.Contains(reviewOutput, "implementation output") {
		t.Fatalf("review archive corrupted: %q, %v", reviewOutput, err)
	}
}

func TestLegacyStableArchiveMigratesToNumericLookupAndCounter(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "jobs")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	stableID := "j-20260813T120000Z-deadbeef"
	legacy := "---\n" +
		"id: \"" + stableID + "\"\n" +
		"live_alias: \"73\"\n" +
		"title: \"legacy release\"\n" +
		"type: \"task\"\n" +
		"origin: \"task-implementation\"\n" +
		"work_id: \"depfix-release\"\n" +
		"started_at: \"2026-08-13T12:00:00Z\"\n" +
		"ended_at: \"2026-08-13T12:01:00Z\"\n" +
		"state: \"completed\"\n" +
		"---\n\n# Output\nlegacy output remains\n"
	if err := os.WriteFile(filepath.Join(directory, stableID+".md"), []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	runtime := NewRuntime()
	runtime.ConfigureJobArchive(directory)
	item, output, err := runtime.ArchivedJob(73)
	if err != nil || item.Number != 73 || !strings.Contains(output, "legacy output remains") {
		t.Fatalf("legacy numeric lookup = %#v %q %v", item, output, err)
	}
	handle := runtime.BeginJob("after-migration", "cli", "private", "conversation")
	job, _ := runtime.Job(handle)
	if job.Number != 74 {
		t.Fatalf("post-migration number = %d, want 74", job.Number)
	}
}

func TestJobArchiveSurvivesRestartWithStableReferenceAndOrderedOutput(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "jobs")
	now := time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)
	runtime := NewRuntime()
	runtime.Now = func() time.Time { return now }
	runtime.ConfigureJobArchive(directory)
	id := runtime.BeginJobWithDetails("session-private", "telegram", "TG-private", "inspect output", JobDetails{Kind: "conversation", Provider: "codex"})
	job, ok := runtime.Job(id)
	if !ok || !validStableJobID(job.StableID) {
		t.Fatalf("live job = %#v, %t", job, ok)
	}
	runtime.RecordJobEvent(id, core.Event{Kind: core.EventDelta, Text: "first"})
	runtime.RecordJobEvent(id, core.Event{Kind: core.EventError, Text: "api_key=do-not-store\nsecond", Done: true})
	runtime.UpdateJob(id, core.ExecutionStatus{State: string(JobError)})
	now = now.Add(2 * time.Second)
	runtime.EndJob(id)

	restarted := NewRuntime()
	restarted.ConfigureJobArchive(directory)
	items, err := restarted.RecentArchivedJobs(20)
	if err != nil || len(items) != 1 || items[0].ID != job.StableID || items[0].State != "error" || items[0].Title != "conversation" {
		t.Fatalf("restored jobs = %#v, %v", items, err)
	}
	_, output, err := restarted.ArchivedJob(job.StableID)
	if err != nil || !strings.Contains(output, "first") || !strings.Contains(output, "second") || strings.Index(output, "first") > strings.Index(output, "second") || strings.Contains(output, "do-not-store") || !strings.Contains(output, "[REDACTED]") {
		t.Fatalf("restored output = %q, %v", output, err)
	}
	data, err := os.ReadFile(filepath.Join(directory, job.StableID+".md"))
	if err != nil || strings.Contains(string(data), "TG-private") || strings.Contains(string(data), "session-private") {
		t.Fatalf("archive leaked routing identity: %v\n%s", err, data)
	}
}

func TestJobArchiveConcurrentEventsAreBoundedAndReadable(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "jobs")
	runtime := NewRuntime()
	runtime.ConfigureJobArchive(directory)
	id := runtime.BeginJobWithDetails("concurrent", "orchestrator", "markdown", "task.md", JobDetails{Kind: "task", Provider: "codex", ParentID: "task-1"})
	var group sync.WaitGroup
	for index := 0; index < 32; index++ {
		group.Add(1)
		go func() {
			defer group.Done()
			runtime.RecordJobEvent(id, core.Event{Kind: core.EventDelta, Text: strings.Repeat("x", 32<<10)})
		}()
	}
	group.Wait()
	job, _ := runtime.Job(id)
	runtime.EndJob(id)
	data, err := os.ReadFile(filepath.Join(directory, job.StableID+".md"))
	if err != nil || len(data) > maxJobArchiveBytes+16<<10 || !strings.Contains(string(data), "JOB OUTPUT TRUNCATED") {
		t.Fatalf("bounded archive bytes=%d err=%v marker=%t", len(data), err, strings.Contains(string(data), "JOB OUTPUT TRUNCATED"))
	}
	if _, _, err := runtime.ArchivedJob(job.StableID); err != nil {
		t.Fatalf("read bounded archive: %v", err)
	}
}

func TestJobArchiveCleanupProtectsLiveAndRemovesExpiredTerminalFile(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "jobs")
	now := time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)
	runtime := NewRuntime()
	runtime.Now = func() time.Time { return now.Add(-10 * 24 * time.Hour) }
	runtime.ConfigureJobArchive(directory)
	finishedID := runtime.BeginJob("finished", "cli", "private", "finished")
	finished, _ := runtime.Job(finishedID)
	runtime.EndJob(finishedID)
	liveID := runtime.BeginJob("live", "cli", "private", "live")
	live, _ := runtime.Job(liveID)
	partial := filepath.Join(directory, "j-20260801T120000Z-deadbeef.md")
	if err := os.WriteFile(partial, []byte("partial archive"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := now.Add(-10 * 24 * time.Hour)
	if err := os.Chtimes(partial, old, old); err != nil {
		t.Fatal(err)
	}
	removed, removedBytes, protected, failed := runtime.CleanupArchivedJobs(now.Add(-7 * 24 * time.Hour))
	if removed != 2 || removedBytes <= 0 || protected != 1 || failed != 0 {
		t.Fatalf("cleanup = removed %d bytes %d protected %d failed %d", removed, removedBytes, protected, failed)
	}
	if _, err := os.Stat(filepath.Join(directory, finished.StableID+".md")); !os.IsNotExist(err) {
		t.Fatalf("expired archive remains: %v", err)
	}
	if _, err := os.Stat(filepath.Join(directory, live.StableID+".md")); err != nil {
		t.Fatalf("live archive removed: %v", err)
	}
}

func TestJobArchiveRejectsSymlinkedWorkspaceBoundaryAndArchiveFiles(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	directory := filepath.Join(root, "jobs")
	if err := os.Symlink(outside, directory); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	runtime := NewRuntime()
	runtime.ConfigureJobArchive(directory)
	id, err := runtime.TryBeginJobWithDetails("private", "cli", "private", "conversation", JobDetails{})
	if err == nil || id != 0 {
		t.Fatalf("symlinked archive admitted job %d with error %v", id, err)
	}
	if status := runtime.Status(); status.Jobs != 0 {
		t.Fatalf("failed allocation registered a live job: %#v", status)
	}
	entries, readErr := os.ReadDir(outside)
	if readErr != nil || len(entries) != 0 {
		t.Fatalf("archive crossed symlinked workspace boundary: entries=%v err=%v", entries, readErr)
	}

	if err := os.Remove(directory); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(outside, "secret.md")
	if err := os.WriteFile(target, []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	ref := "j-20260813T120000Z-deadbeef"
	if err := os.Symlink(target, filepath.Join(directory, ref+".md")); err != nil {
		t.Fatal(err)
	}
	clean := NewRuntime()
	clean.ConfigureJobArchive(directory)
	if _, _, err := clean.ArchivedJob(ref); err == nil {
		t.Fatal("symlinked archive file was readable")
	}
}

func TestJobArchiveClassifiesOrchestratorOriginsAndLinkage(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "jobs")
	runtime := NewRuntime()
	runtime.ConfigureJobArchive(directory)
	for _, test := range []struct {
		kind, phase, state, origin string
	}{
		{kind: "task", phase: "implementation", state: "processing", origin: "task-implementation"},
		{kind: "task", phase: "task_review", state: "processing", origin: "task-review"},
		{kind: "goal", phase: "planning", state: "processing", origin: "goal-planning"},
		{kind: "goal", phase: "goal_review", state: "processing", origin: "goal-review"},
		{kind: "heartbeat", phase: "semantic_heartbeat", state: "working", origin: "heartbeat"},
		{kind: "notification", phase: "notification", state: "acting", origin: "notification"},
	} {
		id := runtime.BeginJobWithDetails(test.origin, "orchestrator", "markdown", test.origin, JobDetails{Kind: test.kind, Provider: "codex", WorkID: "task-1", ParentID: "goal-1", PhaseAttempt: 2})
		runtime.UpdateJobFromLease(id, test.state, test.phase, "", time.Now().UTC(), 1)
		job, _ := runtime.Job(id)
		runtime.EndJob(id)
		item, _, err := runtime.ArchivedJob(job.StableID)
		if err != nil || item.Origin != test.origin || item.WorkID != "task-1" || item.ParentID != "goal-1" || item.Phase != test.phase || item.State != "completed" {
			t.Errorf("%s archive = %#v, %v", test.origin, item, err)
		}
	}
}
