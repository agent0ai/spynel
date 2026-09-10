package localapi

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/agent0ai/spynel/internal/app"
	"github.com/agent0ai/spynel/internal/core"
	"github.com/agent0ai/spynel/internal/instance"
)

func TestGlobalJobActivityReachesPrimaryAndAttachedTUI(t *testing.T) {
	root := t.TempDir()
	election, server, _, cancel, done := startTestServer(t, root)
	defer func() {
		cancel()
		<-done
		_ = server.Service.Close()
		lease, _ := election.Current()
		_ = election.Release(lease.Token)
	}()
	secondary, err := instance.New(server.Service.Settings.Snapshot().StatePath())
	if err != nil {
		t.Fatal(err)
	}
	clients := []*Client{NewClient(election), NewClient(secondary)}
	ctx := context.Background()
	check := func(registered, live int) {
		t.Helper()
		for _, client := range clients {
			// Registration is also the initial/reconnect refresh boundary.
			initial, err := client.RegisterLiveTUIState(ctx, "idle-displayed")
			if err != nil {
				t.Fatal(err)
			}
			state, err := client.State(ctx)
			if err != nil {
				t.Fatal(err)
			}
			status, err := client.Status(ctx, "idle-displayed")
			if err != nil {
				t.Fatal(err)
			}
			if initial.Runtime != state.Runtime || state.Runtime != status.Runtime || state.Runtime.Jobs != registered || state.Runtime.LiveJobs != live {
				t.Fatalf("registration/poll/status disagree: initial=%#v state=%#v status=%#v", initial.Runtime, state.Runtime, status.Runtime)
			}
			if state.ConversationActivity != 0 || state.DurableWork != (core.DurableWorkCounts{}) || status.TurnActive || status.OrchestratorLease != 0 || status.OrchestratorRuns != 0 {
				t.Fatalf("global job manufactured local work or leases: %#v %#v", state, status)
			}
		}
	}
	runtime := server.Service.Runtime
	check(0, 0)
	first := runtime.BeginJob("chat:telegram:elsewhere", "telegram", "elsewhere", "remote conversation")
	runtime.SetJobRunningIfStarting(first)
	check(1, 1)
	second := runtime.BeginJob("chat:tui:other", "tui", "other", "another TUI")
	check(2, 2)
	runtime.UpdateJob(first, core.ExecutionStatus{State: string(app.JobReconnecting)})
	check(2, 2)
	runtime.EndJob(second)
	check(1, 1)
	runtime.UpdateJob(first, core.ExecutionStatus{State: string(app.JobRunning)})
	check(1, 1)
	runtime.UpdateJob(first, core.ExecutionStatus{State: string(app.JobFinishing)})
	check(1, 0)
	runtime.EndJob(first)
	check(0, 0)

	for _, relative := range []string{"tasks/todo/queued.md", "goals/active/passive.md"} {
		path := filepath.Join(root, ".spynel", relative)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("---\nid: pending\n---\n# Pending work\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	state, err := clients[1].State(ctx)
	if err != nil || state.DurableWork != (core.DurableWorkCounts{Tasks: 1, Goals: 1}) || state.Runtime.LiveJobs != 0 {
		t.Fatalf("queued task/passive goal manufactured activity: %#v, %v", state, err)
	}
}
