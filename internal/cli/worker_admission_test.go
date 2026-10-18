//go:build linux || darwin

package cli

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agent0ai/spynel/internal/harness"
	"github.com/agent0ai/spynel/internal/instance"
	"github.com/agent0ai/spynel/internal/orchestrator"
	"github.com/agent0ai/spynel/internal/updater"
)

func runWorkerAdmissionProvider() int {
	file, err := os.OpenFile(os.Getenv("SPYNEL_TEST_WORKER_EVIDENCE"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return 1
	}
	defer file.Close()
	var outputMu sync.Mutex
	write := func(value any) error {
		outputMu.Lock()
		defer outputMu.Unlock()
		return json.NewEncoder(os.Stdout).Encode(value)
	}
	starts := 0
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		var request struct {
			ID     int
			Method string
			Params json.RawMessage
		}
		if json.Unmarshal(scanner.Bytes(), &request) != nil {
			return 1
		}
		if _, err := file.WriteString(request.Method + "\n"); err != nil {
			return 1
		}
		result := map[string]any{}
		switch request.Method {
		case "initialize":
		case "turn/interrupt":
			if release := os.Getenv("SPYNEL_TEST_WORKER_DELAY_INTERRUPT"); release != "" {
				for {
					if _, err := os.Stat(release); err == nil {
						break
					}
					time.Sleep(10 * time.Millisecond)
				}
			}
		case "model/list":
			result["data"] = []any{map[string]any{"id": "model-a", "model": "model-a", "isDefault": true, "supportedReasoningEfforts": []any{map[string]string{"reasoningEffort": "high"}}, "serviceTiers": []any{map[string]string{"id": "priority", "name": "Priority"}}}}
		case "turn/steer":
			result["turnId"] = "synthetic-turn"
		case "thread/start", "thread/resume":
			result["thread"] = map[string]string{"id": "synthetic-thread"}
		case "turn/start":
			if os.Getenv("SPYNEL_TEST_WORKER_REJECT_START") == "1" {
				if write(map[string]any{"id": request.ID, "error": map[string]any{"code": -32000, "message": "synthetic provider admission refused"}}) != nil {
					return 1
				}
				continue
			}
			result["turn"] = map[string]string{"id": "synthetic-turn", "status": "inProgress"}
		default:
			continue
		}
		if write(map[string]any{"id": request.ID, "result": result}) != nil {
			return 1
		}
		if request.Method == "turn/interrupt" {
			turn := map[string]any{"id": "synthetic-turn", "status": "interrupted"}
			if os.Getenv("SPYNEL_TEST_WORKER_CONVERSATION_ERROR") == "1" {
				turn["status"], turn["error"] = "failed", map[string]string{"message": "synthetic conversation failure"}
			}
			if write(map[string]any{"method": "turn/completed", "params": map[string]any{"threadId": "synthetic-thread", "turn": turn}}) != nil {
				return 1
			}
		}
		if request.Method == "turn/start" {
			if mode := os.Getenv("SPYNEL_TEST_WORKER_CONVERSATION_ERROR"); mode != "" {
				partial := "retained partial"
				if mode == "2" {
					partial += "\n[Send attachment](<missing>)"
				}
				if write(map[string]any{"method": "item/agentMessage/delta", "params": map[string]any{"threadId": "synthetic-thread", "turnId": "synthetic-turn", "delta": partial}}) != nil {
					return 1
				}
			}
			if _, err := file.WriteString("selection:" + string(request.Params) + "\n"); err != nil {
				return 1
			}
			starts++
			if prefix := os.Getenv("SPYNEL_TEST_WORKER_COMPLETIONS"); prefix != "" {
				go func(path string) {
					for tick := 0; tick < 3000; tick++ {
						if _, err := os.Stat(path); err == nil {
							_ = write(map[string]any{"method": "turn/completed", "params": map[string]any{"threadId": "synthetic-thread", "turn": map[string]string{"id": "synthetic-turn", "status": "completed"}}})
							return
						}
						time.Sleep(10 * time.Millisecond)
					}
				}(prefix + "." + strconv.Itoa(starts))
			}
		}
	}
	return 0
}

