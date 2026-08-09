package harness

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"
)

const (
	fixtureModeEnv = "SPYNEL_HARNESS_FIXTURE_MODE"
	fixtureLogEnv  = "SPYNEL_HARNESS_FIXTURE_LOG"
)

type fixtureRecord struct {
	Kind       string          `json:"kind"`
	Args       []string        `json:"args"`
	Cwd        string          `json:"cwd"`
	Executable string          `json:"executable"`
	Text       string          `json:"text"`
	Method     string          `json:"method"`
	Params     json.RawMessage `json:"params"`
}

func TestMain(m *testing.M) {
	if mode := os.Getenv(fixtureModeEnv); mode != "" {
		os.Exit(runHarnessFixture(mode))
	}
	os.Exit(m.Run())
}

// portableHarnessFixture copies the current Go test executable to a path that
// contains spaces and Unicode. The adapter then launches that exact path from
// an equally awkward working directory, exercising exec.Command directly on
// every supported host without a shell or quoting layer.
func portableHarnessFixture(t *testing.T, mode string) (command, cwd, logPath string) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "portable fixture 世界")
	toolsDir := filepath.Join(root, "tool bin café")
	cwd = filepath.Join(root, "work tree λ")
	if err := os.MkdirAll(toolsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	source, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	name := "provider fixture"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	command = filepath.Join(toolsDir, name)
	input, err := os.Open(source)
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	output, err := os.OpenFile(command, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o700)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(output, input); err != nil {
		_ = output.Close()
		t.Fatal(err)
	}
	if err := output.Close(); err != nil {
		t.Fatal(err)
	}
	logPath = filepath.Join(root, "fixture evidence 日志.jsonl")
	t.Setenv(fixtureModeEnv, mode)
	t.Setenv(fixtureLogEnv, logPath)
	return command, cwd, logPath
}

func readFixtureRecords(t *testing.T, path string) []fixtureRecord {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	var records []fixtureRecord
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		var record fixtureRecord
		if err := json.Unmarshal(scanner.Bytes(), &record); err != nil {
			t.Fatalf("decode fixture record %q: %v", scanner.Text(), err)
		}
		records = append(records, record)
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	return records
}

func runHarnessFixture(mode string) int {
	executable, _ := os.Executable()
	appendFixtureLog(map[string]any{"kind": "invocation", "args": os.Args[1:], "cwd": mustGetwd(), "executable": executable})
	switch mode {
	case "codex-lifecycle", "codex-interrupt", "codex-models", "codex-init-missing-method", "codex-resume-missing-method", "codex-thread-changed-field", "codex-terminal-changed-status":
		return runCodexFixture(mode)
	case "claude-stream", "claude-steer", "claude-text", "claude-interrupt", "claude-help-missing-flag", "claude-init-changed-event", "claude-terminal-error", "claude-result-nonzero":
		return runClaudeFixture(mode)
	case "pi-lifecycle", "pi-steer", "pi-interrupt", "pi-state-missing-session":
		return runPiFixture(mode)
	case "acp-lifecycle", "acp-interrupt", "acp-version-mismatch":
		return runACPFixture(mode)
	default:
		_, _ = fmt.Fprintf(os.Stderr, "unknown harness fixture mode %q\n", mode)
		return 2
	}
}

