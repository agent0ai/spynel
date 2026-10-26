package harness

import (
	"context"
	"errors"
	"testing"

	"github.com/agent0ai/spynel/internal/core"
)

func TestNativeConversationSteeringCompletionFence(t *testing.T) {
	for _, name := range []string{"codex", "claude-code", "pi"} {
		t.Run(name, func(t *testing.T) {
			original, replacement := 0, 0
			emit := func(core.Event) { original++ }
			var target ConversationSteerer
			var publish func(core.Event)
			switch name {
			case "codex":
				c, err := NewCodex(CodexConfig{})
				if err != nil {
					t.Fatal(err)
				}
				turn := &turnState{threadID: "thread", turnID: "turn", completed: true, emit: emit}
				c.session["chat"], c.active["thread"] = "thread", turn
				target, publish = c, turn.emitEvent
			case "claude-code":
				c, err := NewClaude(HarnessConfig{})
				if err != nil {
					t.Fatal(err)
				}
				turn := &claudeTurn{emit: emit, accepting: false}
				c.active["chat"] = turn
				target, publish = c, turn.emitEvent
			case "pi":
				p, err := NewPi(HarnessConfig{})
				if err != nil {
					t.Fatal(err)
				}
				turn := &piTurn{emit: emit, completed: true}
				p.processes["chat"] = &piProcess{active: turn}
				target, publish = p, turn.emitEvent
			}
			// The turn is still indexed, but completion has won its delivery fence.
			_, err := target.SteerConversation(context.Background(), "chat", "too late", func(core.Event) { replacement++ })
			if !errors.Is(err, errNativeTurnInactive) || original != 0 || replacement != 0 {
				t.Fatalf("completed native turn accepted delivery or handed off its emitter: %v", err)
			}
			publish(core.Event{Kind: core.EventFinal, Done: true})
			if original != 1 || replacement != 0 {
				t.Fatal("refused follow-up stole the original terminal")
			}
		})
	}
}

func TestSupervisorStrictFollowupRequiresNativeOperation(t *testing.T) {
	target := &supervisorHarness{name: "synthetic", followUp: FollowUpSteer, active: map[string]bool{}, emits: map[string]core.Emit{}}
	s := NewSupervisor(NewRegistry(), HarnessConfig{})
	s.current = target
	original, replacement := 0, 0
	if _, _, err := s.Send(context.Background(), "chat", "first", func(event core.Event) {
		if event.Done {
			original++
		}
	}); err != nil {
		t.Fatal(err)
	}
	_, _, err := s.send(context.Background(), "chat", "second", "second", func(core.Event) { replacement++ }, true)
	var rejected *SendNotAdmittedError
	if !errors.As(err, &rejected) || len(target.prompts["chat"]) != 1 || !target.IsActive("chat") {
		t.Fatal("missing strict operation fell back to ordinary Send", err)
	}
	target.finish("chat")
	if original != 1 || replacement != 0 {
		t.Fatal("refused follow-up changed response ownership")
	}
}

func TestSupervisorStrictFollowupDoesNotQueueAfterNativeCompletion(t *testing.T) {
	target := &supervisorHarness{name: "synthetic", followUp: FollowUpSteer, active: map[string]bool{}, emits: map[string]core.Emit{}}
	s := NewSupervisor(NewRegistry(), HarnessConfig{})
	s.current = target
	if _, _, err := s.Send(context.Background(), "chat", "first", nil); err != nil {
		t.Fatal(err)
	}
	// Adapter completion can precede publication through the Supervisor wrapper.
	delete(target.active, "chat")
	_, _, err := s.send(context.Background(), "chat", "second", "second", nil, true)
	var rejected *SendNotAdmittedError
	if !errors.As(err, &rejected) || len(s.pending["chat"]) != 0 || len(target.prompts["chat"]) != 1 {
		t.Fatal("strict native follow-up entered a successor queue after completion", err)
	}
}

func TestPiConversationSteeringClosedInputKeepsEmitter(t *testing.T) {
	p, err := NewPi(HarnessConfig{})
	if err != nil {
		t.Fatal(err)
	}
	original, replacement := 0, 0
	turn := &piTurn{emit: func(core.Event) { original++ }}
	p.processes["chat"] = &piProcess{owner: p, active: turn, closed: true}
	_, err = p.SteerConversation(context.Background(), "chat", "follow-up", func(core.Event) { replacement++ })
	if err == nil {
		t.Fatal("closed input accepted steering")
	}
	turn.emitEvent(core.Event{Kind: core.EventError, Done: true})
	if original != 1 || replacement != 0 {
		t.Fatal("failed delivery cleared an emitter it never replaced")
	}
}
