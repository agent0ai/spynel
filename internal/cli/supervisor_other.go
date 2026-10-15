//go:build !linux && !darwin

package cli

import (
	"context"
	"errors"
)

type executorControl struct {
	parent     int
	generation string
	restart    chan struct{}
	unregister func()
}

var executor *executorControl

func supervisorEligible([]string) bool     { return false }
func runSupervisor([]string, string) error { return errors.New("unsupported supervision platform") }
func enterExecutor([]string, string) ([]string, error) {
	return nil, errors.New("unsupported executor platform")
}
func executorContext() context.Context             { return context.Background() }
func executorIntent(string) error                  { return nil }
func completeExecutorRun(err error) error          { return err }
func admitExecutor(string, string, []string) error { return nil }

func executorDiagnostic(processDiagnostic) error { return nil }
