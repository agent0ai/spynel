package localapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agent0ai/spynel/internal/app"
	"github.com/agent0ai/spynel/internal/config"
	"github.com/agent0ai/spynel/internal/core"
	"github.com/agent0ai/spynel/internal/history"
	"github.com/agent0ai/spynel/internal/instance"
	"github.com/agent0ai/spynel/internal/shortid"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

func environmentID(character string) string { return strings.Repeat(character, 64) }

func TestForeignLoopbackFailsBeforeDialAndDoesNotPermitTakeover(t *testing.T) {
	state := t.TempDir()
	owner, err := instance.NewWithEnvironmentID(state, environmentID("a"))
	if err != nil {
		t.Fatal(err)
	}
	token, _ := owner.NewToken()
	lease, acquired, err := owner.TryAcquire("127.0.0.1:12345", token)
	if err != nil || !acquired {
		t.Fatalf("owner lease = %#v, %t, %v", lease, acquired, err)
	}
	contender, err := instance.NewWithEnvironmentID(state, environmentID("b"))
	if err != nil {
		t.Fatal(err)
	}
	var dials atomic.Int32
	client := NewClient(contender)
	client.HTTP.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		dials.Add(1)
		return nil, errors.New("must not dial")
	})
	started := time.Now()
	if _, err := client.WaitReady(context.Background()); !errors.Is(err, ErrForeignLoopback) || !strings.Contains(err.Error(), "another host/container environment") {
		t.Fatalf("foreign loopback error = %v", err)
	}
	if dials.Load() != 0 || time.Since(started) > time.Second {
		t.Fatalf("foreign loopback dials = %d, elapsed = %s", dials.Load(), time.Since(started))
	}
	current, err := contender.Current()
	if err != nil || contender.CanTakeOver(current) || current.InstanceID != owner.ID() {
		t.Fatalf("foreign fresh lease was not fenced: %#v, %v", current, err)
	}
}

func TestUnknownEnvironmentReadinessIsBoundedAndRedacted(t *testing.T) {
	state := t.TempDir()
	owner, err := instance.NewWithEnvironmentID(state, environmentID("a"))
	if err != nil {
		t.Fatal(err)
	}
	token, _ := owner.NewToken()
	lease, acquired, err := owner.TryAcquire("127.0.0.1:12345", token)
	if err != nil || !acquired {
		t.Fatalf("owner lease = %#v, %t, %v", lease, acquired, err)
	}
	lease.EnvironmentID = "invalid-old-value"
	data, _ := json.Marshal(lease)
	if err := os.WriteFile(filepath.Join(state, "runtime", "primary.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	contender, err := instance.NewWithEnvironmentID(state, environmentID("b"))
	if err != nil {
		t.Fatal(err)
	}
	var attempts atomic.Int32
	client := NewClient(contender)
	client.ReadyTimeout = 40 * time.Millisecond
	client.HTTP.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		attempts.Add(1)
		return nil, errors.New("secret-token /private/workspace refused")
	})
	_, err = client.WaitReady(context.Background())
	if err == nil || !strings.Contains(err.Error(), "lacks a current environment identifier") || strings.Contains(err.Error(), "secret-token") || strings.Contains(err.Error(), "/private/workspace") {
		t.Fatalf("bounded unknown-owner error = %v", err)
	}
	if attempts.Load() == 0 {
		t.Fatal("unknown environment did not receive its bounded compatibility attempt")
	}
	current, currentErr := contender.Current()
	if currentErr != nil || contender.CanTakeOver(current) {
		t.Fatalf("unknown fresh owner became takeover-eligible: %#v, %v", current, currentErr)
	}
}

type apiHarness struct {
	mu      sync.Mutex
	active  map[string]bool
	threads map[string]string
	keys    []string
}

func newAPIHarness() *apiHarness {
	return &apiHarness{active: map[string]bool{}, threads: map[string]string{}}
}

func (h *apiHarness) Start(context.Context) error { return nil }

func (h *apiHarness) Send(_ context.Context, key, _ string, emit core.Emit) (string, bool, error) {
	h.mu.Lock()
	h.keys = append(h.keys, key)
	thread := h.threads[key]
	if thread == "" {
		thread = "thread-" + key
		h.threads[key] = thread
	}
	h.active[key] = true
	h.mu.Unlock()
	go func() {
		time.Sleep(5 * time.Millisecond)
		h.mu.Lock()
		delete(h.active, key)
		h.mu.Unlock()
		emit(core.Event{Kind: core.EventFinal, Text: "reply for " + key, ThreadID: thread, Done: true})
	}()
	return thread, false, nil
}

func (h *apiHarness) Interrupt(context.Context, string) (bool, error) { return false, nil }
func (h *apiHarness) ResetSession(key string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.threads, key)
	return nil
}
func (h *apiHarness) ThreadID(key string) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.threads[key]
}
func (h *apiHarness) IsActive(key string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.active[key]
}
func (h *apiHarness) Close() error { return nil }