func runPiFixture(mode string) int {
	if len(os.Args) > 1 && os.Args[1] == "--version" {
		_, _ = fmt.Fprintln(os.Stdout, "pi 0.fixture")
		return 0
	}
	type request struct {
		ID      string          `json:"id"`
		Type    string          `json:"type"`
		Message json.RawMessage `json:"message"`
	}
	var outputMu sync.Mutex
	write := func(value any) {
		outputMu.Lock()
		defer outputMu.Unlock()
		_ = json.NewEncoder(os.Stdout).Encode(value)
	}
	respond := func(message request, data any) {
		write(map[string]any{"id": message.ID, "type": "response", "command": message.Type, "success": true, "data": data})
	}
	messageStart := func() {
		write(map[string]any{"type": "message_start", "message": map[string]any{"role": "assistant", "content": []any{}}})
	}
	delta := func(text string) {
		write(map[string]any{"type": "message_update", "assistantMessageEvent": map[string]any{"type": "text_delta", "delta": text}})
	}
	messageEnd := func(text, reason string) {
		write(map[string]any{"type": "message_end", "message": map[string]any{"role": "assistant", "stopReason": reason, "content": []any{map[string]any{"type": "text", "text": text}}}})
	}
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		var message request
		if json.Unmarshal(scanner.Bytes(), &message) != nil {
			continue
		}
		appendFixtureLog(map[string]any{"kind": "request", "method": message.Type, "params": json.RawMessage(scanner.Bytes())})
		switch message.Type {
		case "get_state":
			sessionFile := filepath.Join(mustGetwd(), "pi-fixture-session.jsonl")
			_ = os.WriteFile(sessionFile, []byte("{}\n"), 0o600)
			if mode == "pi-state-missing-session" {
				respond(message, map[string]any{"isStreaming": false})
			} else {
				respond(message, map[string]any{"sessionId": "pi-session", "sessionFile": sessionFile, "isStreaming": false})
			}
		case "set_steering_mode", "set_follow_up_mode":
			respond(message, map[string]any{})
		case "get_available_models":
			respond(message, map[string]any{"models": []any{map[string]any{"id": "model-a", "name": "Model A", "provider": "fixture", "reasoning": true}}})
		case "prompt":
			respond(message, map[string]any{})
			messageStart()
			if mode == "pi-steer" {
				delta("first")
			} else if mode == "pi-interrupt" {
				delta("working")
			} else {
				delta("hello ")
				delta("world")
				messageEnd("hello world", "stop")
				write(map[string]any{"type": "agent_end"})
				go func() {
					time.Sleep(80 * time.Millisecond)
					write(map[string]any{"type": "agent_settled"})
				}()
			}
		case "steer":
			respond(message, map[string]any{})
			delta(" second")
			messageEnd("first second", "stop")
			write(map[string]any{"type": "agent_settled"})
		case "abort":
			respond(message, map[string]any{})
			messageEnd("working", "aborted")
			write(map[string]any{"type": "agent_settled"})
		}
	}
	return 0
}

func runACPFixture(mode string) int {
	type message struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Method  string          `json:"method"`
		Params  json.RawMessage `json:"params"`
		Result  json.RawMessage `json:"result"`
	}
	var outputMu sync.Mutex
	write := func(value any) {
		outputMu.Lock()
		defer outputMu.Unlock()
		_ = json.NewEncoder(os.Stdout).Encode(value)
	}
	respond := func(request message, result any) {
		write(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": result})
	}
	var promptMu sync.Mutex
	var promptID json.RawMessage
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		var request message
		if json.Unmarshal(scanner.Bytes(), &request) != nil {
			continue
		}
		if request.Method == "" {
			appendFixtureLog(map[string]any{"kind": "request", "method": "permission-response", "params": json.RawMessage(scanner.Bytes())})
			if mode == "acp-interrupt" {
				promptMu.Lock()
				id := append(json.RawMessage(nil), promptID...)
				promptMu.Unlock()
				if len(id) != 0 {
					respond(message{ID: id}, map[string]any{"stopReason": "cancelled"})
				}
			}
			continue
		}
		appendFixtureLog(map[string]any{"kind": "request", "method": request.Method, "params": request.Params})
		switch request.Method {
		case "initialize":
			protocolVersion := 1
			if mode == "acp-version-mismatch" {
				protocolVersion = 2
			}
			respond(request, map[string]any{
				"protocolVersion":   protocolVersion,
				"agentCapabilities": map[string]any{"loadSession": true, "sessionCapabilities": map[string]any{"resume": map[string]any{}, "close": map[string]any{}}},
				"agentInfo":         map[string]string{"name": "fixture", "version": "1"},
			})
		case "session/new":
			respond(request, map[string]any{
				"sessionId": "acp-session",
				"configOptions": []any{
					map[string]any{"id": "model", "name": "Model", "category": "model", "type": "select", "currentValue": "default", "options": []any{map[string]any{"value": "model-a", "name": "Model A"}}},
					map[string]any{"id": "thought", "name": "Thought", "category": "thought_level", "type": "select", "currentValue": "medium", "options": []any{map[string]any{"value": "high", "name": "High"}}},
				},
			})
		case "session/resume":
			respond(request, map[string]any{})
		case "session/load":
			respond(request, nil)
		case "session/set_config_option":
			respond(request, map[string]any{"configOptions": []any{}})
		case "session/prompt":
			promptMu.Lock()
			promptID = append(json.RawMessage(nil), request.ID...)
			promptMu.Unlock()
			if mode == "acp-lifecycle" {
				write(map[string]any{"jsonrpc": "2.0", "id": 900, "method": "session/request_permission", "params": map[string]any{
					"sessionId": "acp-session", "toolCall": map[string]any{"toolCallId": "tool-1", "kind": "edit"},
					"options": []any{map[string]any{"optionId": "allow", "name": "Allow", "kind": "allow_once"}, map[string]any{"optionId": "reject", "name": "Reject", "kind": "reject_once"}},
				}})
				go func(id json.RawMessage) {
					time.Sleep(20 * time.Millisecond)
					write(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{"sessionId": "acp-session", "update": map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]any{"type": "text", "text": "hello "}}}})
					write(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{"sessionId": "acp-session", "update": map[string]any{"sessionUpdate": "tool_call", "toolCallId": "tool-1", "kind": "edit", "title": "Edit file", "status": "in_progress"}}})
					write(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{"sessionId": "acp-session", "update": map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]any{"type": "text", "text": "world"}}}})
					respond(message{ID: id}, map[string]any{"stopReason": "end_turn"})
				}(append(json.RawMessage(nil), request.ID...))
			}
		case "session/cancel":
			if mode == "acp-interrupt" {
				write(map[string]any{"jsonrpc": "2.0", "id": 901, "method": "session/request_permission", "params": map[string]any{
					"sessionId": "acp-session", "toolCall": map[string]any{"toolCallId": "tool-2", "kind": "read"},
					"options": []any{map[string]any{"optionId": "allow", "name": "Allow", "kind": "allow_once"}, map[string]any{"optionId": "reject", "name": "Reject", "kind": "reject_once"}},
				}})
				continue
			}
			promptMu.Lock()
			id := append(json.RawMessage(nil), promptID...)
			promptMu.Unlock()
			if len(id) != 0 {
				respond(message{ID: id}, map[string]any{"stopReason": "cancelled"})
			}
		}
	}
	return 0
}

