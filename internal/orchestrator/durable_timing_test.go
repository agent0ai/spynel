package orchestrator

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"
)

func TestClaimAndProviderTurnsPreserveDurableTiming(t *testing.T) {
	directory := t.TempDir()
	source := filepath.Join(directory, "todo", "task.md")
	target := filepath.Join(directory, "working", "task.md")
	if err := os.MkdirAll(filepath.Dir(source), 0o700); err != nil {
		t.Fatal(err)
	}
	document := Document{FrontMatter: map[string]any{
		"id": "task", "status": "todo", "attempt": 4, "custom": "preserved",
	}, Body: "# Task\n"}
	if err := WriteDocument(source, document); err != nil {
		t.Fatal(err)
	}
	assigned := time.Date(2026, 8, 8, 10, 0, 0, 0, time.UTC)
	claimed, err := ClaimDocument(source, target, "working", assigned)
	if err != nil {
		t.Fatal(err)
	}
	if claimed.FrontMatter["first_assigned_at"] != assigned.Format(time.RFC3339) || claimed.FrontMatter["attempt"] != 5 || claimed.FrontMatter["custom"] != "preserved" {
		t.Fatalf("claimed front matter = %#v", claimed.FrontMatter)
	}
	for want := 1; want <= 3; want++ {
		first, iterations, err := ReserveProviderTurn(target, assigned.Add(time.Duration(want)*time.Minute))
		if err != nil || !first.Equal(assigned) || iterations != want {
			t.Fatalf("turn %d = %v, %d, %v", want, first, iterations, err)
		}
	}
	stored, err := ReadDocument(target)
	if err != nil {
		t.Fatal(err)
	}
	first, iterations, ok := DurableTiming(stored)
	if !ok || !first.Equal(assigned) || iterations != 3 || stored.FrontMatter["attempt"] != 5 {
		t.Fatalf("durable timing = %v, %d, %t; front matter %#v", first, iterations, ok, stored.FrontMatter)
	}
	review := filepath.Join(directory, "review", "task.md")
	reviewing := filepath.Join(directory, "reviewing", "task.md")
	if err := os.MkdirAll(filepath.Dir(review), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(target, review); err != nil {
		t.Fatal(err)
	}
	if _, err := claimDocument(review, reviewing, "reviewing", "review_attempt", assigned.Add(10*time.Minute)); err != nil {
		t.Fatal(err)
	}
	reviewed, err := ReadDocument(reviewing)
	if err != nil {
		t.Fatal(err)
	}
	if reviewed.FrontMatter["attempt"] != 5 || reviewed.FrontMatter["review_attempt"] != 1 {
		t.Fatalf("review claim counters = %#v", reviewed.FrontMatter)
	}
	if first, iterations, err := ReserveProviderTurn(reviewing, assigned.Add(11*time.Minute)); err != nil || !first.Equal(assigned) || iterations != 4 {
		t.Fatalf("review turn = %v, %d, %v", first, iterations, err)
	}
	rework := filepath.Join(directory, "todo", "task.md")
	if err := os.Rename(reviewing, rework); err != nil {
		t.Fatal(err)
	}
	if _, err := ClaimDocument(rework, target, "working", assigned.Add(20*time.Minute)); err != nil {
		t.Fatal(err)
	}
	reworked, err := ReadDocument(target)
	if err != nil {
		t.Fatal(err)
	}
	if reworked.FrontMatter["attempt"] != 6 || reworked.FrontMatter["review_attempt"] != 1 {
		t.Fatalf("rework claim counters = %#v", reworked.FrontMatter)
	}
	if first, iterations, err := ReserveProviderTurn(target, assigned.Add(21*time.Minute)); err != nil || !first.Equal(assigned) || iterations != 5 {
		t.Fatalf("rework turn = %v, %d, %v", first, iterations, err)
	}
}

func TestLegacyAndCorruptDurableTimingMigratesOnlyOnRealWork(t *testing.T) {
	now := time.Date(2026, 8, 8, 11, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name  string
		front map[string]any
	}{
		{name: "legacy", front: map[string]any{"created_at": "2020-01-01T00:00:00Z"}},
		{name: "corrupt", front: map[string]any{"first_assigned_at": "not-a-time", "provider_iterations": -7}},
		{name: "future", front: map[string]any{"first_assigned_at": now.Add(time.Hour).Format(time.RFC3339)}},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "task.md")
			if err := WriteDocument(path, Document{FrontMatter: test.front, Body: "body\n"}); err != nil {
				t.Fatal(err)
			}
			before, err := ReadDocument(path)
			if err != nil {
				t.Fatal(err)
			}
			if test.name == "legacy" {
				if _, exists := before.FrontMatter["first_assigned_at"]; exists {
					t.Fatal("reading legacy document falsely initialized first assignment")
				}
			}
			first, iterations, err := ReserveProviderTurn(path, now)
			if err != nil || !first.Equal(now) || iterations != 1 {
				t.Fatalf("migration = %v, %d, %v", first, iterations, err)
			}
		})
	}
}

