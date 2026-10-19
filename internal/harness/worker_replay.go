package harness

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"unicode/utf8"

	"github.com/agent0ai/spynel/internal/core"
	"github.com/agent0ai/spynel/internal/fsx"
	"github.com/agent0ai/spynel/internal/updater"
)

// The caller holds the harness lock and exact election fence. Copy before
// publishing the successor so a crash always leaves a discoverable complete copy.
func (h *workerHost) recoverPrevious(cfg HarnessConfig) (string, error) {
	prior, err := readWorkerRecord(h.recordPath)
	if os.IsNotExist(err) || errors.Is(err, errWorkerProtocol) {
		return "", nil // Incompatible schemas retain their existing confirmed-death policy.
	}
	if err != nil || !errors.Is(updater.VerifyProcess(prior.Info.Process), syscall.ESRCH) ||
		prior.Info.EnvironmentID != h.info.EnvironmentID || prior.Info.WorkspaceID != h.info.WorkspaceID || prior.Info.ConfigDigest != ConfigDigest(cfg) {
		return "", errors.New("prior worker journal identity unavailable")
	}
	directory := filepath.Dir(prior.Info.Socket)
	info, err := os.Lstat(directory)
	if err != nil || !info.IsDir() || info.Mode().Perm()&0077 != 0 || directory == h.directory || !strings.HasPrefix(filepath.Base(directory), "spynel-worker-") {
		return "", errors.New("prior worker journal directory unsafe")
	}
	entries, err := os.ReadDir(directory)
	if err != nil || len(entries) > workerMaxExecutions*3+1 {
		return "", errors.New("prior worker journal inventory unavailable")
	}
	files := map[string][]byte{}
	sequences := map[uint64]bool{}
	var total int64
	for _, entry := range entries {
		if entry.Name() == "control.sock" {
			info, err := os.Lstat(filepath.Join(directory, entry.Name()))
			if err != nil || info.Mode()&os.ModeSocket == 0 || info.Mode().Perm()&0077 != 0 {
				return "", errors.New("prior worker socket unsafe")
			}
			continue
		}
		name := entry.Name()
		base := strings.TrimSuffix(strings.TrimSuffix(name, ".admission"), ".cursor")
		sequence, err := strconv.ParseUint(strings.TrimSuffix(base, ".jsonl"), 10, 64)
		if err != nil || sequence == 0 || sequence == ^uint64(0) || base != strconv.FormatUint(sequence, 10)+".jsonl" ||
			(name != base && name != base+".admission" && name != base+".cursor") {
			return "", errors.New("prior worker journal inventory invalid")
		}
		limit := int64(workerJournalBytes) - total
		if strings.HasSuffix(name, ".admission") && limit > workerRequestBytes {
			limit = workerRequestBytes
		}
		if strings.HasSuffix(name, ".cursor") && limit > 32 {
			limit = 32
		}
		data, err := readWorkerJournalFile(filepath.Join(directory, name), limit)
		if err != nil {
			return "", err
		}
		total += int64(len(data))
		files[name], sequences[sequence] = data, true
	}
	if len(sequences) > workerMaxExecutions {
		return "", errors.New("prior worker journal capacity exceeded")
	}
	for name, data := range files {
		if err := fsx.AtomicWriteFile(filepath.Join(h.directory, name), data, 0600); err != nil {
			return "", errors.New("copy prior worker journal failed")
		}
		if !strings.HasSuffix(name, ".cursor") {
			h.bytes += int64(len(data))
		}
	}
	for sequence := range sequences {
		name := strconv.FormatUint(sequence, 10) + ".jsonl"
		journal, ok := files[name]
		admission, admitted := files[name+".admission"]
		if !ok || !admitted {
			return "", errors.New("prior worker journal checkpoint missing")
		}
		var saved struct {
			Execution WorkerExecution
			Digest    [32]byte
		}
		decoder := json.NewDecoder(bytes.NewReader(admission))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&saved) != nil || decoder.Decode(new(any)) != io.EOF || saved.Execution.Sequence != sequence ||
			len(saved.Execution.ReceiptWorkerID) < 32 || len(saved.Execution.ReceiptWorkerID) > 128 || saved.Execution.Key == "" || len(saved.Execution.Key) > 4096 {
			return "", errors.New("prior worker admission invalid")
		}
		execution := &workerExecution{WorkerExecution: saved.Execution, digest: saved.Digest, admissionBytes: int64(len(admission)), bytes: int64(len(journal))}
		scanner := bufio.NewScanner(bytes.NewReader(journal))
		scanner.Buffer(make([]byte, 4096), workerEventBytes+1)
		terminal, offset := false, int64(0)
		for scanner.Scan() {
			var event WorkerEvent
			decoder := json.NewDecoder(bytes.NewReader(scanner.Bytes()))
			decoder.DisallowUnknownFields()
			if terminal || len(scanner.Bytes()) > workerEventBytes || len(execution.offsets) >= workerMaxEvents || decoder.Decode(&event) != nil || decoder.Decode(new(any)) != io.EOF || event.Cursor != uint64(len(execution.offsets))+1 {
				return "", errors.New("prior worker event journal invalid")
			}
			execution.offsets = append(execution.offsets, offset)
			offset += int64(len(scanner.Bytes()) + 1)
			terminal = event.Event.Done && !event.Event.Continues && !event.RequestOnly
			if terminal && event.Event.Kind != core.EventFinal && event.Event.Kind != core.EventError && event.Event.Kind != core.EventStatus {
				return "", errors.New("prior worker terminal event invalid")
			}
			if event.Conversation != nil {
				if event.Conversation.Validate() != nil || execution.Conversation == nil || event.Conversation.ExecutionID != execution.Conversation.ExecutionID || event.Conversation.Channel != execution.Conversation.Channel || event.Conversation.Conversation != execution.Conversation.Conversation {
					return "", errors.New("prior worker conversation identity invalid")
				}
				execution.Conversation = event.Conversation
			}
			if event.ConversationTurns > execution.ConversationTurns {
				execution.ConversationTurns = event.ConversationTurns
			}
			if terminal {
				execution.Error = event.TerminalError
			}
		}
		if scanner.Err() != nil || offset != int64(len(journal)) || execution.LastCursor > uint64(len(execution.offsets)) {
			return "", errors.New("prior worker journal truncated")
		}
		execution.LastCursor = uint64(len(execution.offsets))
		if data, ok := files[name+".cursor"]; ok {
			cursor, err := strconv.ParseUint(strings.TrimSuffix(string(data), "\n"), 10, 64)
			if err != nil || string(data) != strconv.FormatUint(cursor, 10)+"\n" || cursor > execution.LastCursor || cursor < execution.CommittedCursor {
				return "", errors.New("prior worker committed cursor invalid")
			}
			execution.CommittedCursor = cursor
		} else if execution.CommittedCursor != 0 {
			return "", errors.New("prior worker committed cursor missing")
		}
		if sequence >= h.next {
			h.next = sequence + 1
		}
		if !terminal && !(execution.Done && execution.LastCursor == 0 && execution.Error != "") {
			// No terminal proves completion. Keep the raw prefix within the same
			// capacity bound; ordinary durable source/claim recovery owns reconciliation.
			h.incomplete++
			continue
		}
		execution.Done = true
		file, err := os.OpenFile(filepath.Join(h.directory, name), os.O_RDWR, 0600)
		if err != nil {
			return "", errors.New("open copied worker journal failed")
		}
		execution.file = file
		h.executions[sequence] = execution
		if err := h.saveAdmissionLocked(execution); err != nil {
			return "", err
		}
	}
	return directory, nil
}

func readWorkerJournalFile(path string, limit int64) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > limit {
		return nil, errors.New("prior worker journal file unsafe or oversized")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, errors.New("prior worker journal file unavailable")
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return nil, errors.New("prior worker journal file changed")
	}
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil || int64(len(data)) > limit || !utf8.Valid(data) {
		return nil, errors.New("prior worker journal file unavailable or oversized")
	}
	return data, nil
}