func mustGetwd() string {
	cwd, _ := os.Getwd()
	return cwd
}

func appendFixtureLog(value any) {
	path := os.Getenv(fixtureLogEnv)
	if path == "" {
		return
	}
	data, err := json.Marshal(value)
	if err != nil {
		return
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	_, _ = file.Write(append(data, '\n'))
	_ = file.Close()
}

func runCodexFixture(mode string) int {
	type request struct {
		ID     int             `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	var outputMu sync.Mutex
	write := func(value any) {
		outputMu.Lock()
		defer outputMu.Unlock()
		_ = json.NewEncoder(os.Stdout).Encode(value)
	}
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		var message request
		if err := json.Unmarshal(scanner.Bytes(), &message); err != nil {
			continue
		}
		appendFixtureLog(map[string]any{"kind": "request", "method": message.Method, "params": json.RawMessage(message.Params)})
		switch message.Method {
		case "initialize":
			if mode == "codex-init-missing-method" {
				write(map[string]any{"id": message.ID, "error": map[string]any{"code": -32601, "message": "Method not found"}})
				continue
			}
			write(map[string]any{"id": message.ID, "result": map[string]any{}})
		case "thread/start", "thread/resume":
			if mode == "codex-resume-missing-method" && message.Method == "thread/resume" {
				write(map[string]any{"id": message.ID, "error": map[string]any{"code": -32601, "message": "Method not found"}})
				continue
			}
			if mode == "codex-thread-changed-field" && message.Method == "thread/start" {
				write(map[string]any{"id": message.ID, "result": map[string]any{"conversation": map[string]any{"id": "changed"}}})
				continue
			}
			threadID := "thr_test"
			if mode == "codex-interrupt" {
				threadID = "thr_stop"
			}
			write(map[string]any{"id": message.ID, "result": map[string]any{"thread": map[string]any{"id": threadID}}})
		case "turn/start":
			threadID, turnID := "thr_test", "turn_test"
			if mode == "codex-interrupt" {
				threadID, turnID = "thr_stop", "turn_stop"
			}
			write(map[string]any{"id": message.ID, "result": map[string]any{"turn": map[string]any{"id": turnID, "status": "inProgress"}}})
			if mode == "codex-terminal-changed-status" {
				write(map[string]any{"method": "turn/completed", "params": map[string]any{"threadId": threadID, "turn": map[string]any{"id": turnID, "status": "done"}}})
			}
			if mode == "codex-lifecycle" {
				go func() {
					time.Sleep(40 * time.Millisecond)
					write(map[string]any{"method": "item/agentMessage/delta", "params": map[string]any{"threadId": threadID, "turnId": turnID, "delta": "hello "}})
					time.Sleep(40 * time.Millisecond)
					write(map[string]any{"method": "item/agentMessage/delta", "params": map[string]any{"threadId": threadID, "turnId": turnID, "delta": "world"}})
					write(map[string]any{"method": "turn/completed", "params": map[string]any{"threadId": threadID, "turn": map[string]any{"id": turnID, "status": "completed"}}})
				}()
			}
		case "turn/steer":
			write(map[string]any{"id": message.ID, "result": map[string]any{"turnId": "turn_test"}})
		case "turn/interrupt":
			write(map[string]any{"id": message.ID, "result": map[string]any{}})
			write(map[string]any{"method": "turn/completed", "params": map[string]any{"threadId": "thr_stop", "turn": map[string]any{"id": "turn_stop", "status": "interrupted"}}})
		case "model/list":
			write(map[string]any{"id": message.ID, "result": map[string]any{"data": []any{map[string]any{"id": "model-a", "model": "model-a", "displayName": "Model A", "defaultReasoningEffort": "medium", "supportedReasoningEfforts": []any{map[string]any{"reasoningEffort": "low"}, map[string]any{"reasoningEffort": "medium"}}, "isDefault": true}}, "nextCursor": nil}})
		}
	}
	return 0
}

func runClaudeFixture(mode string) int {
	if len(os.Args) > 1 && os.Args[1] == "--help" {
		help := "--print --input-format --output-format --verbose --include-partial-messages --resume --model --effort --permission-mode --allowedTools --dangerously-skip-permissions"
		if mode == "claude-help-missing-flag" {
			help = "--print --input-format --output-format --verbose --resume --model --effort --permission-mode --allowedTools --dangerously-skip-permissions"
		}
		_, _ = fmt.Fprintln(os.Stdout, help)
		return 0
	}
	write := func(value any) { _ = json.NewEncoder(os.Stdout).Encode(value) }
	stream := func(session, text string) {
		write(map[string]any{"type": "stream_event", "session_id": session, "event": map[string]any{"type": "content_block_delta", "delta": map[string]any{"type": "text_delta", "text": text}}})
	}
	scanner := bufio.NewScanner(os.Stdin)
	if mode == "claude-text" {
		input, _ := io.ReadAll(os.Stdin)
		appendFixtureLog(map[string]any{"kind": "input", "text": string(input)})
		write(map[string]any{"type": "system", "subtype": "init", "session_id": "text-session"})
		write(map[string]any{"type": "result", "subtype": "success", "session_id": "text-session", "is_error": false, "result": "tool done"})
		return 0
	}
	if !scanner.Scan() {
		return 1
	}
	appendFixtureLog(map[string]any{"kind": "input", "text": scanner.Text()})
	session := "claude-session"
	if mode == "claude-steer" {
		session = "steered-session"
	} else if mode == "claude-interrupt" {
		session = "interrupt-session"
	}
	if mode == "claude-init-changed-event" {
		write(map[string]any{"type": "system", "subtype": "startup", "session_id": session})
	} else {
		write(map[string]any{"type": "system", "subtype": "init", "session_id": session})
	}
	write(map[string]any{"type": "stream_event", "session_id": session, "event": map[string]any{"type": "message_start"}})
	if mode == "claude-steer" {
		stream(session, "first")
		if !scanner.Scan() {
			return 1
		}
		appendFixtureLog(map[string]any{"kind": "input", "text": scanner.Text()})
		stream(session, " second")
		write(map[string]any{"type": "result", "subtype": "success", "session_id": session, "is_error": false, "result": "first second"})
	} else if mode == "claude-interrupt" {
		stream(session, "working")
		for scanner.Scan() {
		}
		return 0
	} else if mode == "claude-terminal-error" {
		write(map[string]any{"type": "result", "subtype": "error_max_turns", "session_id": session, "is_error": true, "result": "maximum turns exceeded"})
	} else {
		stream(session, "progress")
		write(map[string]any{"type": "stream_event", "session_id": session, "event": map[string]any{"type": "message_start"}})
		stream(session, "hello")
		write(map[string]any{"type": "result", "subtype": "success", "session_id": session, "is_error": false, "result": "progress\nhello"})
	}
	for scanner.Scan() {
		appendFixtureLog(map[string]any{"kind": "input", "text": scanner.Text()})
	}
	if mode == "claude-result-nonzero" {
		_, _ = fmt.Fprintln(os.Stderr, "post-result process failure")
		return 7
	}
	return 0
}
