package instance

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// Exercise the real OS lock and real stale interval in separate processes.
func TestOwnerActionRetainsTermAcrossActualProcessContention(t *testing.T) {
	state := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestElectionOwnerProcessFixture$")
	command.Env = append(os.Environ(), "SPYNEL_ELECTION_TEST_STATE="+state)
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = command.Process.Kill(); _ = command.Wait() }()
	for {
		if _, err := os.Stat(filepath.Join(state, "admission-started")); err == nil {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("owner fixture never entered admission")
		case <-time.After(10 * time.Millisecond):
		}
	}
	contender, err := NewWithEnvironmentID(state, testEnvironmentID("a"))
	if err != nil {
		t.Fatal(err)
	}
	original, err := contender.Current()
	if err != nil {
		t.Fatal(err)
	}
	var observation Observation
	contender.SetObserver(func(value Observation) { observation = value })
	token, _ := contender.NewToken()
	current, acquired, err := contender.TryAcquire("127.0.0.1:10002", token)
	if err != nil || acquired || current.InstanceID != original.InstanceID || current.Token != original.Token || contender.IsStale(current) {
		t.Fatalf("blocked contender displaced an active admission: acquired=%t err=%v", acquired, err)
	}
	if observation.Operation != "acquire" || observation.LockWait < StaleAfter-2*time.Second {
		t.Fatalf("real lock delay was not observed: %#v", observation)
	}
	if err := os.WriteFile(filepath.Join(state, "admission-observed"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := command.Wait(); err != nil {
		t.Fatalf("owner fixture failed: %v", err)
	}
	if _, err := contender.Current(); !os.IsNotExist(err) {
		t.Fatalf("fixture did not release ownership: %v", err)
	}
}

func TestElectionOwnerProcessFixture(t *testing.T) {
	state := os.Getenv("SPYNEL_ELECTION_TEST_STATE")
	if state == "" {
		return
	}
	owner, err := NewWithEnvironmentID(state, testEnvironmentID("a"))
	if err != nil {
		t.Fatal(err)
	}
	token, _ := owner.NewToken()
	if _, acquired, err := owner.TryAcquire("127.0.0.1:10001", token); err != nil || !acquired {
		t.Fatalf("fixture acquire = %t, %v", acquired, err)
	}
	defer owner.Release(token)
	if ran, err := owner.RunWhileOwner(token, func() error {
		if err := os.WriteFile(filepath.Join(state, "admission-started"), nil, 0600); err != nil {
			return err
		}
		time.Sleep(StaleAfter + time.Second)
		return nil
	}); err != nil || !ran {
		t.Fatalf("fixture admission = %t, %v", ran, err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(filepath.Join(state, "admission-observed")); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("contender never acknowledged admission")
}
