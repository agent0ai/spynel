package app

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	maxRuntimeLogFiles = 8
	maxRuntimeLogBytes = 2 << 20
	runtimeLogWait     = 2 * time.Second
	maxRuntimeJSONLine = maxLogEntryRunes*6 + 4096
)

var urlUserinfoPattern = regexp.MustCompile(`(?i)(\b[a-z][a-z0-9+.-]*://)[^/?#\s'"<>]+@([^/?#\s'"<>])`)

var secretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)(authorization\s*[:=]\s*)[^\r\n]+`),
	regexp.MustCompile(`(?i)(\bbearer[ \t]+)[A-Za-z0-9._~+/=-]+`),
	regexp.MustCompile(`\b\d{8,10}:[A-Za-z0-9_-]{20,}\b`), // Telegram bot tokens.
}

var quotedSecretPatterns = []struct {
	prefix *regexp.Regexp
	quote  byte
}{
	{regexp.MustCompile(`(?i)(?:["'][^"'\r\n]*(?:authorization|token|secret|password|passwd|passphrase|api[_-]?key|access[_-]?key|signing[_-]?key|encryption[_-]?key|ssh[_-]?key)[^"'\r\n]*["']|\b(?:authorization|auth|access[_-]?token|api[_-]?key|bot[_-]?token|secret|password|passphrase|signing[_-]?key|encryption[_-]?key|ssh[_-]?key)\b|\b[A-Z][A-Z0-9_]*(?:TOKEN|SECRET|PASSWORD|PASSWD|PASSPHRASE|API_KEY|ACCESS_KEY|SIGNING_KEY|ENCRYPTION_KEY|SSH_KEY)[A-Z0-9_]*\b)\s*[:=]\s*"`), '"'},
	{regexp.MustCompile(`(?i)(?:["'][^"'\r\n]*(?:authorization|token|secret|password|passwd|passphrase|api[_-]?key|access[_-]?key|signing[_-]?key|encryption[_-]?key|ssh[_-]?key)[^"'\r\n]*["']|\b(?:authorization|auth|access[_-]?token|api[_-]?key|bot[_-]?token|secret|password|passphrase|signing[_-]?key|encryption[_-]?key|ssh[_-]?key)\b|\b[A-Z][A-Z0-9_]*(?:TOKEN|SECRET|PASSWORD|PASSWD|PASSPHRASE|API_KEY|ACCESS_KEY|SIGNING_KEY|ENCRYPTION_KEY|SSH_KEY)[A-Z0-9_]*\b)\s*[:=]\s*'`), '\''},
}

var unquotedSecretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)(["'](?:auth|authorization|access[_-]?token|api[_-]?key|bot[_-]?token|credentials?|private|private(?:[_.-]|[ \t]+)?key|signing(?:[_.-]|[ \t]+)?key|encryption(?:[_.-]|[ \t]+)?key|ssh(?:[_.-]|[ \t]+)?key|secret|password|passphrase)["']\s*:\s*(?:bearer\s+)?)([^\r\n]*)`),
	regexp.MustCompile(`(?i)((?:auth|authorization|access[_-]?token|api[_-]?key|bot[_-]?token|credentials?|private|private(?:[_.-]|[ \t]+)?key|signing(?:[_.-]|[ \t]+)?key|encryption(?:[_.-]|[ \t]+)?key|ssh(?:[_.-]|[ \t]+)?key|secret|password|passphrase|[A-Za-z0-9_.-]*(?:credential|private(?:[_.-]|[ \t]+)?key|signing(?:[_.-]|[ \t]+)?key|encryption(?:[_.-]|[ \t]+)?key|ssh(?:[_.-]|[ \t]+)?key|passphrase)[A-Za-z0-9_.-]*)\s*[:=]\s*)([^\r\n]*)`),
	regexp.MustCompile(`(?i)(\b[A-Z][A-Z0-9_]*(?:TOKEN|SECRET|PASSWORD|PASSWD|PASSPHRASE|API_KEY|ACCESS_KEY|CREDENTIALS?|PRIVATE_KEY|SIGNING_KEY|ENCRYPTION_KEY|SSH_KEY)[A-Z0-9_]*\s*=\s*)([^\r\n]*)`),
}

var yamlCommentPattern = regexp.MustCompile(`\s+#`)
var bareStructuredSecretPrefix = regexp.MustCompile(`(?im)(?:^|[\r\n])[ \t]*(?:auth|authorization|access[_-]?token|api[_-]?key|bot[_-]?token|credentials?|private|private(?:[_.-]|[ \t]+)?key|signed[_-]?identity[_-]?key|noise[_-]?key|signed[_-]?pre[_-]?key|pairing[_-]?ephemeral[_-]?key(?:pair)?|adv[_-]?secret[_-]?key|signing(?:[_.-]|[ \t]+)?key|encryption(?:[_.-]|[ \t]+)?key|ssh(?:[_.-]|[ \t]+)?key|secret|password|passphrase|[A-Za-z0-9_.-]*(?:credential|private(?:[_.-]|[ \t]+)?key|signing(?:[_.-]|[ \t]+)?key|encryption(?:[_.-]|[ \t]+)?key|ssh(?:[_.-]|[ \t]+)?key|passphrase)[A-Za-z0-9_.-]*|[A-Z][A-Z0-9_.-]*(?:TOKEN|SECRET|PASSWORD|PASSWD|PASSPHRASE|API_KEY|ACCESS_KEY|AUTH|AUTHORIZATION|CREDENTIALS?|PRIVATE_KEY|SIGNING_KEY|ENCRYPTION_KEY|SSH_KEY)[A-Z0-9_.-]*)\s*[:=]\s*`)
var nestedPrivateFieldPrefix = regexp.MustCompile(`(?i)(?:^|[,{[:space:]])priv[[:space:]]*[:=][[:space:]]*`)

type runtimeLogPersistence struct {
	mu        sync.Mutex
	queueMu   sync.Mutex
	directory string
	instance  string
	file      *os.File
	path      string
	size      int64
	part      int
	queue     chan persistenceRequest
	done      chan error
	closed    bool
	wait      time.Duration
	failures  chan error
}

type persistenceRequest struct {
	kind  string
	entry LogEntry
	flush bool
	ack   chan error
}

type restoredLogEntry struct {
	LogEntry
	pathIndex int
	lineIndex int
}

type attributedLogWriter struct {
	runtime   *Runtime
	component string
}

func (w *attributedLogWriter) Write(data []byte) (int, error) {
	return w.runtime.writeAttributed(w.component, data)
}

func newRuntimeLogPersistence(directory, instance string) *runtimeLogPersistence {
	if strings.TrimSpace(instance) == "" {
		instance = fmt.Sprintf("pid-%d", os.Getpid())
	}
	persistence := &runtimeLogPersistence{
		directory: directory, instance: logField(instance, "unknown"),
		queue: make(chan persistenceRequest, 1024), done: make(chan error, 1),
		failures: make(chan error, 16), wait: runtimeLogWait,
	}
	go persistence.run()
	return persistence
}

func (p *runtimeLogPersistence) restore() ([]LogEntry, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := os.MkdirAll(p.directory, 0o700); err != nil {
		return nil, err
	}
	var paths []string
	var restoreErrors []error
	lockErr := p.withDirectoryLock(func() error {
		var err error
		paths, err = filepath.Glob(filepath.Join(p.directory, "runtime-*.jsonl"))
		if err != nil {
			return err
		}
		paths, err = pruneRuntimeLogFiles(paths, maxRuntimeLogFiles)
		return err
	})
	if lockErr != nil {
		restoreErrors = append(restoreErrors, lockErr)
		var err error
		paths, err = filepath.Glob(filepath.Join(p.directory, "runtime-*.jsonl"))
		if err != nil {
			restoreErrors = append(restoreErrors, err)
		}
	}
	sort.Strings(paths)
	restored := make([]restoredLogEntry, 0, maxLogEntries)
	for pathIndex, path := range paths {
		file, err := os.Open(path)
		if err != nil {
			restoreErrors = append(restoreErrors, fmt.Errorf("open %s: %w", filepath.Base(path), err))
			continue
		}
		scanner := bufio.NewScanner(file)
		scanner.Buffer(make([]byte, 4096), maxRuntimeJSONLine)
		lineIndex := 0
		for scanner.Scan() {
			lineIndex++
			var entry LogEntry
			if json.Unmarshal(scanner.Bytes(), &entry) != nil || entry.At.IsZero() || strings.TrimSpace(entry.Text) == "" {
				continue // tolerate corrupt records and a partial final append
			}
			entry.Text = boundAndRedactLogText(entry.Text)
			if entry.Text == "" {
				continue
			}
			entry.Level = logField(entry.Level, "info")
			entry.Component = logField(entry.Component, "runtime")
			entry.Event = logField(entry.Event, "event")
			entry.Instance = logField(entry.Instance, "unknown")
			restored = append(restored, restoredLogEntry{
				LogEntry: entry, pathIndex: pathIndex, lineIndex: lineIndex,
			})
		}
		if err := scanner.Err(); err != nil {
			restoreErrors = append(restoreErrors, fmt.Errorf("scan %s: %w", filepath.Base(path), err))
		}
		if err := file.Close(); err != nil {
			restoreErrors = append(restoreErrors, fmt.Errorf("close %s: %w", filepath.Base(path), err))
		}
	}
	sort.SliceStable(restored, func(i, j int) bool {
		if !restored[i].At.Equal(restored[j].At) {
			return restored[i].At.Before(restored[j].At)
		}
		if restored[i].pathIndex != restored[j].pathIndex {
			return restored[i].pathIndex < restored[j].pathIndex
		}
		return restored[i].lineIndex < restored[j].lineIndex
	})
	start := max(0, len(restored)-maxLogEntries)
	entries := make([]LogEntry, 0, len(restored)-start)
	for _, restoredEntry := range restored[start:] {
		entries = append(entries, restoredEntry.LogEntry)
	}
	return entries, errors.Join(restoreErrors...)
}

func (p *runtimeLogPersistence) append(entry LogEntry, flush bool) error {
	request := persistenceRequest{kind: "append", entry: entry, flush: flush}
	if flush {
		request.ack = make(chan error, 1)
	}
	p.queueMu.Lock()
	if p.closed {
		p.queueMu.Unlock()
		return errors.New("runtime log is closed")
	}
	if flush {
		select {
		case p.queue <- request:
		case <-time.After(p.wait):
			p.queueMu.Unlock()
			return errors.New("runtime log persistence queue timed out")
		}
	} else {
		select {
		case p.queue <- request:
		default:
			p.queueMu.Unlock()
			return errors.New("runtime log persistence queue is full")
		}
	}
	p.queueMu.Unlock()
	if request.ack != nil {
		select {
		case err := <-request.ack:
			return err
		case <-time.After(p.wait):
			return errors.New("runtime log persistence acknowledgement timed out")
		}
	}
	return nil
}

func (p *runtimeLogPersistence) appendSync(entry LogEntry, flush bool) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	data, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if p.file == nil || p.size+int64(len(data)) > maxRuntimeLogBytes {
		if err := p.rotateLocked(); err != nil {
			return err
		}
	}
	written, err := p.file.Write(data)
	p.size += int64(written)
	if err == nil && flush {
		err = p.file.Sync()
	}
	return err
}

func (p *runtimeLogPersistence) rotateLocked() error {
	if p.file != nil {
		syncErr := p.file.Sync()
		closeErr := p.file.Close()
		p.file = nil
		if syncErr != nil {
			return syncErr
		}
		if closeErr != nil {
			return closeErr
		}
	}
	return p.withDirectoryLock(func() error {
		paths, err := filepath.Glob(filepath.Join(p.directory, "runtime-*.jsonl"))
		if err != nil {
			return err
		}
		remaining, err := pruneRuntimeLogFiles(paths, maxRuntimeLogFiles-1)
		if err != nil {
			return err
		}
		if len(remaining) >= maxRuntimeLogFiles {
			return errors.New("runtime log retention is occupied by active sessions")
		}
		p.part++
		stamp := time.Now().UTC().Format("20060102T150405.000000000Z")
		p.path = filepath.Join(p.directory, fmt.Sprintf("runtime-%s-%s-%02d.jsonl", stamp, p.instance, p.part))
		file, err := os.OpenFile(p.path, os.O_CREATE|os.O_APPEND|os.O_RDWR, 0o600)
		if err != nil {
			return err
		}
		if err := lockRuntimeFile(file); err != nil {
			_ = file.Close()
			return err
		}
		p.file = file
		info, err := file.Stat()
		if err != nil {
			_ = file.Close()
			p.file = nil
			return err
		}
		p.size = info.Size()
		return nil
	})
}

func (p *runtimeLogPersistence) clear() error {
	return p.control("clear")
}

func (p *runtimeLogPersistence) clearSync() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	var first error
	if p.file != nil {
		first = p.file.Close()
		p.file = nil
	}
	lockErr := p.withDirectoryLock(func() error {
		paths, globErr := filepath.Glob(filepath.Join(p.directory, "runtime-*.jsonl"))
		if globErr != nil {
			return globErr
		}
		remaining, pruneErr := pruneRuntimeLogFiles(paths, 0)
		if first == nil {
			first = pruneErr
		}
		if len(remaining) > 0 && first == nil {
			first = fmt.Errorf("%d active runtime log files remain", len(remaining))
		}
		return nil
	})
	if first == nil {
		first = lockErr
	}
	p.size = 0
	return first
}

func (p *runtimeLogPersistence) withDirectoryLock(action func() error) error {
	if err := os.MkdirAll(p.directory, 0o700); err != nil {
		return err
	}
	file, err := os.OpenFile(filepath.Join(p.directory, "runtime.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer file.Close()
	wait := p.wait
	if wait <= 0 {
		wait = runtimeLogWait
	}
	deadline := time.Now().Add(wait)
	for {
		available, lockErr := tryLockRuntimeFile(file)
		if lockErr != nil {
			return lockErr
		}
		if available {
			break
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return fmt.Errorf("runtime log directory lock timed out after %s", wait)
		}
		delay := min(10*time.Millisecond, remaining)
		time.Sleep(delay)
	}
	defer unlockRuntimeFile(file)
	return action()
}

// pruneRuntimeLogFiles removes only inactive session files. Every writer holds
// an advisory lock on its current file for the complete open lifetime, so one
// process can never unlink another process's crash evidence.
func pruneRuntimeLogFiles(paths []string, target int) ([]string, error) {
	sort.Strings(paths)
	remaining := append([]string(nil), paths...)
	var first error
	for index := 0; len(remaining) > target && index < len(remaining); {
		path := remaining[index]
		file, err := os.OpenFile(path, os.O_RDWR, 0)
		if err != nil {
			if os.IsNotExist(err) {
				remaining = append(remaining[:index], remaining[index+1:]...)
				continue
			}
			if first == nil {
				first = err
			}
			index++
			continue
		}
		available, lockErr := tryLockRuntimeFile(file)
		_ = file.Close()
		if lockErr != nil {
			if first == nil {
				first = lockErr
			}
			index++
			continue
		}
		if !available {
			index++
			continue
		}
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			if first == nil {
				first = err
			}
			index++
			continue
		}
		remaining = append(remaining[:index], remaining[index+1:]...)
	}
	return remaining, first
}

func (p *runtimeLogPersistence) close() error {
	p.queueMu.Lock()
	if p.closed {
		p.queueMu.Unlock()
		return nil
	}
	p.closed = true
	close(p.queue)
	p.queueMu.Unlock()
	select {
	case err := <-p.done:
		return err
	case <-time.After(p.wait):
		return errors.New("runtime log persistence shutdown timed out")
	}
}

func (p *runtimeLogPersistence) control(kind string) error {
	request := persistenceRequest{kind: kind, ack: make(chan error, 1)}
	p.queueMu.Lock()
	if p.closed {
		p.queueMu.Unlock()
		return errors.New("runtime log is closed")
	}
	select {
	case p.queue <- request:
	case <-time.After(p.wait):
		p.queueMu.Unlock()
		return errors.New("runtime log persistence control queue timed out")
	}
	p.queueMu.Unlock()
	select {
	case err := <-request.ack:
		return err
	case <-time.After(p.wait):
		return errors.New("runtime log persistence control acknowledgement timed out")
	}
}

func (p *runtimeLogPersistence) run() {
	for request := range p.queue {
		var err error
		switch request.kind {
		case "append":
			err = p.appendSync(request.entry, request.flush)
		case "clear":
			err = p.clearSync()
		case "flush":
			p.mu.Lock()
			if p.file != nil {
				err = p.file.Sync()
			}
			p.mu.Unlock()
		}
		if request.ack != nil {
			request.ack <- err
		} else if err != nil {
			select {
			case p.failures <- err:
			default:
			}
		}
	}
	p.mu.Lock()
	var err error
	if p.file != nil {
		err = p.file.Sync()
		if closeErr := p.file.Close(); err == nil {
			err = closeErr
		}
		p.file = nil
	}
	p.mu.Unlock()
	close(p.failures)
	p.done <- err
}

func boundAndRedactLogText(message string) string {
	message = redactLogText(message)
	if runes := []rune(message); len(runes) > maxLogEntryRunes {
		message = string(runes[:maxLogEntryRunes-1]) + "…"
	}
	return message
}

func redactLogText(message string) string {
	message = strings.TrimSpace(sanitizeLogText(message))
	message = urlUserinfoPattern.ReplaceAllString(message, "${1}[REDACTED]@${2}")
	message = redactNestedPrivateFields(message)
	message = redactBareStructuredSecrets(message)
	message = redactStructuredQuotedSecrets(message)
	for _, pattern := range secretPatterns {
		message = pattern.ReplaceAllString(message, "${1}[REDACTED]")
	}
	for _, pattern := range quotedSecretPatterns {
		message = redactQuotedSecrets(message, pattern.prefix, pattern.quote)
	}
	for _, pattern := range unquotedSecretPatterns {
		message = redactUnquotedSecrets(message, pattern)
	}
	return message
}

// redactNestedPrivateFields covers the Priv member used by whatsmeow's
// keys.KeyPair in both JSON and fmt-style diagnostics. It redacts only the
// private scalar so adjacent public key and status fields remain useful.
func redactNestedPrivateFields(message string) string {
	var redacted strings.Builder
	cursor := 0
	search := 0
	for search < len(message) {
		match := nestedPrivateFieldPrefix.FindStringIndex(message[search:])
		if match == nil {
			break
		}
		valueStart := search + match[1]
		if valueStart >= len(message) {
			break
		}
		valueEnd := valueStart
		if quote := message[valueStart]; quote == '"' || quote == '\'' {
			valueEnd++
			valueEnd += structuredQuotedScalarEnd(message[valueEnd:], quote)
			if valueEnd < len(message) && message[valueEnd] == quote {
				valueEnd++
			}
		} else if compositeEnd, ok := structuredCompositeScalarEnd(message[valueStart:]); ok {
			valueEnd += compositeEnd
		} else {
			for valueEnd < len(message) && message[valueEnd] != ',' && message[valueEnd] != '}' &&
				message[valueEnd] != '\n' && message[valueEnd] != '\r' && message[valueEnd] != ' ' && message[valueEnd] != '\t' {
				valueEnd++
			}
		}
		redacted.WriteString(message[cursor:valueStart])
		redacted.WriteString("[REDACTED]")
		cursor = valueEnd
		search = valueEnd
	}
	if cursor == 0 {
		return message
	}
	redacted.WriteString(message[cursor:])
	return redacted.String()
}

func redactBareStructuredSecrets(message string) string {
	var redacted strings.Builder
	cursor := 0
	search := 0
	for search < len(message) {
		match := bareStructuredSecretPrefix.FindStringIndex(message[search:])
		if match == nil {
			break
		}
		valueStart := search + match[1]
		if valueStart >= len(message) || message[valueStart] == '\n' || message[valueStart] == '\r' || message[valueStart] == '#' {
			search = valueStart
			continue
		}
		if message[valueStart] == '"' || message[valueStart] == '\'' {
			quote := message[valueStart]
			valueEnd := structuredQuotedScalarEnd(message[valueStart+1:], quote)
			redacted.WriteString(message[cursor : valueStart+1])
			redacted.WriteString("[REDACTED]")
			cursor = valueStart + 1 + valueEnd
			search = min(len(message), cursor+1)
			continue
		}

		valueEnd := valueStart
		for valueEnd < len(message) && message[valueEnd] != '\n' && message[valueEnd] != '\r' {
			valueEnd++
		}
		value := message[valueStart:valueEnd]
		comment := yamlCommentPattern.FindStringIndex(value)
		redacted.WriteString(message[cursor:valueStart])
		redacted.WriteString("[REDACTED]")
		if compositeEnd, ok := structuredCompositeScalarEnd(message[valueStart:]); ok {
			valueEnd = valueStart + compositeEnd
		} else if pemEnd, ok := structuredPEMScalarEnd(message, valueStart, valueEnd); ok {
			if comment != nil {
				redacted.WriteString(value[comment[0]:])
			}
			valueEnd = pemEnd
		} else if isYAMLBlockScalarHeader(value) {
			valueEnd = yamlBlockScalarEnd(message, valueEnd, logLineIndent(message, valueStart))
		} else if comment != nil {
			redacted.WriteString(value[comment[0]:])
		}
		cursor = valueEnd
		search = valueEnd
	}
	if cursor == 0 {
		return message
	}
	redacted.WriteString(message[cursor:])
	return redacted.String()
}

func redactStructuredQuotedSecrets(message string) string {
	var redacted strings.Builder
	cursor := 0
	for index := 0; index < len(message); index++ {
		quote := message[index]
		if quote != '"' && quote != '\'' {
			continue
		}
		keyValue := message[index+1:]
		keyEnd := quotedScalarEnd(keyValue, quote)
		if keyEnd >= len(keyValue) || keyValue[keyEnd] != quote {
			continue
		}
		key, ok := decodeLogKey(message[index:index+keyEnd+2], quote)
		if !ok || !isSensitiveLogKey(key) {
			index += keyEnd + 1
			continue
		}
		valueStart := index + keyEnd + 2
		for valueStart < len(message) && (message[valueStart] == ' ' || message[valueStart] == '\t') {
			valueStart++
		}
		if valueStart >= len(message) || message[valueStart] != ':' && message[valueStart] != '=' {
			index += keyEnd + 1
			continue
		}
		valueStart++
		for valueStart < len(message) && (message[valueStart] == ' ' || message[valueStart] == '\t') {
			valueStart++
		}
		if valueStart >= len(message) || message[valueStart] == '\n' || message[valueStart] == '\r' || message[valueStart] == '#' {
			index += keyEnd + 1
			continue
		}
		if message[valueStart] != '"' && message[valueStart] != '\'' {
			valueEnd := valueStart
			for valueEnd < len(message) && message[valueEnd] != '\n' && message[valueEnd] != '\r' {
				valueEnd++
			}
			value := message[valueStart:valueEnd]
			comment := yamlCommentPattern.FindStringIndex(value)
			redacted.WriteString(message[cursor:valueStart])
			redacted.WriteString("[REDACTED]")
			if compositeEnd, ok := structuredCompositeScalarEnd(message[valueStart:]); ok {
				valueEnd = valueStart + compositeEnd
			} else if pemEnd, ok := structuredPEMScalarEnd(message, valueStart, valueEnd); ok {
				if comment != nil {
					redacted.WriteString(value[comment[0]:])
				}
				valueEnd = pemEnd
			} else if isYAMLBlockScalarHeader(value) {
				valueEnd = yamlBlockScalarEnd(message, valueEnd, logLineIndent(message, index))
			} else if comment != nil {
				redacted.WriteString(value[comment[0]:])
			}
			cursor = valueEnd
			index = valueEnd
			continue
		}

		valueQuote := message[valueStart]
		value := message[valueStart+1:]
		valueEnd := structuredQuotedScalarEnd(value, valueQuote)
		redacted.WriteString(message[cursor : valueStart+1])
		redacted.WriteString("[REDACTED]")
		cursor = valueStart + 1 + valueEnd
		index = cursor
	}
	if cursor == 0 {
		return message
	}
	redacted.WriteString(message[cursor:])
	return redacted.String()
}

func logLineIndent(message string, position int) int {
	lineStart := position
	for lineStart > 0 && message[lineStart-1] != '\n' && message[lineStart-1] != '\r' {
		lineStart--
	}
	indent := 0
	for lineStart+indent < len(message) && (message[lineStart+indent] == ' ' || message[lineStart+indent] == '\t') {
		indent++
	}
	return indent
}

func isYAMLBlockScalarHeader(value string) bool {
	if comment := yamlCommentPattern.FindStringIndex(value); comment != nil {
		value = value[:comment[0]]
	}
	fields := strings.Fields(value)
	if len(fields) == 0 {
		return false
	}
	style := fields[len(fields)-1]
	if style[0] != '|' && style[0] != '>' {
		return false
	}
	for _, character := range style[1:] {
		if character != '+' && character != '-' && (character < '0' || character > '9') {
			return false
		}
	}
	return true
}

func yamlBlockScalarEnd(message string, headerEnd, keyIndent int) int {
	position := headerEnd
	for position < len(message) && (message[position] == '\n' || message[position] == '\r') {
		separator := position
		if message[position] == '\r' && position+1 < len(message) && message[position+1] == '\n' {
			position += 2
		} else {
			position++
		}
		lineStart := position
		for position < len(message) && message[position] != '\n' && message[position] != '\r' {
			position++
		}
		if position > lineStart {
			indent := 0
			for lineStart+indent < position && (message[lineStart+indent] == ' ' || message[lineStart+indent] == '\t') {
				indent++
			}
			if indent == position-lineStart {
				continue
			}
			if indent <= keyIndent {
				return separator
			}
		}
	}
	return position
}

func structuredQuotedScalarEnd(value string, quote byte) int {
	for index := 0; index < len(value); index++ {
		switch value[index] {
		case '\\':
			if quote == '"' && index+1 < len(value) && value[index+1] != '\n' && value[index+1] != '\r' {
				index++
			}
		case quote:
			if quote == '\'' && index+1 < len(value) && value[index+1] == quote {
				index++
				continue
			}
			return index
		case '\n', '\r':
			position := index
			for position < len(value) && (value[position] == '\n' || value[position] == '\r') {
				separator := position
				if value[position] == '\r' && position+1 < len(value) && value[position+1] == '\n' {
					position += 2
				} else {
					position++
				}
				lineStart := position
				for position < len(value) && value[position] != '\n' && value[position] != '\r' {
					position++
				}
				if position == lineStart {
					continue
				}
				if value[lineStart] == ' ' || value[lineStart] == '\t' {
					index = lineStart - 1
					break
				}
				if value[lineStart] == quote {
					return lineStart
				}
				return separator
			}
		}
	}
	return len(value)
}

func structuredCompositeScalarEnd(value string) (int, bool) {
	if len(value) == 0 || value[0] != '{' && value[0] != '[' {
		return 0, false
	}
	stack := []byte{value[0]}
	var quote byte
	for index := 1; index < len(value); index++ {
		character := value[index]
		if quote != 0 {
			if quote == '"' && character == '\\' && index+1 < len(value) {
				index++
				continue
			}
			if character == quote {
				if quote == '\'' && index+1 < len(value) && value[index+1] == quote {
					index++
					continue
				}
				quote = 0
			}
			continue
		}
		switch character {
		case '"', '\'':
			quote = character
		case '{', '[':
			stack = append(stack, character)
		case '}', ']':
			opening := stack[len(stack)-1]
			if opening == '{' && character != '}' || opening == '[' && character != ']' {
				return len(value), true
			}
			stack = stack[:len(stack)-1]
			if len(stack) == 0 {
				return index + 1, true
			}
		}
	}
	return len(value), true
}

func structuredPEMScalarEnd(message string, valueStart, lineEnd int) (int, bool) {
	header := message[valueStart:lineEnd]
	if comment := yamlCommentPattern.FindStringIndex(header); comment != nil {
		header = header[:comment[0]]
	}
	header = strings.TrimSpace(header)
	if !strings.HasPrefix(header, "-----BEGIN ") || !strings.HasSuffix(header, "-----") {
		return 0, false
	}
	label := strings.TrimSuffix(strings.TrimPrefix(header, "-----BEGIN "), "-----")
	if strings.TrimSpace(label) == "" {
		return 0, false
	}
	footer := "-----END " + label + "-----"
	position := lineEnd
	for position < len(message) {
		for position < len(message) && (message[position] == '\n' || message[position] == '\r') {
			position++
		}
		end := position
		for end < len(message) && message[end] != '\n' && message[end] != '\r' {
			end++
		}
		line := message[position:end]
		comment := yamlCommentPattern.FindStringIndex(line)
		content := line
		if comment != nil {
			content = line[:comment[0]]
		}
		if strings.TrimSpace(content) == footer {
			if comment != nil {
				return position + comment[0], true
			}
			return end, true
		}
		position = end
	}
	return len(message), true
}

func decodeLogKey(raw string, quote byte) (string, bool) {
	if quote == '"' {
		decoded, err := strconv.Unquote(raw)
		return decoded, err == nil
	}
	if len(raw) < 2 || raw[0] != '\'' || raw[len(raw)-1] != '\'' {
		return "", false
	}
	return strings.ReplaceAll(raw[1:len(raw)-1], "''", "'"), true
}

func isSensitiveLogKey(key string) bool {
	var normalized strings.Builder
	for _, character := range strings.ToLower(key) {
		if character >= 'a' && character <= 'z' || character >= '0' && character <= '9' {
			normalized.WriteRune(character)
		}
	}
	value := normalized.String()
	if value == "auth" || strings.HasSuffix(value, "auth") {
		return true
	}
	if value == "private" || value == "priv" {
		return true
	}
	for _, indicator := range []string{"authorization", "credential", "privatekey", "signedidentitykey", "noisekey", "signedprekey", "pairingephemeralkey", "advsecretkey", "signingkey", "encryptionkey", "sshkey", "accesstoken", "apikey", "accesskey", "bottoken", "token", "secret", "password", "passwd", "passphrase"} {
		if strings.Contains(value, indicator) {
			return true
		}
	}
	return false
}

func redactQuotedSecrets(message string, prefixPattern *regexp.Regexp, quote byte) string {
	var redacted strings.Builder
	remaining := message
	for {
		prefix := prefixPattern.FindStringIndex(remaining)
		if prefix == nil {
			redacted.WriteString(remaining)
			return redacted.String()
		}
		redacted.WriteString(remaining[:prefix[1]])
		redacted.WriteString("[REDACTED]")

		value := remaining[prefix[1]:]
		end := structuredQuotedScalarEnd(value, quote)
		remaining = value[end:]
	}
}

func quotedScalarEnd(value string, quote byte) int {
	for index := 0; index < len(value); index++ {
		switch value[index] {
		case '\\':
			if quote == '"' && index+1 < len(value) && value[index+1] != '\n' && value[index+1] != '\r' {
				index++
			}
		case quote:
			if quote == '\'' && index+1 < len(value) && value[index+1] == quote {
				index++
				continue
			}
			return index
		case '\n', '\r':
			return index
		}
	}
	return len(value)
}

func redactUnquotedSecrets(message string, pattern *regexp.Regexp) string {
	return pattern.ReplaceAllStringFunc(message, func(match string) string {
		parts := pattern.FindStringSubmatchIndex(match)
		if len(parts) < 6 {
			return match
		}
		prefix := match[parts[2]:parts[3]]
		value := match[parts[4]:parts[5]]
		trimmed := strings.TrimSpace(value)
		if strings.HasPrefix(trimmed, `"`) || strings.HasPrefix(trimmed, `'`) || strings.HasPrefix(trimmed, "[REDACTED]") {
			return match
		}
		boundary := yamlCommentPattern.FindStringIndex(value)
		if boundary == nil {
			return prefix + "[REDACTED]"
		}
		return prefix + "[REDACTED]" + value[boundary[0]:]
	})
}

func logField(value, fallback string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	var clean strings.Builder
	for _, character := range value {
		if character >= 'a' && character <= 'z' || character >= '0' && character <= '9' || character == '_' || character == '-' || character == '.' {
			clean.WriteRune(character)
		}
		if clean.Len() == 48 {
			break
		}
	}
	if clean.Len() == 0 {
		return fallback
	}
	return clean.String()
}