func TestClientStreamsIndependentTUIConversationsThroughOwner(t *testing.T) {
	state := t.TempDir()
	for relative, body := range map[string]string{
		".spynel/tasks/waiting/open.md":  "not valid front matter",
		".spynel/goals/proposed/open.md": "---\nid: goal\nstatus: proposed\n---\n# Goal\n",
	} {
		path := filepath.Join(state, filepath.FromSlash(relative))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	election, server, target, cancel, done := startTestServer(t, state)
	defer func() {
		cancel()
		<-done
		lease, _ := election.Current()
		_ = election.Release(lease.Token)
	}()
	client := NewClient(election)
	if err := client.RegisterLiveTUI(context.Background(), "idle-live"); err != nil {
		t.Fatalf("register live TUI: %v", err)
	}
	if err := client.UnregisterLiveTUI(context.Background()); err != nil {
		t.Fatalf("unregister live TUI: %v", err)
	}

	conversations := []string{"local-primary", "local-secondary"}
	var wait sync.WaitGroup
	for _, conversation := range conversations {
		conversation := conversation
		wait.Add(1)
		go func() {
			defer wait.Done()
			var final core.Event
			err := client.Handle(context.Background(), core.Message{Channel: "tui", Conversation: conversation, Text: "hello"}, func(event core.Event) {
				if event.Kind == core.EventFinal {
					final = event
				}
			})
			if err != nil {
				t.Errorf("handle %s: %v", conversation, err)
				return
			}
			if final.Text != "reply for chat:tui:"+conversation {
				t.Errorf("final for %s = %#v", conversation, final)
			}
		}()
	}
	wait.Wait()
	target.mu.Lock()
	keys := append([]string(nil), target.keys...)
	target.mu.Unlock()
	sort.Strings(keys)
	if len(keys) != 2 || keys[0] != "chat:tui:local-primary" || keys[1] != "chat:tui:local-secondary" {
		t.Fatalf("harness keys = %#v", keys)
	}

	stateSnapshot, err := client.State(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if stateSnapshot.Title != "Spynel" || len(stateSnapshot.Connections) != 2 || stateSnapshot.DurableWork != (core.DurableWorkCounts{Tasks: 1, Goals: 1}) || len(stateSnapshot.WorkDiagnostics) != 1 {
		t.Fatalf("shared state = %#v", stateSnapshot)
	}
	_ = server
}

func TestResumeScreenActionRegistersBranchThroughOwner(t *testing.T) {
	state := t.TempDir()
	election, server, _, cancel, done := startTestServer(t, state)
	defer func() {
		cancel()
		<-done
		lease, _ := election.Current()
		_ = election.Release(lease.Token)
	}()
	client := NewClient(election)
	old := time.Now().UTC().Add(-8 * 24 * time.Hour)
	if _, err := server.Service.History.Append("telegram", "old-source", history.Entry{At: old, Role: "assistant", Content: "old answer"}); err != nil {
		t.Fatal(err)
	}

	var picker core.Event
	if err := client.Handle(context.Background(), core.Message{Channel: "tui", Conversation: "picker", Text: "/resume"}, func(event core.Event) {
		picker = event
	}); err != nil {
		t.Fatal(err)
	}
	if picker.Screen == nil {
		t.Fatalf("resume picker = %#v", picker)
	}
	var action string
	for _, control := range picker.Screen.Controls {
		if strings.Contains(control.Value, "old-source") {
			action = control.Key
			break
		}
	}
	if action == "" {
		t.Fatalf("old source missing from picker: %#v", picker.Screen.Controls)
	}
	chat, err := client.ScreenAction(context.Background(), "resume", action, nil)
	if err != nil || chat == nil || chat.Conversation == "" {
		t.Fatalf("resume action = %#v, %v", chat, err)
	}

	if err := client.Handle(context.Background(), core.Message{Channel: "tui", Conversation: "cleanup-invoker", Text: "/cleanup 7"}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(server.Service.History.Path("tui", chat.Conversation)); err != nil {
		t.Fatalf("owner-admitted resume branch was removed: %v", err)
	}
}

func TestServerRejectsMissingToken(t *testing.T) {
	state := t.TempDir()
	election, _, _, cancel, done := startTestServer(t, state)
	defer func() { cancel(); <-done }()
	lease, err := election.Current()
	if err != nil {
		t.Fatal(err)
	}
	response, err := http.Get("http://" + lease.Endpoint + "/v1/state") //nolint:gosec
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, response.Body)
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusUnauthorized)
	}
}

func TestDiagnosticUsesOwnerRuntimeLogger(t *testing.T) {
	state := t.TempDir()
	election, server, _, cancel, done := startTestServer(t, state)
	defer func() {
		cancel()
		<-done
	}()
	client := NewClient(election)
	if err := client.Diagnostic(context.Background(), "slow_markdown", "stream Markdown render took 125ms"); err != nil {
		t.Fatal(err)
	}
	logs := server.Service.Runtime.Logs()
	entry := logs[len(logs)-1]
	if entry.Component != "tui" || entry.Event != "slow_markdown" || !strings.Contains(entry.Text, "125ms") {
		t.Fatalf("diagnostic log entry = %#v", entry)
	}
}

