package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent0ai/spynel/internal/config"
	"github.com/agent0ai/spynel/internal/core"
	"github.com/agent0ai/spynel/internal/history"
	"github.com/agent0ai/spynel/internal/workspace"
)

func TestCleanupUsesLastUpdateStrictCutoffAndArchivesOnlyTerminalTasks(t *testing.T) {
	root := t.TempDir()
	if err := workspace.Init(root, false); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(config.PathForRoot(root))
	if err != nil {
		t.Fatal(err)
	}
	service := New(cfg, newServiceHarness())
	now := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)
	cutoff := now.Add(-7 * 24 * time.Hour)
	for conversation, at := range map[string]time.Time{
		"old":      cutoff.Add(-time.Second),
		"boundary": cutoff,
		"live":     cutoff.Add(-time.Hour),
	} {
		if _, err := service.History.Append("tui", conversation, history.Entry{At: at, Role: "assistant", Content: conversation}); err != nil {
			t.Fatal(err)
		}
	}

	writeTask := func(status, name string, updated time.Time, valid bool) []byte {
		path := filepath.Join(cfg.StatePath("tasks", status), name)
		data := []byte(fmt.Sprintf("---\nid: %s\nstatus: %s\nupdated_at: %q\nreview_required: true\n---\n\n# Preserved\n", strings.TrimSuffix(name, ".md"), status, updated.Format(time.RFC3339)))
		if !valid {
			data = []byte("not a task document\n")
		}
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		return data
	}
	preserved := writeTask("done", "old-done.md", cutoff.Add(-time.Second), true)
	writeTask("failed", "boundary.md", cutoff, true)
	writeTask("cancelled", "bad.md", cutoff.Add(-time.Hour), false)
	working := writeTask("working", "active.md", cutoff.Add(-time.Hour), true)

	result, err := service.runCleanup(7, "tui", "live", now)
	if err != nil {
		t.Fatal(err)
	}
	if result.RemovedConversations != 1 || result.ArchivedTasks != 1 || result.Protected != 1 || result.Failed != 1 {
		t.Fatalf("cleanup result = %#v", result)
	}
	if _, err := os.Stat(service.History.Path("tui", "old")); !os.IsNotExist(err) {
		t.Fatalf("old conversation remains: %v", err)
	}
	for _, conversation := range []string{"boundary", "live"} {
		if _, err := os.Stat(service.History.Path("tui", conversation)); err != nil {
			t.Fatalf("protected conversation %s was removed: %v", conversation, err)
		}
	}
	archived, err := os.ReadFile(filepath.Join(cfg.StatePath("tasks", "archive"), "old-done.md"))
	if err != nil || string(archived) != string(preserved) {
		t.Fatalf("archived task changed: %v\n%s", err, archived)
	}
	if got, err := os.ReadFile(filepath.Join(cfg.StatePath("tasks", "working"), "active.md")); err != nil || string(got) != string(working) {
		t.Fatalf("active task changed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cfg.StatePath("tasks", "failed"), "boundary.md")); err != nil {
		t.Fatalf("boundary task was archived: %v", err)
	}
	var listing core.Event
	if err := service.Handle(context.Background(), core.Message{Channel: "cli", Conversation: "inspect", Text: "/tasks all"}, func(event core.Event) { listing = event }); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(listing.Text, "old-done") || service.Orchestrator.WorkStatus().TasksActive != 1 {
		t.Fatalf("archive leaked into ordinary workflow views: listing=%q status=%#v", listing.Text, service.Orchestrator.WorkStatus())
	}
}

