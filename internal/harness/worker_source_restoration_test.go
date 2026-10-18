//go:build linux || darwin

package harness

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"testing"

	"github.com/agent0ai/spynel/internal/core"
)

func TestWorkerSourceRestorationRequiresProvenOrdinaryRejection(t *testing.T) {
	for _, scenario := range []string{"rejected", "uncertain", "recovery", "storage-failure"} {
		t.Run(scenario, func(t *testing.T) {
			root := t.TempDir()
			rejection := &SendNotAdmittedError{Err: errors.New("synthetic rejection")}
			target := &workerSendErrorHarness{
				supervisorHarness: &supervisorHarness{active: map[string]bool{}, emits: map[string]core.Emit{}},
				returnErr:         fmt.Errorf("wrapped: %w", rejection), beforeReturn: func(core.Emit) {},
			}
			if scenario == "uncertain" {
				target.returnErr = errors.New("synthetic lost acknowledgement")
			}
			supervisor := NewSupervisor(NewRegistry(), HarnessConfig{Cwd: root})
			supervisor.current, supervisor.ctx = target, context.Background()
			host := &workerHost{supervisor: supervisor, info: WorkerInfo{ID: "synthetic-worker"}, directory: root,
				next: 1, executions: map[uint64]*workerExecution{}, stop: make(chan struct{})}
			t.Cleanup(func() {
				for _, execution := range host.executions {
					_ = execution.file.Close()
				}
			})
			conversation := &WorkerConversation{Channel: "cli", Conversation: "owned", ExecutionID: "execution",
				SourceMessageID: "alpha", Sources: []string{"alpha", "beta"}, Recovery: scenario == "recovery"}
			reservations, restorations := 0, 0
			host.conversation = func(gotRoot, key string, got WorkerConversation, receipt string, sendErr error) error {
				if gotRoot != root || key != "chat:cli:owned" || !reflect.DeepEqual(got, *conversation) || receipt != "synthetic-worker:1:admission" {
					t.Fatal("source action changed the exact admission identity or whole batch")
				}
				if sendErr == nil {
					reservations++
					return nil
				}
				var proof *SendNotAdmittedError
				if !errors.As(sendErr, &proof) || proof != rejection || conversation.Recovery || host.executions[1].LastCursor != 0 {
					t.Fatal("restoration lacked proof or followed terminal publication")
				}
				restorations++
				if scenario == "storage-failure" {
					return &os.PathError{Op: "open", Path: root + "/private-source", Err: os.ErrPermission}
				}
				return nil
			}
			request := WorkerRequest{Sequence: 1, Key: "chat:cli:owned", Prompt: "synthetic", Conversation: conversation}
			reply := host.send(request)
			execution := host.executions[1]
			if reservations != 1 || target.sends != 1 || !execution.SourcesReserved {
				t.Fatal("source restoration changed initial reservation or provider admission")
			}
			if scenario == "storage-failure" {
				if reply.Error != "restore worker conversation admission failed" || host.failure == nil || execution.Done || execution.LastCursor != 0 || restorations != 1 {
					t.Fatal("failed restoration published terminal completion or private storage details")
				}
				select {
				case <-host.stop:
				default:
					t.Fatal("failed restoration did not stop the worker")
				}
				return
			}
			if scenario == "uncertain" {
				if reply.Error == "" || execution.Done || restorations != 0 {
					t.Fatal("uncertain delivery restored sources or manufactured completion")
				}
			} else if reply.Error != "" || !execution.Done || restorations != map[bool]int{true: 0, false: 1}[conversation.Recovery] {
				t.Fatal("ordinary rejection restoration or recovery reservation policy changed")
			}
			cursor := execution.LastCursor
			_ = host.send(request)
			if reservations != 1 || target.sends != 1 || execution.LastCursor != cursor || restorations > 1 {
				t.Fatal("exact retry repeated reservation, restoration, Send or terminal publication")
			}
		})
	}
}
