package instance

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func testEnvironmentID(character string) string { return strings.Repeat(character, 64) }

func TestEnvironmentIDIsStablePrivateAndEnvironmentScoped(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	first, err := EnvironmentID()
	if err != nil {
		t.Fatal(err)
	}
	second, err := EnvironmentID()
	if err != nil || second != first || !validEnvironmentID(first) {
		t.Fatalf("stable environment ID = %q, %q, %v", first, second, err)
	}
	configDirectory, _ := os.UserConfigDir()
	tokenPath := filepath.Join(configDirectory, "spynel", "environment-token")
	token, err := os.ReadFile(tokenPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(first, strings.TrimSpace(string(token))) {
		t.Fatal("published environment ID exposed its private source token")
	}
	info, err := os.Stat(tokenPath)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("environment token permissions = %v, %v", info, err)
	}
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	separate, err := EnvironmentID()
	if err != nil || separate == first {
		t.Fatalf("separate configuration environment ID = %q, first %q, %v", separate, first, err)
	}
}

func TestLeasePublishesEnvironmentAndLegacyOwnerRemainsFenced(t *testing.T) {
	state := t.TempDir()
	owner, err := NewWithEnvironmentID(state, testEnvironmentID("a"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 11, 5, 0, 0, 0, time.UTC)
	owner.now = func() time.Time { return now }
	token, _ := owner.NewToken()
	lease, acquired, err := owner.TryAcquire("127.0.0.1:10001", token)
	if err != nil || !acquired || lease.EnvironmentID != testEnvironmentID("a") {
		t.Fatalf("environment lease = %#v, %t, %v", lease, acquired, err)
	}

	lease.EnvironmentID = "obsolete-or-invalid"
	data, _ := json.Marshal(lease)
	if err := os.WriteFile(owner.leasePath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	contender, err := NewWithEnvironmentID(state, testEnvironmentID("b"))
	if err != nil {
		t.Fatal(err)
	}
	contender.now = func() time.Time { return now }
	legacy, err := contender.Current()
	if err != nil || legacy.EnvironmentID != "" || contender.CanTakeOver(legacy) {
		t.Fatalf("legacy lease safety = %#v, %v", legacy, err)
	}
	contenderToken, _ := contender.NewToken()
	if _, acquired, err := contender.TryAcquire("127.0.0.1:10002", contenderToken); err != nil || acquired {
		t.Fatalf("fresh legacy owner takeover = %t, %v", acquired, err)
	}
	contender.now = func() time.Time { return now.Add(StaleAfter) }
	replacement, acquired, err := contender.TryAcquire("127.0.0.1:10002", contenderToken)
	if err != nil || !acquired || replacement.EnvironmentID != testEnvironmentID("b") {
		t.Fatalf("stale legacy replacement = %#v, %t, %v", replacement, acquired, err)
	}
}

func TestRenewAndHandoffRestoreValidatedEnvironmentID(t *testing.T) {
	state := t.TempDir()
	owner, err := NewWithEnvironmentID(state, testEnvironmentID("a"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 11, 5, 0, 0, 0, time.UTC)
	owner.now = func() time.Time { return now }
	token, _ := owner.NewToken()
	lease, acquired, err := owner.TryAcquire("127.0.0.1:10001", token)
	if err != nil || !acquired {
		t.Fatalf("owner acquire = %#v, %t, %v", lease, acquired, err)
	}

	lease.EnvironmentID = "damaged"
	data, _ := json.Marshal(lease)
	if err := os.WriteFile(owner.leasePath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	now = now.Add(HeartbeatInterval)
	renewed, owned, err := owner.Renew(token)
	if err != nil || !owned || renewed.EnvironmentID != testEnvironmentID("a") {
		t.Fatalf("renewed environment = %#v, %t, %v", renewed, owned, err)
	}

	target, err := NewWithEnvironmentID(state, testEnvironmentID("a"))
	if err != nil {
		t.Fatal(err)
	}
	renewed.EnvironmentID = ""
	data, _ = json.Marshal(renewed)
	if err := os.WriteFile(owner.leasePath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	now = now.Add(HeartbeatInterval)
	handoff, handedOff, err := owner.Handoff(token, target.ID())
	if err != nil || !handedOff || handoff.EnvironmentID != testEnvironmentID("a") {
		t.Fatalf("handoff environment = %#v, %t, %v", handoff, handedOff, err)
	}
}

func TestOnlyOneConcurrentContenderAcquiresLease(t *testing.T) {
	state := t.TempDir()
	const contenders = 24
	elections := make([]*Election, contenders)
	for index := range elections {
		var err error
		elections[index], err = New(state)
		if err != nil {
			t.Fatal(err)
		}
	}
	start := make(chan struct{})
	var wait sync.WaitGroup
	results := make(chan Lease, contenders)
	for index, election := range elections {
		wait.Add(1)
		go func(index int, election *Election) {
			defer wait.Done()
			<-start
			token, err := election.NewToken()
			if err != nil {
				t.Errorf("token: %v", err)
				return
			}
			lease, acquired, err := election.TryAcquire("127.0.0.1:"+time.Now().Add(time.Duration(index)).Format("150405.000000000"), token)
			if err != nil {
				t.Errorf("acquire: %v", err)
				return
			}
			if acquired {
				results <- lease
			}
		}(index, election)
	}
	close(start)
	wait.Wait()
	close(results)
	var winners []Lease
	for lease := range results {
		winners = append(winners, lease)
	}
	if len(winners) != 1 {
		t.Fatalf("winners = %d, want exactly one: %#v", len(winners), winners)
	}
	current, err := elections[0].Current()
	if err != nil {
		t.Fatal(err)
	}
	if current.InstanceID != winners[0].InstanceID || current.Token != winners[0].Token {
		t.Fatalf("published lease = %#v, winner = %#v", current, winners[0])
	}
}

func TestOnlyOneConcurrentContenderWinsStaleTakeover(t *testing.T) {
	state := t.TempDir()
	startTime := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	owner, err := New(state)
	if err != nil {
		t.Fatal(err)
	}
	owner.now = func() time.Time { return startTime }
	ownerToken, _ := owner.NewToken()
	if _, acquired, err := owner.TryAcquire("127.0.0.1:9000", ownerToken); err != nil || !acquired {
		t.Fatalf("initial acquire = %t, %v", acquired, err)
	}

	const contenders = 24
	start := make(chan struct{})
	winners := make(chan Lease, contenders)
	var wait sync.WaitGroup
	for index := 0; index < contenders; index++ {
		candidate, err := New(state)
		if err != nil {
			t.Fatal(err)
		}
		candidate.now = func() time.Time { return startTime.Add(StaleAfter) }
		wait.Add(1)
		go func(index int, candidate *Election) {
			defer wait.Done()
			<-start
			token, _ := candidate.NewToken()
			lease, acquired, err := candidate.TryAcquire(fmt.Sprintf("127.0.0.1:%d", 10000+index), token)
			if err != nil {
				t.Errorf("takeover: %v", err)
				return
			}
			if acquired {
				winners <- lease
			}
		}(index, candidate)
	}
	close(start)
	wait.Wait()
	close(winners)
	count := 0
	for range winners {
		count++
	}
	if count != 1 {
		t.Fatalf("stale takeover winners = %d, want 1", count)
	}
	if _, owned, err := owner.Renew(ownerToken); err != nil || owned {
		t.Fatalf("stale owner renewed after takeover = %t, %v", owned, err)
	}
}

func TestStaleTakeoverFencesFormerOwner(t *testing.T) {
	state := t.TempDir()
	first, err := New(state)
	if err != nil {
		t.Fatal(err)
	}
	second, err := New(state)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	first.now = func() time.Time { return now }
	second.now = func() time.Time { return now }
	firstToken, _ := first.NewToken()
	firstLease, acquired, err := first.TryAcquire("127.0.0.1:10001", firstToken)
	if err != nil || !acquired {
		t.Fatalf("first acquire = %#v, %t, %v", firstLease, acquired, err)
	}
	if firstLease.PID != os.Getpid() || firstLease.InstanceID == "" || firstLease.Token == "" {
		t.Fatalf("owner identity was not persisted: %#v", firstLease)
	}
	info, err := os.Stat(filepath.Join(state, "runtime", "primary.json"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("lease permissions = %v, %v", info, err)
	}
	secondToken, _ := second.NewToken()
	if _, acquired, err := second.TryAcquire("127.0.0.1:10002", secondToken); err != nil || acquired {
		t.Fatalf("takeover before stale acquired = %t, err = %v", acquired, err)
	}
	now = now.Add(StaleAfter)
	if _, owned, err := first.Renew(firstToken); err != nil || owned {
		t.Fatalf("owner renewed an already stale term = %t, err = %v", owned, err)
	}
	secondLease, acquired, err := second.TryAcquire("127.0.0.1:10002", secondToken)
	if err != nil || !acquired {
		t.Fatalf("stale takeover = %#v, %t, %v", secondLease, acquired, err)
	}
	if _, owned, err := first.Renew(firstToken); err != nil || owned {
		t.Fatalf("former owner renewed = %t, err = %v", owned, err)
	}
	if err := first.Release(firstToken); err != nil {
		t.Fatal(err)
	}
	current, err := second.Current()
	if err != nil {
		t.Fatal(err)
	}
	if current.InstanceID != second.ID() || current.Token != secondToken {
		t.Fatalf("former owner disturbed successor: %#v", current)
	}
}

func TestRenewalKeepsLeaseFreshAndReleaseIsImmediate(t *testing.T) {
	election, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	election.now = func() time.Time { return now }
	token, _ := election.NewToken()
	if _, acquired, err := election.TryAcquire("127.0.0.1:10001", token); err != nil || !acquired {
		t.Fatalf("acquire = %t, %v", acquired, err)
	}
	now = now.Add(HeartbeatInterval)
	lease, owned, err := election.Renew(token)
	if err != nil || !owned || !lease.HeartbeatAt.Equal(now) || !validEnvironmentID(lease.EnvironmentID) {
		t.Fatalf("renew = %#v, %t, %v", lease, owned, err)
	}
	if err := election.Release(token); err != nil {
		t.Fatal(err)
	}
	if _, err := election.Current(); !os.IsNotExist(err) {
		t.Fatalf("lease after release error = %v", err)
	}
}

func TestCurrentDoesNotWaitForElectionMutationLock(t *testing.T) {
	state := t.TempDir()
	election, err := New(state)
	if err != nil {
		t.Fatal(err)
	}
	token, err := election.NewToken()
	if err != nil {
		t.Fatal(err)
	}
	want, acquired, err := election.TryAcquire("127.0.0.1:10001", token)
	if err != nil || !acquired {
		t.Fatalf("acquire = %#v, %t, %v", want, acquired, err)
	}

	lock, err := os.OpenFile(election.lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := lockFile(lock); err != nil {
		t.Fatal(err)
	}
	locked := true
	defer func() {
		if locked {
			unlockFile(lock)
		}
	}()

	type currentResult struct {
		lease Lease
		err   error
	}
	result := make(chan currentResult, 1)
	go func() {
		lease, readErr := election.Current()
		result <- currentResult{lease: lease, err: readErr}
	}()

	select {
	case got := <-result:
		if got.err != nil {
			t.Fatal(got.err)
		}
		if got.lease.InstanceID != want.InstanceID || got.lease.Token != want.Token {
			t.Fatalf("current lease = %#v, want %#v", got.lease, want)
		}
	case <-time.After(time.Second):
		unlockFile(lock)
		locked = false
		<-result
		t.Fatal("lease discovery waited for the election mutation lock")
	}
}

func TestTargetedHandoffFencesOwnerAndExcludesOtherContenders(t *testing.T) {
	state := t.TempDir()
	now := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	owner, err := New(state)
	if err != nil {
		t.Fatal(err)
	}
	target, err := New(state)
	if err != nil {
		t.Fatal(err)
	}
	other, err := New(state)
	if err != nil {
		t.Fatal(err)
	}
	owner.now = func() time.Time { return now }
	target.now = func() time.Time { return now }
	other.now = func() time.Time { return now }
	ownerToken, _ := owner.NewToken()
	if _, acquired, err := owner.TryAcquire("127.0.0.1:10001", ownerToken); err != nil || !acquired {
		t.Fatalf("owner acquire = %t, %v", acquired, err)
	}
	handoff, handedOff, err := owner.Handoff(ownerToken, target.ID())
	if err != nil || !handedOff || handoff.HandoffTo != target.ID() || handoff.HandoffAt != now || !validEnvironmentID(handoff.EnvironmentID) {
		t.Fatalf("handoff = %#v, %t, %v", handoff, handedOff, err)
	}
	if _, owned, err := owner.Renew(ownerToken); err != nil || owned {
		t.Fatalf("former owner renewed handoff term = %t, %v", owned, err)
	}
	if !target.CanTakeOver(handoff) || other.CanTakeOver(handoff) {
		t.Fatalf("takeover eligibility: target = %t, other = %t", target.CanTakeOver(handoff), other.CanTakeOver(handoff))
	}
	otherToken, _ := other.NewToken()
	if _, acquired, err := other.TryAcquire("127.0.0.1:10003", otherToken); err != nil || acquired {
		t.Fatalf("non-target acquire = %t, %v", acquired, err)
	}
	targetToken, _ := target.NewToken()
	lease, acquired, err := target.TryAcquire("127.0.0.1:10002", targetToken)
	if err != nil || !acquired || lease.InstanceID != target.ID() || lease.HandoffTo != "" {
		t.Fatalf("target acquire = %#v, %t, %v", lease, acquired, err)
	}
}

func TestTargetedHandoffExpiresWhenTargetDisappears(t *testing.T) {
	state := t.TempDir()
	now := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	owner, err := New(state)
	if err != nil {
		t.Fatal(err)
	}
	contender, err := New(state)
	if err != nil {
		t.Fatal(err)
	}
	owner.now = func() time.Time { return now }
	contender.now = func() time.Time { return now }
	ownerToken, _ := owner.NewToken()
	if _, acquired, err := owner.TryAcquire("127.0.0.1:10001", ownerToken); err != nil || !acquired {
		t.Fatalf("owner acquire = %t, %v", acquired, err)
	}
	handoff, handedOff, err := owner.Handoff(ownerToken, "missing-target-instance")
	if err != nil || !handedOff {
		t.Fatalf("handoff = %#v, %t, %v", handoff, handedOff, err)
	}
	contenderToken, _ := contender.NewToken()
	if _, acquired, err := contender.TryAcquire("127.0.0.1:10002", contenderToken); err != nil || acquired {
		t.Fatalf("acquire during reservation = %t, %v", acquired, err)
	}
	now = now.Add(HandoffTimeout)
	if !contender.CanTakeOver(handoff) {
		t.Fatal("expired handoff did not become eligible for fallback takeover")
	}
	if _, acquired, err := contender.TryAcquire("127.0.0.1:10002", contenderToken); err != nil || !acquired {
		t.Fatalf("acquire after reservation expiry = %t, %v", acquired, err)
	}
}

func TestMalformedLeaseCanBeRecovered(t *testing.T) {
	state := t.TempDir()
	election, err := New(state)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(state, "runtime", "primary.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("not json\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	token, _ := election.NewToken()
	if _, acquired, err := election.TryAcquire("127.0.0.1:10001", token); err != nil || !acquired {
		t.Fatalf("recover malformed lease = %t, %v", acquired, err)
	}
}
