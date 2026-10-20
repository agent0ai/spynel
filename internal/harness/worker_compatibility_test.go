package harness

import "testing"

func TestWorkerCompatibilityKeepsRuntimeSettingsExact(t *testing.T) {
	initial := HarnessConfig{Name: "codex", Cwd: "/synthetic", Version: "0.14.0"}
	successor := initial
	successor.Version = "0.14.1"
	if ConfigDigest(initial) != ConfigDigest(successor) {
		t.Fatal("compatible controller versions cannot adopt the same runtime")
	}
	for _, change := range []struct {
		name string
		set  func(*HarnessConfig)
	}{
		{"adapter", func(c *HarnessConfig) { c.Name = "pi" }},
		{"command", func(c *HarnessConfig) { c.Command = "/other-provider" }},
		{"arguments", func(c *HarnessConfig) { c.Args = []string{"other"} }},
		{"environment", func(c *HarnessConfig) { c.Env = []string{"SYNTHETIC=other"} }},
		{"workspace", func(c *HarnessConfig) { c.Cwd = "/other" }},
		{"model", func(c *HarnessConfig) { c.Model = "other" }},
		{"effort", func(c *HarnessConfig) { c.Effort = "high" }},
		{"legacy effort", func(c *HarnessConfig) { c.LegacyEffort = true }},
		{"service mode", func(c *HarnessConfig) { c.ServiceMode = "priority" }},
		{"approval policy", func(c *HarnessConfig) { c.ApprovalPolicy = "never" }},
		{"sandbox", func(c *HarnessConfig) { c.Sandbox = "read-only" }},
		{"network", func(c *HarnessConfig) { c.Network = true }},
		{"sessions", func(c *HarnessConfig) { c.SessionsFile = "/other/sessions" }},
	} {
		t.Run(change.name, func(t *testing.T) {
			changed := successor
			change.set(&changed)
			if ConfigDigest(initial) == ConfigDigest(changed) {
				t.Fatal("changed runtime settings accepted as compatible")
			}
		})
	}
}
