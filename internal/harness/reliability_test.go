package harness

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent0ai/spynel/internal/core"
)

func TestClaudePreservesPreInitFailure(t *testing.T) {
	for _, mode := range []string{"claude-pre-init-error", "claude-stderr-error", "claude-pre-init-hang"} {
		t.Run(mode, func(t *testing.T) {
			command, root, _ := portableHarnessFixture(t, mode)
			sessions := filepath.Join(root, "sessions.json")
			c, err := NewClaude(HarnessConfig{Command: command, Cwd: root, ApprovalPolicy: "plan", SessionsFile: sessions})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			if err := c.Start(ctx); err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			terminal := make(chan core.Event, 1)
			_, _, err = c.Send(ctx, "chat", "synthetic", func(e core.Event) {
				if e.Done {
					terminal <- e
				}
			})
			if err == nil || !strings.Contains(err.Error(), "synthetic stale session") || !strings.Contains(err.Error(), "No conversation found") {
				t.Fatalf("real startup failure was masked: %v", err)
			}
			if strings.Contains(err.Error(), "synthetic-secret") || strings.Contains(err.Error(), "synthetic-bearer") || strings.Contains(err.Error(), "update Claude") {
				t.Fatalf("unsafe or misleading startup error: %v", err)
			}
			select {
			case e := <-terminal:
				if e.Kind != core.EventError || e.ThreadID != "" || e.Text != err.Error() {
					t.Fatalf("terminal: %+v", e)
				}
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			if c.IsActive("chat") || c.ThreadID("chat") != "" {
				t.Fatal("failed startup left active or persisted session")
			}
			if _, err := os.Stat(sessions); !os.IsNotExist(err) {
				t.Fatal("pre-init session was persisted", err)
			}
			if err := c.Close(); err != nil {
				t.Fatal("provider teardown", err)
			}
		})
	}
}

func TestCodexReleaseDoesNotBlockReaderOrRaceFollowUp(t *testing.T) {
	command, root, logPath := portableHarnessFixture(t, "codex-lifecycle")
	release := filepath.Join(root, "release")
	t.Setenv("SPYNEL_TEST_CODEX_DELAY_RELEASE", release)
	c, err := NewCodex(CodexConfig{Command: command, Cwd: root})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := c.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	done := make(chan core.Event, 2)
	emit := func(e core.Event) {
		if e.Done {
			done <- e
		}
	}
	if _, _, err := c.Send(ctx, "chat", "first", emit); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	deadline := time.Now().Add(time.Second)
	for {
		found := false
		for _, r := range readFixtureRecords(t, logPath) {
			found = found || r.Method == "thread/unsubscribe"
		}
		if found {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no idle release")
		}
		time.Sleep(time.Millisecond)
	}
	// This reply must pass the reader while the unsubscribe reply is withheld.
	if _, err := c.Models(ctx); err != nil {
		t.Fatal("cleanup blocked protocol reader", err)
	}
	wait, cancelWait := context.WithCancel(ctx)
	sent := make(chan error, 1)
	go func() { _, _, err := c.Send(wait, "chat", "cancelled follow-up", emit); sent <- err }()
	cancelWait()
	if err := <-sent; !errors.Is(err, context.Canceled) {
		t.Fatal("cancelled follow-up was delivered", err)
	}
	go func() { _, _, err := c.Send(ctx, "chat", "next", emit); sent <- err }()
	select {
	case err := <-sent:
		t.Fatal("new turn crossed unfinished release", err)
	case <-time.After(20 * time.Millisecond):
	}
	if err := os.WriteFile(release, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := <-sent; err != nil {
		t.Fatal(err)
	}
	select {
	case e := <-done:
		if e.Kind != core.EventFinal {
			t.Fatal(e)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestCodexQueuedReleasePreservesNewActiveTurn(t *testing.T) {
	c, err := NewCodex(CodexConfig{})
	if err != nil {
		t.Fatal(err)
	}
	c.ctx, c.cancel = context.WithCancel(t.Context())
	defer c.cancel()
	c.session["chat"] = "thread"
	c.loaded["thread"] = true
	c.active["thread"] = &turnState{key: "chat", threadID: "thread", turnID: "old"}
	gate := c.lockForKey("chat")
	gate.Lock()
	params, _ := json.Marshal(map[string]any{"threadId": "thread", "turn": map[string]any{"id": "old", "status": "completed"}})
	c.handleNotification(wireMessage{Method: "turn/completed", Params: params})
	c.mu.Lock()
	c.active["thread"] = &turnState{key: "chat", threadID: "thread", turnID: "new"}
	c.mu.Unlock()
	gate.Unlock()
	c.idleReleases.Wait()
	if !c.loaded["thread"] || !c.IsActive("chat") {
		t.Fatal("old cleanup released the new turn")
	}
}

func TestCodexReleaseFailureRemainsResumableAndObservable(t *testing.T) {
	for _, status := range []string{"failed", "interrupted"} {
		t.Run(status, func(t *testing.T) {
			command, root, logPath := portableHarnessFixture(t, "codex-lifecycle")
			t.Setenv("SPYNEL_TEST_CODEX_TERMINAL_STATUS", status)
			t.Setenv("SPYNEL_TEST_CODEX_RELEASE_ERROR", "1")
			diagnostic := newTailBuffer(4096)
			c, _ := NewCodex(CodexConfig{Command: command, Cwd: root, Stderr: diagnostic})
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			if err := c.Start(ctx); err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			for turn := 0; turn < 2; turn++ {
				done := make(chan struct{})
				if _, _, err := c.Send(ctx, "chat", "test", func(e core.Event) {
					if e.Done {
						close(done)
					}
				}); err != nil {
					t.Fatal(err)
				}
				select {
				case <-done:
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
				gate := c.lockForKey("chat")
				deadline := time.Now().Add(time.Second)
				for {
					c.mu.Lock()
					loaded := c.loaded["thr_test"]
					c.mu.Unlock()
					if !loaded {
						break
					}
					if time.Now().After(deadline) {
						t.Fatal("release not scheduled")
					}
					time.Sleep(time.Millisecond)
				}
				gate.Lock()
				gate.Unlock()
				c.idleReleases.Wait()
			}
			if !strings.Contains(diagnostic.String(), "idle thread release failed") {
				t.Fatal("cleanup failure was hidden")
			}
			resumed := false
			for _, r := range readFixtureRecords(t, logPath) {
				resumed = resumed || r.Method == "thread/resume"
			}
			if !resumed || c.ThreadID("chat") != "thr_test" {
				t.Fatal("uncertain cleanup lost resumability")
			}
		})
	}
}

func TestCodexResetReleasesIdleThreadAndCloseCancelsRelease(t *testing.T) {
	command, root, logPath := portableHarnessFixture(t, "codex-lifecycle")
	t.Setenv("SPYNEL_TEST_CODEX_DELAY_RELEASE", filepath.Join(root, "never-released"))
	c, _ := NewCodex(CodexConfig{Command: command, Cwd: root, SessionsFile: filepath.Join(root, "sessions.json")})
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := c.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.ensureThread(ctx, "chat", "", nil); err != nil {
		t.Fatal(err)
	}
	if err := c.ResetSession("chat"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for {
		found := false
		for _, r := range readFixtureRecords(t, logPath) {
			found = found || r.Method == "thread/unsubscribe"
		}
		if found {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("reset retained loaded thread")
		}
		time.Sleep(time.Millisecond)
	}
	if c.ThreadID("chat") != "" {
		t.Fatal("reset retained mapping")
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	pending := len(c.pending)
	c.mu.Unlock()
	if pending != 0 {
		t.Fatal("cleanup left pending RPC", pending)
	}
}

func TestClaudeHookEventsStillRequireInit(t *testing.T) {
	command, root, _ := portableHarnessFixture(t, "claude-hook-init")
	c, err := NewClaude(HarnessConfig{Command: command, Cwd: root, ApprovalPolicy: "plan"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := c.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	done := make(chan core.Event, 1)
	id, _, err := c.Send(ctx, "chat", "synthetic", func(e core.Event) {
		if e.Done {
			done <- e
		}
	})
	if err != nil || id != "claude-session" {
		t.Fatal("hook/init admission", id, err)
	}
	select {
	case e := <-done:
		if e.Kind != core.EventFinal {
			t.Fatal(e)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}

func TestCodexReleasesIdleThreadAndResumesSameSession(t *testing.T) {
	command, root, logPath := portableHarnessFixture(t, "codex-lifecycle")
	c, err := NewCodex(CodexConfig{Command: command, Cwd: root, SessionsFile: filepath.Join(root, "sessions.json")})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := c.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	for turn := 0; turn < 2; turn++ {
		done := make(chan core.Event, 1)
		id, _, err := c.Send(ctx, "chat", "synthetic", func(e core.Event) {
			if e.Done {
				done <- e
			}
		})
		if err != nil || id != "thr_test" {
			t.Fatal("send", id, err)
		}
		select {
		case e := <-done:
			if e.Text != "hello world" {
				t.Fatal(e)
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		deadline := time.Now().Add(time.Second)
		for {
			c.mu.Lock()
			loaded := c.loaded[id]
			c.mu.Unlock()
			if !loaded {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("completed thread remains loaded")
			}
			time.Sleep(time.Millisecond)
		}
		if c.ThreadID("chat") != id {
			t.Fatal("release discarded resumable identity")
		}
		c.idleReleases.Wait()
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	methods := map[string]int{}
	for _, r := range readFixtureRecords(t, logPath) {
		methods[r.Method]++
	}
	if methods["thread/start"] != 1 || methods["thread/resume"] != 1 || methods["thread/unsubscribe"] != 2 {
		t.Fatal("lifecycle calls", methods)
	}
}

func TestClaudeStartupDiagnosticIsBoundedAndRedacted(t *testing.T) {
	message := "\x1b[31mstartup\x1b[0m\r\x00\nAuthorization: Bearer synthetic-bearer\napi_key=synthetic-key; session ID: private-session\n" + strings.Repeat("x", 6000) + "\nlast cause: sk-private"
	got := claudeStartupDiagnostic(message)
	if len([]rune(got)) > 4096 || !strings.HasPrefix(got, "startup\n") || !strings.Contains(got, "diagnostic middle omitted") || !strings.Contains(got, "last cause:") {
		t.Fatal("diagnostic bounds or useful edges lost")
	}
	for _, secret := range []string{"synthetic-bearer", "synthetic-key", "private-session", "sk-private", "\x1b", "\r", "\x00"} {
		if strings.Contains(got, secret) {
			t.Fatalf("unsafe diagnostic contains %q", secret)
		}
	}
	for _, input := range []string{
		`{"authorization":"Bearer private-value"}`,
		`AWS_SECRET_ACCESS_KEY=private-value`,
		`password=private-value second-word`,
		`{"token":"private-value\"second-word"}`,
		`request: Bearer private-value`,
		`https://private-value:second-word@example.com/`,
	} {
		got := claudeStartupDiagnostic(input)
		if strings.Contains(got, "private-value") || strings.Contains(got, "second-word") {
			t.Fatal("credential diagnostic was not redacted")
		}
	}
}