func TestPhaseSpecificClaimIncrementsOnlyItsOwnAttempt(t *testing.T) {
	directory := t.TempDir()
	source := filepath.Join(directory, "review", "task.md")
	target := filepath.Join(directory, "reviewing", "task.md")
	if err := os.MkdirAll(filepath.Dir(source), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := WriteDocument(source, Document{FrontMatter: map[string]any{
		"id": "task", "status": "review", "attempt": 1, "review_attempt": 2,
	}, Body: "# Task\n"}); err != nil {
		t.Fatal(err)
	}
	claimed, err := claimDocument(source, target, "reviewing", "review_attempt", time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if claimed.FrontMatter["attempt"] != 1 || claimed.FrontMatter["review_attempt"] != 3 {
		t.Fatalf("phase-specific attempts = %#v", claimed.FrontMatter)
	}
}

func TestProviderTurnReservationIsRaceSafeAndMonotonic(t *testing.T) {
	path := filepath.Join(t.TempDir(), "task.md")
	assigned := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	if err := WriteDocument(path, Document{FrontMatter: map[string]any{
		"first_assigned_at": assigned.Format(time.RFC3339),
	}, Body: "body\n"}); err != nil {
		t.Fatal(err)
	}
	const turns = 32
	counts := make(chan int, turns)
	var wait sync.WaitGroup
	for index := 0; index < turns; index++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			_, count, err := ReserveProviderTurn(path, assigned.Add(time.Hour))
			if err != nil {
				t.Errorf("reserve: %v", err)
				return
			}
			counts <- count
		}()
	}
	wait.Wait()
	close(counts)
	seen := make(map[int]bool, turns)
	for count := range counts {
		seen[count] = true
	}
	for want := 1; want <= turns; want++ {
		if !seen[want] {
			t.Fatalf("missing monotonic count %d: %#v", want, seen)
		}
	}
}

func TestProviderTurnReservationIsSafeAcrossOwners(t *testing.T) {
	path := filepath.Join(t.TempDir(), "task.md")
	assigned := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	if err := WriteDocument(path, Document{FrontMatter: map[string]any{
		"first_assigned_at": assigned.Format(time.RFC3339),
	}, Body: "body\n"}); err != nil {
		t.Fatal(err)
	}
	const owners = 16
	commands := make([]*exec.Cmd, owners)
	outputs := make([]bytes.Buffer, owners)
	for index := range commands {
		commands[index] = exec.Command(os.Args[0], "-test.run=^TestProviderTurnReservationOwnerHelper$")
		commands[index].Stdout = &outputs[index]
		commands[index].Stderr = &outputs[index]
		commands[index].Env = append(os.Environ(),
			"SPYNEL_PROVIDER_TURN_HELPER=1",
			"SPYNEL_PROVIDER_TURN_PATH="+path,
			"SPYNEL_PROVIDER_TURN_NOW="+assigned.Add(time.Hour).Format(time.RFC3339Nano),
		)
		if err := commands[index].Start(); err != nil {
			t.Fatal(err)
		}
	}
	for index, command := range commands {
		if err := command.Wait(); err != nil {
			t.Fatalf("provider owner failed: %v\n%s", err, outputs[index].String())
		}
	}
	document, err := ReadDocument(path)
	if err != nil {
		t.Fatal(err)
	}
	_, iterations, ok := DurableTiming(document)
	if !ok || iterations != owners {
		t.Fatalf("cross-owner provider iterations = %d, want %d", iterations, owners)
	}
}

func TestProviderTurnReservationReconcilesConcurrentAgentMove(t *testing.T) {
	directory := t.TempDir()
	working := filepath.Join(directory, "working", "task.md")
	review := filepath.Join(directory, "review", "task.md")
	if err := os.MkdirAll(filepath.Dir(working), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(review), 0o700); err != nil {
		t.Fatal(err)
	}
	assigned := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	if err := WriteDocument(working, Document{FrontMatter: map[string]any{
		"id": "task", "status": "working", "first_assigned_at": assigned.Format(time.RFC3339),
	}, Body: "working body\n"}); err != nil {
		t.Fatal(err)
	}

	first, iterations, err := reserveProviderTurn(working, assigned.Add(time.Hour), func() {
		document, readErr := ReadDocument(working)
		if readErr != nil {
			t.Fatal(readErr)
		}
		document.FrontMatter["status"] = "review"
		document.Body = "moved body\n"
		if writeErr := WriteDocument(working, document); writeErr != nil {
			t.Fatal(writeErr)
		}
		if renameErr := os.Rename(working, review); renameErr != nil {
			t.Fatal(renameErr)
		}
	})
	if err != nil || !first.Equal(assigned) || iterations != 1 {
		t.Fatalf("reservation = %v, %d, %v", first, iterations, err)
	}
	if _, err := os.Stat(working); !os.IsNotExist(err) {
		t.Fatalf("stale working copy remains: %v", err)
	}
	moved, err := ReadDocument(review)
	if err != nil {
		t.Fatal(err)
	}
	_, count, ok := DurableTiming(moved)
	if !ok || count != 1 || moved.Body != "moved body\n" || moved.FrontMatter["status"] != "review" {
		t.Fatalf("moved document = %#v, body %q", moved.FrontMatter, moved.Body)
	}
}

func TestProviderTurnReservationOwnerHelper(t *testing.T) {
	if enabled, _ := strconv.ParseBool(os.Getenv("SPYNEL_PROVIDER_TURN_HELPER")); !enabled {
		return
	}
	now, err := time.Parse(time.RFC3339Nano, os.Getenv("SPYNEL_PROVIDER_TURN_NOW"))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := ReserveProviderTurn(os.Getenv("SPYNEL_PROVIDER_TURN_PATH"), now); err != nil {
		t.Fatal(err)
	}
}
