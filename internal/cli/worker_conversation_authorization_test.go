//go:build linux || darwin

package cli

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/agent0ai/spynel/internal/config"
	"github.com/agent0ai/spynel/internal/harness"
	"github.com/agent0ai/spynel/internal/history"
)

func TestWorkerConversationRevocationAfterJobAdmission(t *testing.T) {
	for _, channel := range []string{"telegram", "whatsapp"} {
		t.Run(channel, func(t *testing.T) {
			f := newWorkerAgentFixture(t)
			var err error
			f.cfg, err = f.service.Settings.Update(func(cfg *config.Config) error {
				cfg.Orchestrator.SemanticHeartbeatMinutes = 5
				cfg.Channels.Telegram.Enabled = true
				cfg.Channels.Telegram.AllowedUsers = []string{"7"}
				cfg.Channels.WhatsApp.Enabled = true
				cfg.Channels.WhatsApp.AllowedNumbers = []string{"15551234567"}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			f.service.SetConversationDelivery(&workerConversationRouter{})
			name := "TG-7"
			if channel == "whatsapp" {
				name = "WA-15551234567"
			}
			c := harness.WorkerConversation{Channel: channel, Conversation: name, SourceMessageID: "source", ExecutionID: "execution", Sources: []string{"source"}}
			if _, err := f.service.History.Append(channel, name, history.Entry{Role: "user", SourceMessageID: "source", Content: "synthetic private request", AcceptedAt: time.Now().UTC()}); err != nil {
				t.Fatal(err)
			}
			start := f.service.Orchestrator.ConversationStarted
			f.service.Orchestrator.ConversationStarted = func(c harness.WorkerConversation) (int, error) {
				id, err := start(c)
				if err != nil {
					return 0, err
				}
				f.cfg.Channels.Telegram.AllowedUsers = []string{"8"}
				f.cfg.Channels.WhatsApp.AllowedNumbers = []string{"15557654321"}
				return id, config.Save(f.cfg)
			}
			execution, err := f.service.Orchestrator.AdmitConversation(t.Context(), c, "synthetic private prompt", "synthetic private message")
			if err == nil || !execution.ConversationRefused() {
				t.Fatal("revocation after job admission reached provider Send", err)
			}
			identity := f.client.Info()
			t.Logf("authorization worker pid=%d start=%s generation=%s", identity.PID, identity.Process.StartTime, identity.Process.Generation)
			job, ok := f.service.Runtime.JobForSession(execution.Key)
			if !ok {
				t.Fatal("refusal lost its provisional job")
			}
			f.stop(t)
			if err := f.owner.Release(f.term); err != nil {
				t.Fatal(err)
			}
			f.open(t)
			if f.client.Info().Process != identity.Process || f.client.Info().ID != identity.ID {
				t.Fatal("replacement changed the refused worker")
			}
			f.service.Orchestrator.ConversationDelivery = func(context.Context, int, harness.WorkerDelivery) error {
				t.Error("refused authorization entered provider effects")
				return errors.New("unexpected conversation delivery")
			}
			if err := f.service.Orchestrator.ScanOnce(t.Context()); err != nil {
				t.Fatal(err)
			}
			f.service.Orchestrator.Wait()
			if len(f.inspect(t)) != 0 || len(f.service.Runtime.Jobs()) != 0 {
				t.Fatal("refused authorization was not retired")
			}
			archived, output, err := f.service.Runtime.ArchivedJob(job.StableID)
			if err != nil || archived.Number != job.Number || archived.Generation != job.Generation || strings.Count(output, "Conversation source reservation refused") != 1 {
				t.Fatal("refusal lost its original archive", err)
			}
			entries, _, err := f.service.History.Entries(channel, name)
			if err != nil || len(entries) != 1 || entries[0].Role != "user" {
				t.Fatal("authorization refusal fabricated source admission or a provider response", err)
			}
			data, err := os.ReadFile(f.log)
			if err != nil || strings.Contains(string(data), "turn/start\n") {
				t.Fatal("revoked authorization reached the synthetic provider", err)
			}
		})
	}
}
