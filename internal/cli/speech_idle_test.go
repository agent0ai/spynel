package cli

import (
	"testing"

	"github.com/agent0ai/spynel/internal/config"
)

func TestSpeechIdleTimeoutPreservesTransportFingerprint(t *testing.T) {
	cfg := config.Default()
	for _, managed := range managedChannels(nil, nil) {
		before := managed.Fingerprint(cfg)
		next := cfg
		next.Speech.IdleTimeoutSec = 300
		if managed.Fingerprint(next) != before {
			t.Fatalf("%s would restart on an idle-timeout change", managed.Name)
		}
		next.Speech.Enabled = !cfg.Speech.Enabled
		if managed.Fingerprint(next) == before {
			t.Fatalf("%s lost its speech enablement fingerprint", managed.Name)
		}
	}
}