func TestWorkerWorkflowReservationThroughNativeEntry(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(root, ".spynel")
	owner, err := instance.New(state)
	if err != nil {
		t.Fatal(err)
	}
	term, _ := owner.NewToken()
	if _, ok, err := owner.TryAcquire("127.0.0.1:10001", term); err != nil || !ok {
		t.Fatalf("owner: %v", err)
	}
	work := &harness.WorkerWorkflow{WorkID: "synthetic", OwnerID: "synthetic-manager", Route: "tasks", Phase: "task_implementation", ClaimAttempt: 2}
	digest := sha256.Sum256([]byte("tasks:task_implementation\x00synthetic"))
	work.LeaseID, work.ClaimID = hex.EncodeToString(digest[:12]), hex.EncodeToString(digest[:12])
	work.File = filepath.Join(state, "tasks", "working", "synthetic.md")
	if err := orchestrator.WriteDocument(work.File, orchestrator.Document{FrontMatter: map[string]any{"id": work.WorkID, "status": "working", "attempt": 2, "provider_iterations": 4}, Body: "# Synthetic work\n"}); err != nil {
		t.Fatal(err)
	}
	key := "orchestrator:tasks:task_implementation:synthetic:2"
	lease := orchestrator.Lease{ID: work.LeaseID, ClaimID: work.ClaimID, OwnerID: work.OwnerID, File: work.File, Route: work.Route, Phase: work.Phase, ClaimAttempt: work.ClaimAttempt, SessionKey: key, State: "processing"}
	data, _ := json.Marshal(lease)
	if err := os.MkdirAll(filepath.Join(state, "runtime", "leases"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(state, "runtime", "leases", work.LeaseID+".json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	executable, _ := os.Executable()
	log := filepath.Join(root, "synthetic-provider-methods")
	t.Setenv("SPYNEL_TEST_WORKER_PROVIDER", "1")
	t.Setenv("SPYNEL_TEST_WORKER_EVIDENCE", log)
	cfg := harness.HarnessConfig{Name: "codex", Command: executable, Cwd: root, Sandbox: "read-only", ApprovalPolicy: "never", SessionsFile: filepath.Join(root, "sessions.json")}
	client, err := harness.OpenWorker(t.Context(), cfg, owner.ID(), term)
	if err != nil {
		_ = owner.Release(term)
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := client.Call(context.Background(), harness.WorkerRequest{Action: "shutdown"}); err != nil {
			t.Error(err)
		}
		client.Close()
		_ = owner.Release(term)
		deadline := time.Now().Add(10 * time.Second)
		for {
			if _, err := os.Stat(filepath.Dir(client.Info().Socket)); os.IsNotExist(err) {
				break
			}
			if time.Now().After(deadline) {
				t.Error("worker did not clean up its endpoint/journals")
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
	})
	call := func(request harness.WorkerRequest) harness.WorkerReply {
		t.Helper()
		reply, err := client.Call(t.Context(), request)
		if err != nil {
			t.Fatal(err)
		}
		return reply
	}
	request := harness.WorkerRequest{Action: "send", Sequence: 1, Key: key, Prompt: "synthetic workflow admission", Workflow: work}
	first := call(request).Executions[0]
	if first.Error != "" || first.Reservation == nil || first.Reservation.ProviderIterations != 5 || first.Reservation.ImplementationAttempts != 2 {
		t.Fatalf("native reservation: %+v", first)
	}
	identity, err := updater.ProcessRecord(client.Info().PID)
	if err != nil {
		t.Fatal(err)
	}
	obsolete := client
	if err := owner.Release(term); err != nil {
		t.Fatal(err)
	}
	owner, err = instance.New(state)
	if err != nil {
		t.Fatal(err)
	}
	term, _ = owner.NewToken()
	if _, ok, err := owner.TryAcquire("127.0.0.1:10002", term); err != nil || !ok {
		t.Fatalf("replacement: %v", err)
	}
	client, err = harness.OpenWorker(t.Context(), cfg, owner.ID(), term)
	if err != nil {
		t.Fatal(err)
	}
	defer obsolete.Close()
	if _, err := obsolete.Call(t.Context(), request); err == nil {
		t.Fatal("obsolete owner reached admission")
	}
	retained := call(harness.WorkerRequest{Action: "inspect"}).Executions[0]
	if retained.Reservation == nil || *retained.Reservation != *first.Reservation || retained.ThreadID != first.ThreadID {
		t.Fatal("native attachment lost the admitted reservation")
	}
	call(request)
	duplicate := request
	duplicate.Sequence = 2
	if _, err := client.Call(t.Context(), duplicate); err == nil {
		t.Fatal("a new sequence duplicated the retained workflow")
	}
	if err := updater.VerifyProcess(identity); err != nil || identity.PID != client.Info().PID {
		t.Fatal("attachment replaced the native worker")
	}
	changed := *work
	changed.OwnerID = "replacement-manager-without-claim"
	changedKey := "orchestrator:tasks:task_implementation:synthetic:3"
	changed.ClaimAttempt = 3
	rejected := call(harness.WorkerRequest{Action: "send", Sequence: 2, Key: changedKey, Prompt: "synthetic rejected claim", Workflow: &changed}).Executions[0]
	if rejected.Error == "" || !rejected.Done || rejected.Reservation != nil {
		t.Fatal("changed claim dispatched")
	}
	document, err := orchestrator.ReadDocument(work.File)
	if err != nil || document.FrontMatter["provider_iterations"] != 5 {
		t.Fatalf("attachment/rejection inflated iterations: %+v, %v", document, err)
	}
	data, err = os.ReadFile(log)
	if err != nil || strings.Count(string(data), "turn/start\n") != 1 {
		t.Fatalf("native provider dispatch count: %q, %v", data, err)
	}
	t.Logf("same native worker PID=%d start=%s generation=%s; one actual provider admission and one durable workflow reservation across replacement", identity.PID, identity.StartTime, identity.Generation)
}
