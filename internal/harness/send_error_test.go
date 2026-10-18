package harness

import (
	"context"
	"errors"
	"testing"
)

func TestSendPreflightProvesNoMessageAdmission(t *testing.T) {
	for _, name := range []string{"codex", "claude-code", "pi", "acp"} {
		t.Run(name, func(t *testing.T) {
			target, err := NewBuiltinRegistry().Create(HarnessConfig{Name: name, Command: "unused"})
			if err != nil {
				t.Fatal(err)
			}
			_, _, err = target.Send(t.Context(), "job", "", nil)
			var rejected *SendNotAdmittedError
			if !errors.As(err, &rejected) || err.Error() != "harness prompt is empty" {
				t.Fatal("preflight lost its no-admission proof or original error", err)
			}
		})
	}
	supervisor := NewSupervisor(NewRegistry(), HarnessConfig{})
	for _, cancelled := range []bool{false, true} {
		ctx, cancel := context.WithCancel(t.Context())
		if cancelled {
			cancel()
		}
		_, _, err := supervisor.Send(ctx, "job", "synthetic", nil)
		cancel()
		var rejected *SendNotAdmittedError
		if !errors.As(err, &rejected) || cancelled && !errors.Is(err, context.Canceled) {
			t.Fatal("unadmitted Supervisor request lost its proof/cause", err)
		}
	}
}
