//go:build linux || darwin

package harness

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/agent0ai/spynel/internal/updater"
)

func TestWorkerUnavailableStartupRetainsOwnershipAndRecovers(t *testing.T) {
	for _, scenario := range []struct{ name, mode string }{
		{"codex", "codex-worker"}, {"claude-code", "claude-interrupt"}, {"pi", "pi-interrupt"}, {"acp", "acp-interrupt"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			var installed string
			fixture := newWorkerFixture(t, scenario.name, scenario.mode, func(cfg *HarnessConfig) {
				installed = cfg.Command
				cfg.Command = filepath.Join(cfg.Cwd, "not-yet-installed")
			})
			client, err := OpenWorker(t.Context(), fixture.config, fixture.owner.ID(), fixture.term)
			if err != nil {
				t.Fatal("unavailable adapter prevented authenticated discovery", err)
			}
			defer client.Close()
			identity := client.Info()
			t.Logf("unavailable worker pid=%d start=%s generation=%s", identity.PID, identity.Process.StartTime, identity.Process.Generation)
			inspection := workerClientCall(t, client, WorkerRequest{Action: "inspect"})
			if inspection.Availability == nil || inspection.Availability.Available || inspection.Availability.Detail == "" ||
				inspection.NextSequence != 1 || len(inspection.Executions) != 0 || inspection.ConfigDigest != ConfigDigest(fixture.config) {
				t.Fatal("startup failure invented readiness, admission or a different configuration")
			}
			lockPath, err := ProcessOwnershipPath(fixture.config.Cwd)
			if err != nil {
				t.Fatal(err)
			}
			if lock, err := lockProcessOwnership(lockPath); !errors.Is(err, errProcessOwnershipBusy) {
				if lock != nil {
					_ = lock.Close()
				}
				t.Fatal("unavailable worker did not retain exclusive harness ownership", err)
			}
			if _, err := os.Stat(fixture.log); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("unavailable startup ran a provider", err)
			}
			fixture.replaceOwner(t)
			if _, err := client.Call(t.Context(), WorkerRequest{Action: "inspect"}); err == nil {
				t.Fatal("unavailable adapter allowed an obsolete controller")
			}
			replacement, err := OpenWorker(t.Context(), fixture.config, fixture.owner.ID(), fixture.term)
			if err != nil {
				t.Fatal("successor could not adopt the unavailable worker", err)
			}
			defer replacement.Close()
			if replacement.Info().Process != identity.Process || replacement.Info().ID != identity.ID {
				t.Fatal("unavailable adoption replaced the worker")
			}
			if err := os.Symlink(installed, fixture.config.Command); err != nil {
				t.Fatal(err)
			}
			workerClientCall(t, replacement, WorkerRequest{Action: "configure", Config: &fixture.config, ExpectedConfig: identity.ConfigDigest})
			inspection = workerClientCall(t, replacement, WorkerRequest{Action: "inspect"})
			if inspection.Availability == nil || !inspection.Availability.Available || inspection.NextSequence != 1 || len(inspection.Executions) != 0 {
				t.Fatal("explicit configuration retry did not restore availability without admission")
			}
			if err := updater.VerifyProcess(identity.Process); err != nil {
				t.Fatal("recovery lost the original native worker", err)
			}
			workerClientCall(t, replacement, WorkerRequest{Action: "send", Sequence: 1, Key: "synthetic/startup", Prompt: "synthetic recovered turn"})
			workerClientCall(t, replacement, WorkerRequest{Action: "interrupt", Key: "synthetic/startup"})
			deadline := time.Now().Add(5 * time.Second)
			for !workerClientCall(t, replacement, WorkerRequest{Action: "inspect"}).Executions[0].Done {
				if time.Now().After(deadline) {
					t.Fatal("recovered provider did not complete interruption")
				}
				time.Sleep(time.Millisecond)
			}
		})
	}
}
