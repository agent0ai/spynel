package instance

import (
	"strings"
	"testing"
)

func TestWorkerFenceAuthenticatesControllerAndEnvironment(t *testing.T) {
	state := t.TempDir()
	owner, err := New(state)
	if err != nil {
		t.Fatal(err)
	}
	worker, err := New(state)
	if err != nil {
		t.Fatal(err)
	}
	token, _ := owner.NewToken()
	if _, acquired, err := owner.TryAcquire("127.0.0.1:10001", token); err != nil || !acquired {
		t.Fatalf("acquire = %t, %v", acquired, err)
	}
	for _, identity := range []struct{ id, token string }{{owner.ID(), "wrong"}, {"wrong", token}, {"", ""}} {
		if ran, err := worker.RunForOwner(identity.id, identity.token, func() error { t.Error("unauthorized controller entered worker"); return nil }); err != nil || ran {
			t.Fatalf("invalid controller admitted: %t, %v", ran, err)
		}
	}
	if ran, err := worker.RunForOwner(owner.ID(), token, func() error { return nil }); err != nil || !ran {
		t.Fatalf("separate worker could not fence elected controller: %t, %v", ran, err)
	}
	foreign, err := NewWithEnvironmentID(state, strings.Repeat("f", 64))
	if err != nil {
		t.Fatal(err)
	}
	if ran, err := foreign.RunForOwner(owner.ID(), token, func() error { t.Error("foreign environment entered worker"); return nil }); err != nil || ran {
		t.Fatalf("foreign environment admitted: %t, %v", ran, err)
	}
}