func TestDecodeJSONRejectsOversizedAndTrailingRequests(t *testing.T) {
	var request notifyRequest
	oversized := `{"origin":"tui/local","message":"` + strings.Repeat("x", maxRequestBytes) + `"}`
	if err := decodeJSON(strings.NewReader(oversized), &request); err == nil || !strings.Contains(err.Error(), "byte limit") {
		t.Fatalf("oversized request error = %v", err)
	}
	if err := decodeJSON(strings.NewReader(`{"origin":"tui/local","message":"first"} {"origin":"tui/local","message":"second"}`), &request); err == nil || !strings.Contains(err.Error(), "multiple JSON values") {
		t.Fatalf("trailing request error = %v", err)
	}
}

func TestInitialScreenCanForceWelcomeForANewSecondary(t *testing.T) {
	state := t.TempDir()
	election, _, _, cancel, done := startTestServer(t, state)
	defer func() {
		cancel()
		<-done
		lease, _ := election.Current()
		_ = election.Release(lease.Token)
	}()
	client := NewClient(election)
	first, err := client.InitialScreen(context.Background(), false, false)
	if err != nil || first == nil || first.ID != "welcome" {
		t.Fatalf("first welcome = %#v, %v", first, err)
	}
	second, err := client.InitialScreen(context.Background(), false, false)
	if err != nil || second != nil {
		t.Fatalf("repeated automatic welcome = %#v, %v", second, err)
	}
	forced, err := client.InitialScreen(context.Background(), false, true)
	if err != nil || forced == nil || forced.ID != "welcome" {
		t.Fatalf("forced secondary welcome = %#v, %v", forced, err)
	}
}

func TestClientLeavesLongRunningResponseHeadersToCallerContext(t *testing.T) {
	client := NewClient(nil)
	transport, ok := client.HTTP.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("client transport = %T", client.HTTP.Transport)
	}
	if transport.ResponseHeaderTimeout != 0 {
		t.Fatalf("response header timeout = %s; long message and run-once requests must use their caller context", transport.ResponseHeaderTimeout)
	}
}

func TestClientStatusIdentifiesCallerAndPrimaryInstances(t *testing.T) {
	state := t.TempDir()
	primary, _, _, cancel, done := startTestServer(t, state)
	defer func() {
		cancel()
		<-done
		lease, _ := primary.Current()
		_ = primary.Release(lease.Token)
	}()
	secondary, err := instance.New(filepath.Join(state, config.StateDirectoryName))
	if err != nil {
		t.Fatal(err)
	}
	client := NewClient(secondary)
	snapshot, err := client.Status(context.Background(), "local-secondary")
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Instance != shortid.Display(secondary.ID()) || snapshot.PrimaryInstance != shortid.Display(primary.ID()) || len(snapshot.Connections) != 2 {
		t.Fatalf("structured status = %#v", snapshot)
	}
	var final core.Event
	if err := client.Handle(context.Background(), core.Message{Channel: "tui", Conversation: "local-secondary", Text: "/status"}, func(event core.Event) {
		if event.Kind == core.EventFinal {
			final = event
		}
	}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Instance ID: `" + shortid.Display(secondary.ID()) + "`",
		"Primary instance ID: `" + shortid.Display(primary.ID()) + "`",
	} {
		if !strings.Contains(final.Text, want) {
			t.Fatalf("status does not contain %q:\n%s", want, final.Text)
		}
	}
}

func startTestServer(t *testing.T, state string) (*instance.Election, *Server, *apiHarness, context.CancelFunc, <-chan error) {
	t.Helper()
	cfg := config.Default()
	cfg.Root = state
	cfg.Path = config.PathForRoot(state)
	cfg.Extensions.Enabled = false
	cfg.Orchestrator.Enabled = false
	if err := os.MkdirAll(cfg.StatePath("prompts"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg.StatePath("prompts", "chat.md"), []byte("{{RECENT_HISTORY}}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	target := newAPIHarness()
	service := app.New(cfg, target)
	election, err := instance.New(cfg.StatePath())
	if err != nil {
		t.Fatal(err)
	}
	service.SetPrimaryInstanceID(election.ID())
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	token, err := election.NewToken()
	if err != nil {
		t.Fatal(err)
	}
	if _, acquired, err := election.TryAcquire(listener.Addr().String(), token); err != nil || !acquired {
		t.Fatalf("acquire = %t, %v", acquired, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	server := &Server{Service: service, Token: token}
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx, listener) }()
	client := NewClient(election)
	readyCtx, readyCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer readyCancel()
	if _, err := client.WaitReady(readyCtx); err != nil {
		cancel()
		t.Fatal(err)
	}
	return election, server, target, cancel, done
}