func TestCleanupRemovesRetiredNotificationRuntimeArtifacts(t *testing.T) {
	root := t.TempDir()
	if err := workspace.Init(root, false); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(config.PathForRoot(root))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"notification-agents", "notification-agent-locks"} {
		directory := cfg.StatePath("runtime", name)
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(directory, "retired.json"), []byte("{}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	service := New(cfg, newServiceHarness())
	result, err := service.runCleanup(7, "", "", time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if result.RemovedObsoleteState != 2 || result.Failed != 0 {
		t.Fatalf("cleanup result = %#v", result)
	}
	for _, name := range []string{"notification-agents", "notification-agent-locks"} {
		if _, err := os.Stat(cfg.StatePath("runtime", name)); !os.IsNotExist(err) {
			t.Fatalf("obsolete runtime artifact %q remains: %v", name, err)
		}
	}
}

func TestCleanupKeepsSettledTasksLinkedToNonterminalGoals(t *testing.T) {
	root := t.TempDir()
	if err := workspace.Init(root, false); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(config.PathForRoot(root))
	if err != nil {
		t.Fatal(err)
	}
	service := New(cfg, newServiceHarness())
	now := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)
	old := now.Add(-8 * 24 * time.Hour)

	writeGoal := func(status, id string) {
		t.Helper()
		path := filepath.Join(cfg.StatePath("goals", status), id+".md")
		data := []byte(fmt.Sprintf("---\nid: %s\nstatus: %s\nupdated_at: %q\n---\n\n# Goal\n", id, status, old.Format(time.RFC3339)))
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeTask := func(name, goalID string) {
		t.Helper()
		path := filepath.Join(cfg.StatePath("tasks", "done"), name+".md")
		goalFields := ""
		if goalID != "" {
			goalFields = fmt.Sprintf("goal_id: %s\ngoal_round: 1\n", goalID)
		}
		data := []byte(fmt.Sprintf("---\nid: %s\nstatus: done\nupdated_at: %q\n%sreview_required: true\n---\n\n# Task\n", name, old.Format(time.RFC3339), goalFields))
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	writeGoal("active", "live-goal")
	writeGoal("done", "terminal-goal")
	writeTask("live-cohort-task", "live-goal")
	writeTask("terminal-goal-task", "terminal-goal")
	writeTask("standalone-task", "")

	result, err := service.runCleanup(7, "", "", now)
	if err != nil {
		t.Fatal(err)
	}
	if result.ArchivedTasks != 2 || result.Protected != 1 || result.Failed != 0 {
		t.Fatalf("cleanup result = %#v", result)
	}
	if _, err := os.Stat(filepath.Join(cfg.StatePath("tasks", "done"), "live-cohort-task.md")); err != nil {
		t.Fatalf("active goal cohort task was archived: %v", err)
	}
	for _, name := range []string{"terminal-goal-task.md", "standalone-task.md"} {
		if _, err := os.Stat(filepath.Join(cfg.StatePath("tasks", "archive"), name)); err != nil {
			t.Fatalf("eligible task %s was not archived: %v", name, err)
		}
	}
}

func TestCleanupFailsSafeWhenNonterminalGoalIndexIsIncomplete(t *testing.T) {
	root := t.TempDir()
	if err := workspace.Init(root, false); err != nil {
		t.Fatal(err)
	}
	cfg, _ := config.Load(config.PathForRoot(root))
	service := New(cfg, newServiceHarness())
	now := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)
	old := now.Add(-8 * 24 * time.Hour)
	if err := os.WriteFile(filepath.Join(cfg.StatePath("goals", "active"), "broken.md"), []byte("not a goal\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(cfg.StatePath("tasks", "done"), "linked.md")
	data := []byte(fmt.Sprintf("---\nid: linked\nstatus: done\nupdated_at: %q\ngoal_id: possibly-live\ngoal_round: 1\nreview_required: true\n---\n", old.Format(time.RFC3339)))
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}

	result, err := service.runCleanup(7, "", "", now)
	if err != nil {
		t.Fatal(err)
	}
	if result.ArchivedTasks != 0 || result.Protected != 1 || result.Failed != 0 {
		t.Fatalf("cleanup result = %#v", result)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("goal-linked task was not protected from an incomplete index: %v", err)
	}
}

func TestCleanupCommandValidatesDaysAndDoesNotOverlap(t *testing.T) {
	root := t.TempDir()
	if err := workspace.Init(root, false); err != nil {
		t.Fatal(err)
	}
	cfg, _ := config.Load(config.PathForRoot(root))
	service := New(cfg, newServiceHarness())
	run := func(command string) string {
		var response core.Event
		if err := service.Handle(context.Background(), core.Message{Channel: "tui", Conversation: "local", Text: command}, func(event core.Event) { response = event }); err != nil {
			t.Fatal(err)
		}
		return response.Text
	}
	for _, command := range []string{"/cleanup 0", "/cleanup -1", "/cleanup 1.5", "/cleanup seven", "/cleanup 2 extra"} {
		if got := run(command); !strings.Contains(got, "Usage: /cleanup [days]") {
			t.Fatalf("%s response = %q", command, got)
		}
	}
	lock, acquired, err := tryCleanupLock(cfg.StatePath("runtime", "cleanup.lock"))
	if err != nil || !acquired {
		t.Fatalf("hold cleanup lock: acquired=%v err=%v", acquired, err)
	}
	defer releaseCleanupLock(lock)
	if got := run("/cleanup"); !strings.Contains(got, "already running") {
		t.Fatalf("overlap response = %q", got)
	}
}

func TestAutomaticCleanupProtectsEveryLeasedIdleTUIConversation(t *testing.T) {
	root := t.TempDir()
	if err := workspace.Init(root, false); err != nil {
		t.Fatal(err)
	}
	cfg, _ := config.Load(config.PathForRoot(root))
	service := New(cfg, newServiceHarness())
	now := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)
	old := now.Add(-8 * 24 * time.Hour)
	for _, conversation := range []string{"idle-primary", "idle-secondary", "stale-client", "unprotected"} {
		if _, err := service.History.Append("tui", conversation, history.Entry{At: old, Role: "assistant", Content: conversation}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := service.History.Append("tui", "newest-saved", history.Entry{At: now, Role: "assistant", Content: "newest"}); err != nil {
		t.Fatal(err)
	}
	if err := service.RegisterLiveTUI("primary-client", "idle-primary", now); err != nil {
		t.Fatal(err)
	}
	if err := service.RegisterLiveTUI("secondary-client", "idle-secondary", now); err != nil {
		t.Fatal(err)
	}
	if err := service.RegisterLiveTUI("stale-client", "stale-client", now.Add(-2*time.Minute)); err != nil {
		t.Fatal(err)
	}

	result, err := service.runCleanup(7, "", "", now)
	if err != nil {
		t.Fatal(err)
	}
	if result.RemovedConversations != 2 || result.Protected != 3 {
		t.Fatalf("cleanup result = %#v", result)
	}
	for _, conversation := range []string{"idle-primary", "idle-secondary"} {
		if _, err := os.Stat(service.History.Path("tui", conversation)); err != nil {
			t.Fatalf("live idle conversation %s was removed: %v", conversation, err)
		}
	}
	for _, conversation := range []string{"stale-client", "unprotected"} {
		if _, err := os.Stat(service.History.Path("tui", conversation)); !os.IsNotExist(err) {
			t.Fatalf("expired/unprotected conversation %s remains: %v", conversation, err)
		}
	}
}

func TestLiveTUIConversationSwitchRetainsTransitionLeaseAndUnregisters(t *testing.T) {
	service := &Service{liveTUI: map[string]map[string]time.Time{}}
	now := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)
	if err := service.RegisterLiveTUI("client", "before", now); err != nil {
		t.Fatal(err)
	}
	if err := service.RegisterLiveTUI("client", "after", now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	protected := service.liveTUIConversations(now.Add(2 * time.Second))
	if !protected["before"] || !protected["after"] {
		t.Fatalf("transition leases = %#v", protected)
	}
	service.UnregisterLiveTUI("client")
	if protected := service.liveTUIConversations(now.Add(2 * time.Second)); len(protected) != 0 {
		t.Fatalf("leases remain after unregister: %#v", protected)
	}
}

func TestPrimaryRestartFencesCleanupUntilLiveTUIsCanRenew(t *testing.T) {
	root := t.TempDir()
	if err := workspace.Init(root, false); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(config.PathForRoot(root))
	if err != nil {
		t.Fatal(err)
	}
	service := New(cfg, newServiceHarness())
	old := time.Now().UTC().Add(-8 * 24 * time.Hour)
	if _, err := service.History.Append("tui", "still-open-after-restart", history.Entry{
		At: old, Role: "assistant", Content: "old but live",
	}); err != nil {
		t.Fatal(err)
	}

	service.SetPrimaryInstanceID("replacement-owner")
	service.instanceMu.RLock()
	fenceEnd := service.cleanupNotBefore
	service.instanceMu.RUnlock()
	if _, err := service.runCleanup(7, "", "", fenceEnd.Add(-time.Nanosecond)); !errors.Is(err, errCleanupLeaseReestablishing) {
		t.Fatalf("cleanup during owner restart fence = %v", err)
	}
	if _, err := os.Stat(service.History.Path("tui", "still-open-after-restart")); err != nil {
		t.Fatalf("restart-window cleanup removed live history: %v", err)
	}

	// A live client renews on its ordinary ten-second cadence. At fence release,
	// the rebuilt owner-side lease protects the conversation normally.
	if err := service.RegisterLiveTUI("idle-client", "still-open-after-restart", fenceEnd.Add(-50*time.Second)); err != nil {
		t.Fatal(err)
	}
	result, err := service.runCleanup(7, "", "", fenceEnd)
	if err != nil {
		t.Fatal(err)
	}
	if result.RemovedConversations != 0 || result.Protected != 1 {
		t.Fatalf("cleanup after renewal = %#v", result)
	}
	if _, err := os.Stat(service.History.Path("tui", "still-open-after-restart")); err != nil {
		t.Fatalf("renewed live history was removed: %v", err)
	}
}

func TestCleanupSerializesLiveTUIAdmissionThroughHistoryDeletion(t *testing.T) {
	root := t.TempDir()
	if err := workspace.Init(root, false); err != nil {
		t.Fatal(err)
	}
	cfg, _ := config.Load(config.PathForRoot(root))
	service := New(cfg, newServiceHarness())
	now := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)
	if _, err := service.History.Append("tui", "admission-race", history.Entry{
		At: now.Add(-8 * 24 * time.Hour), Role: "assistant", Content: "old",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.History.Append("tui", "newest-saved", history.Entry{
		At: now, Role: "assistant", Content: "newest",
	}); err != nil {
		t.Fatal(err)
	}

	protected := make(chan struct{})
	allowRemoval := make(chan struct{})
	removed := make(chan struct{})
	allowUnlock := make(chan struct{})
	service.cleanupHistoryStep = func(step string) {
		switch step {
		case "protected":
			close(protected)
			<-allowRemoval
		case "removed":
			close(removed)
			<-allowUnlock
		}
	}
	cleanupDone := make(chan error, 1)
	go func() {
		_, err := service.runCleanup(7, "", "", now)
		cleanupDone <- err
	}()
	<-protected

	registrationDone := make(chan error, 1)
	go func() {
		registrationDone <- service.RegisterLiveTUI("new-client", "admission-race", now)
	}()
	select {
	case err := <-registrationDone:
		t.Fatalf("registration crossed cleanup protection snapshot: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(allowRemoval)
	<-removed
	if _, err := os.Stat(service.History.Path("tui", "admission-race")); !os.IsNotExist(err) {
		t.Fatalf("old history was not removed inside serialized boundary: %v", err)
	}
	select {
	case err := <-registrationDone:
		t.Fatalf("registration completed before cleanup released deletion boundary: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(allowUnlock)
	if err := <-cleanupDone; err != nil {
		t.Fatal(err)
	}
	if err := <-registrationDone; err != nil {
		t.Fatal(err)
	}
}

func TestResumeRegistersBranchBeforeConcurrentCleanupCanDeleteIt(t *testing.T) {
	root := t.TempDir()
	if err := workspace.Init(root, false); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(config.PathForRoot(root))
	if err != nil {
		t.Fatal(err)
	}
	service := New(cfg, newServiceHarness())
	now := time.Now().UTC()
	if _, err := service.History.Append("telegram", "old-source", history.Entry{
		At: now.Add(-8 * 24 * time.Hour), Role: "assistant", Content: "old answer",
	}); err != nil {
		t.Fatal(err)
	}

	branched := make(chan struct{})
	allowRegistration := make(chan struct{})
	service.resumeAdmissionStep = func(step string) {
		if step == "branched" {
			close(branched)
			<-allowRegistration
		}
	}
	action := "resume:" + encodeConversation("telegram", "old-source")
	resumeDone := make(chan struct {
		screen *core.Screen
		err    error
	}, 1)
	go func() {
		screen, err := service.ScreenActionForInstance(context.Background(), "resume-client", "resume", action, nil)
		resumeDone <- struct {
			screen *core.Screen
			err    error
		}{screen: screen, err: err}
	}()
	<-branched

	cleanupDone := make(chan error, 1)
	go func() {
		_, err := service.runCleanup(7, "tui", "invoking-conversation", now)
		cleanupDone <- err
	}()
	select {
	case err := <-cleanupDone:
		t.Fatalf("cleanup crossed resume creation-to-registration boundary: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	close(allowRegistration)
	resumed := <-resumeDone
	if resumed.err != nil || resumed.screen == nil || resumed.screen.ID != "chat" {
		t.Fatalf("resume result = %#v, %v", resumed.screen, resumed.err)
	}
	if err := <-cleanupDone; err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(service.History.Path("tui", resumed.screen.Conversation)); err != nil {
		t.Fatalf("admitted resumed branch was removed: %v", err)
	}
	if _, err := os.Stat(service.History.Path("telegram", "old-source")); !os.IsNotExist(err) {
		t.Fatalf("unprotected old source was not removed: %v", err)
	}
}
