//go:build linux || darwin

package cli

import (
	"context"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/agent0ai/spynel/internal/harness"
	"github.com/agent0ai/spynel/internal/history"
)

func TestWorkerConversationNativeFollowup(t *testing.T) {
	for _, scenario := range []string{"acknowledged", "uncertain", "deadline"} {
		t.Run(scenario, func(t *testing.T) {
			uncertain := scenario != "acknowledged"
			if uncertain {
				mode := "1"
				if scenario == "deadline" {
					mode = "timeout"
				}
				t.Setenv("SPYNEL_TEST_WORKER_UNCERTAIN_STEER", mode)
			}
			f := newWorkerAgentFixture(t)
			c := harness.WorkerConversation{Channel: "cli", Conversation: "followup", ExecutionID: "synthetic-execution", SourceMessageID: "alpha", Sources: []string{"alpha"}}
			for _, source := range []string{"alpha", "beta"} {
				if _, err := f.service.History.Append(c.Channel, c.Conversation, history.Entry{Role: "user", Content: "synthetic " + source, SourceMessageID: source, AcceptedAt: time.Now().UTC()}); err != nil {
					t.Fatal(err)
				}
			}
			execution, err := f.service.Orchestrator.AdmitConversation(t.Context(), c, "synthetic first", "first")
			if err != nil {
				t.Fatal(err)
			}
			job, ok := f.service.Runtime.JobForSession(execution.Key)
			if !ok {
				t.Fatal("initial durable job absent")
			}
			identity := f.client.Info()
			t.Logf("worker pid=%d start=%s generation=%s", identity.PID, identity.Process.StartTime, identity.Process.Generation)
			batch := c
			batch.SourceMessageID, batch.Sources = "beta", []string{"beta"}
			request := harness.WorkerRequest{Action: "followup", Sequence: execution.Sequence, Key: execution.Key, Prompt: "synthetic second", Message: "second", Conversation: &batch}
			started := time.Now()
			if _, err := f.client.Call(t.Context(), request); (err != nil) != uncertain {
				t.Fatalf("incorrect follow-up result: %v", err)
			}
			if scenario == "deadline" && (time.Since(started) < 4*time.Second || time.Since(started) > 8*time.Second) {
				t.Fatal("follow-up acknowledgement did not honor its server-owned bound")
			}
			state := f.inspect(t)[0]
			if state.Done || state.Sequence != execution.Sequence || !reflect.DeepEqual(state.Conversation.Sources, []string{"alpha", "beta"}) {
				t.Fatal("follow-up replaced or completed its shared execution")
			}
			for retry := 0; retry < 2; retry++ {
				_, _ = f.client.Call(t.Context(), request)
			}
			f.stop(t)
			if err := f.owner.Release(f.term); err != nil {
				t.Fatal(err)
			}
			old := f.client
			f.open(t)
			defer old.Close()
			if f.client.Info().ID != identity.ID || f.client.Info().Process != identity.Process {
				t.Fatal("replacement did not attach the exact worker")
			}
			if _, err := old.Call(context.Background(), request); err == nil {
				t.Fatal("obsolete controller could follow up")
			}
			if _, err := f.client.Call(t.Context(), harness.WorkerRequest{Action: "interrupt", Key: execution.Key}); err != nil {
				t.Fatal(err)
			}
			f.start(t)
			waitWorkerAgent(t, func() bool { return len(f.inspect(t)) == 0 })
			entries, _, err := f.service.History.Entries(c.Channel, c.Conversation)
			if err != nil {
				t.Fatal(err)
			}
			terminals := 0
			for _, entry := range entries {
				if entry.ExecutionID == c.ExecutionID && len(entry.Covers) > 0 {
					terminals++
					if !reflect.DeepEqual(entry.Covers, []string{"alpha", "beta"}) {
						t.Fatal("native terminal lost follow-up source coverage")
					}
				}
			}
			if terminals != 1 {
				t.Fatalf("native terminal count = %d", terminals)
			}
			archived, _, err := f.service.Runtime.ArchivedJob(job.Number)
			if err != nil || archived.Generation != job.Generation {
				t.Fatal("attachment changed the public job or counters", err)
			}
			data, err := os.ReadFile(f.log)
			if err != nil || strings.Count(string(data), "turn/start\n") != 1 || strings.Count(string(data), "turn/steer\n") != 1 {
				t.Fatal("follow-up retry or attachment repeated provider delivery", err)
			}
		})
	}
}
