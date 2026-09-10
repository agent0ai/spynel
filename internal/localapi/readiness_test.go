package localapi

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/agent0ai/spynel/internal/instance"
)

func TestReadinessTimeoutOffersConditionalStoppedJobRecovery(t *testing.T) {
	election, err := instance.NewWithEnvironmentID(t.TempDir(), environmentID("a"))
	if err != nil {
		t.Fatal(err)
	}
	token, _ := election.NewToken()
	if _, ok, err := election.TryAcquire("127.0.0.1:12345", token); err != nil || !ok {
		t.Fatalf("claim: %t %v", ok, err)
	}
	client := NewClient(election)
	client.ReadyTimeout = 20 * time.Millisecond
	client.HTTP.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		<-r.Context().Done()
		return nil, r.Context().Err()
	})
	_, err = client.WaitReady(context.Background())
	if !errors.Is(err, ErrReadinessTimeout) || !strings.Contains(err.Error(), "If its shell reports Stopped") || !strings.Contains(err.Error(), "fg in that shell") || strings.Contains(err.Error(), token) {
		t.Fatalf("readiness error: %v", err)
	}
	lease, err := election.Current()
	if err != nil || lease.InstanceID != election.ID() || election.IsStale(lease) {
		t.Fatal("timeout replaced or invalidated a fresh owner")
	}
}
