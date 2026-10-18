package harness

import (
	"errors"
	"testing"
)

func TestWorkerInspectionReportsOwningAvailabilityAndRetainedSession(t *testing.T) {
	host, _ := newWorkerConfigurationHost(t)
	reply := host.apply(WorkerRequest{Action: "inspect"})
	if reply.Availability == nil || !reply.Availability.Available || reply.Availability.Detail != "" {
		t.Fatal("inspection omitted the owning adapter availability")
	}
	host.supervisor = NewSupervisor(NewRegistry(), host.supervisor.HarnessConfig())
	if err := host.supervisor.ConfigureUnavailable(host.supervisor.HarnessConfig(), errors.New("synthetic offline adapter")); err != nil {
		t.Fatal(err)
	}
	reply = host.apply(WorkerRequest{Action: "inspect"})
	if reply.Availability == nil || reply.Availability.Available || reply.Availability.Detail != "synthetic offline adapter" {
		t.Fatal("inspection invented adapter readiness")
	}
	host.executions[1] = &workerExecution{WorkerExecution: WorkerExecution{Key: "synthetic-session", Done: true}}
	reply = host.apply(WorkerRequest{Action: "session", Key: "synthetic-session"})
	if reply.Session == nil || reply.Session.Active || !reply.Session.Retained {
		t.Fatal("inactive provider hid its retained terminal delivery")
	}
	if reply := host.apply(WorkerRequest{Action: "reset", Key: "synthetic-session"}); reply.Error == "" {
		t.Fatal("retained terminal delivery permitted a reset")
	}
}
