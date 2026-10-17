package harness

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/agent0ai/spynel/internal/core"
	"github.com/agent0ai/spynel/internal/instance"
)

const (
	WorkerProtocol      = 1
	workerDetachedGrace = 10 * time.Minute
	workerRequestBytes  = 256 * 1024
	workerEventBytes    = 32 * 1024 * 1024
	workerJournalBytes  = 128 * 1024 * 1024
	workerMaxEvents     = 65536
	workerMaxExecutions = 256
	workerPageBytes     = 2 * 1024 * 1024
	workerPageEvents    = 256
)

// WorkerBootstrap travels over a private inherited pipe, never arguments or
// environment. The worker owns the complete supervisor and all adapter state.
type WorkerBootstrap struct {
	Protocol   int
	Token      string
	InstanceID string
	TermToken  string
	Harness    HarnessConfig
}

type WorkerInfo struct {
	Protocol int
	ID       string
	Socket   string
	PID      int
}

// Sequence is a strictly increasing admission number, not a provider turn.
// A repeated or retired sequence can never call Send again.
type WorkerRequest struct {
	WorkerID   string
	InstanceID string
	TermToken  string
	Action     string
	Sequence   uint64
	Key        string
	Prompt     string
	Message    string
	After      uint64
}

type WorkerEvent struct {
	Cursor uint64
	Event  core.Event
}

type WorkerExecution struct {
	Sequence uint64
	Key      string
	ThreadID string
	Steered  bool
	Done     bool
	Error    string
}

type WorkerReply struct {
	Error        string
	NextSequence uint64
	Executions   []WorkerExecution
	Events       []WorkerEvent
	Interrupted  bool
}

type workerExecution struct {
	WorkerExecution
	file    *os.File
	offsets []int64
	bytes   int64
	digest  [32]byte
}

type workerHost struct {
	supervisor *Supervisor
	election   *instance.Election
	info       WorkerInfo
	token      string
	directory  string
	stop       chan struct{}
	stopOnce   sync.Once
	operations sync.Mutex
	mu         sync.Mutex
	lastSeen   time.Time
	next       uint64
	executions map[uint64]*workerExecution
	bytes      int64
	failure    error
}

// RunWorker is a private same-binary entry point. Losing its bootstrap pipe,
// HTTP clients or elected controller does not cancel its provider context.
// Explicit shutdown, signal, journal failure or bounded detachment still do.
func RunWorker(ctx context.Context, input io.Reader, ready io.Writer, registry *Registry) (runErr error) {
	var bootstrap WorkerBootstrap
	data, err := io.ReadAll(io.LimitReader(input, workerRequestBytes+1))
	if err != nil || len(data) > workerRequestBytes {
		return errors.New("invalid worker bootstrap size")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&bootstrap); err != nil {
		return errors.New("invalid worker bootstrap")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return errors.New("invalid worker bootstrap")
	}
	if bootstrap.Protocol != WorkerProtocol || len(bootstrap.Token) < 32 || len(bootstrap.Token) > 128 {
		return errors.New("invalid worker protocol or credential")
	}
	root, err := filepath.EvalSymlinks(bootstrap.Harness.Cwd)
	if err != nil || !filepath.IsAbs(root) {
		return errors.New("worker requires an absolute existing workspace")
	}
	bootstrap.Harness.Cwd = root
	lockPath, err := ProcessOwnershipPath(root)
	if err != nil {
		return err
	}
	for _, directory := range []string{filepath.Dir(filepath.Dir(lockPath)), filepath.Dir(lockPath)} {
		if err := os.MkdirAll(directory, 0700); err != nil {
			return err
		}
		info, err := os.Lstat(directory)
		if err != nil || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
			return errors.New("worker directory must be private and not a symlink")
		}
	}
	// Unix socket names have a small platform limit; workspace/config paths do not.
	directory, err := os.MkdirTemp("", "spynel-worker-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(directory)
	election, err := instance.New(filepath.Join(root, ".spynel"))
	if err != nil {
		return err
	}
	id, err := election.NewToken()
	if err != nil {
		return err
	}
	info := WorkerInfo{Protocol: WorkerProtocol, ID: id, Socket: filepath.Join(directory, "control.sock"), PID: os.Getpid()}
	listener, err := net.Listen("unix", info.Socket)
	if err != nil {
		return err
	}
	defer listener.Close()
	if err := os.Chmod(info.Socket, 0600); err != nil {
		return err
	}
	workerCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	supervisor := NewSupervisor(registry, bootstrap.Harness)
	supervisor.SetProcessStartGuard(lockPath, nil)
	var host *workerHost
	defer func() {
		cancel()
		runErr = errors.Join(runErr, supervisor.Close())
		if host != nil {
			host.mu.Lock()
			defer host.mu.Unlock()
			for _, execution := range host.executions {
				_ = execution.file.Close()
			}
		}
	}()
	ran, err := election.RunForOwner(bootstrap.InstanceID, bootstrap.TermToken, func() error { return supervisor.Start(workerCtx) })
	if err != nil || !ran {
		return errors.New("worker startup ownership unavailable")
	}
	host = &workerHost{
		supervisor: supervisor, election: election, info: info, token: bootstrap.Token, directory: directory,
		stop: make(chan struct{}), lastSeen: time.Now(), next: 1, executions: map[uint64]*workerExecution{},
	}
	server := &http.Server{
		Handler: http.HandlerFunc(host.serve), ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout: 30 * time.Second, WriteTimeout: 30 * time.Second, MaxHeaderBytes: 4096,
	}
	served := make(chan error, 1)
	go func() { served <- server.Serve(listener) }()
	defer func() {
		cancel()
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutdownCancel()
		_ = server.Shutdown(shutdownCtx)
		_ = server.Close()
	}()
	if err := json.NewEncoder(ready).Encode(info); err != nil {
		return errors.New("worker readiness pipe unavailable")
	}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-host.stop:
			host.mu.Lock()
			err := host.failure
			host.mu.Unlock()
			return err
		case err := <-served:
			return err
		case <-ticker.C:
			host.mu.Lock()
			expired := time.Since(host.lastSeen) >= workerDetachedGrace
			host.mu.Unlock()
			if expired {
				return errors.New("worker detached grace expired")
			}
		}
	}
}

func (h *workerHost) serve(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.URL.Path != "/worker" {
		http.Error(w, "invalid worker request", http.StatusNotFound)
		return
	}
	if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+h.token)) != 1 {
		http.Error(w, "worker authorization denied", http.StatusUnauthorized)
		return
	}
	var request WorkerRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, workerRequestBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		http.Error(w, "invalid worker request", http.StatusBadRequest)
		return
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		http.Error(w, "invalid worker request", http.StatusBadRequest)
		return
	}
	if request.WorkerID != h.info.ID {
		http.Error(w, "worker identity changed", http.StatusConflict)
		return
	}
	h.operations.Lock()
	defer h.operations.Unlock()
	var reply WorkerReply
	ran, err := h.election.RunForOwner(request.InstanceID, request.TermToken, func() error {
		h.mu.Lock()
		if h.failure != nil {
			h.mu.Unlock()
			return errors.New("worker event journal unavailable")
		}
		h.lastSeen = time.Now()
		h.mu.Unlock()
		reply = h.apply(request)
		return nil
	})
	if err != nil || !ran {
		http.Error(w, "worker controller ownership unavailable", http.StatusForbidden)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(reply)
}

func (h *workerHost) apply(request WorkerRequest) WorkerReply {
	switch request.Action {
	case "inspect":
		h.mu.Lock()
		defer h.mu.Unlock()
		reply := WorkerReply{NextSequence: h.next}
		for _, execution := range h.executions {
			reply.Executions = append(reply.Executions, execution.WorkerExecution)
		}
		sort.Slice(reply.Executions, func(i, j int) bool { return reply.Executions[i].Sequence < reply.Executions[j].Sequence })
		return reply
	case "send":
		return h.send(request)
	case "events":
		return h.events(request)
	case "ack":
		h.mu.Lock()
		defer h.mu.Unlock()
		execution := h.executions[request.Sequence]
		if execution == nil {
			if request.Sequence > 0 && request.Sequence < h.next {
				return WorkerReply{}
			}
			return WorkerReply{Error: "unknown worker execution"}
		}
		if !execution.Done || request.After != uint64(len(execution.offsets)) {
			return WorkerReply{Error: "worker execution is not fully consumed"}
		}
		if err := os.Remove(execution.file.Name()); err != nil {
			return WorkerReply{Error: "retire worker journal failed"}
		}
		_ = execution.file.Close()
		h.bytes -= execution.bytes
		delete(h.executions, request.Sequence)
		return WorkerReply{}
	case "interrupt":
		interrupted, err := h.supervisor.Interrupt(context.Background(), request.Key)
		if err != nil {
			return WorkerReply{Error: err.Error()}
		}
		return WorkerReply{Interrupted: interrupted}
	case "shutdown":
		h.stopOnce.Do(func() { close(h.stop) })
		return WorkerReply{}
	default:
		return WorkerReply{Error: "unsupported worker action"}
	}
}

func (h *workerHost) send(request WorkerRequest) WorkerReply {
	data, _ := json.Marshal([3]string{request.Key, request.Prompt, request.Message})
	digest := sha256.Sum256(data)
	h.mu.Lock()
	if execution := h.executions[request.Sequence]; execution != nil {
		if execution.digest != digest {
			h.mu.Unlock()
			return WorkerReply{Error: "worker admission payload changed"}
		}
		reply := WorkerReply{Executions: []WorkerExecution{execution.WorkerExecution}}
		h.mu.Unlock()
		return reply
	}
	if request.Sequence != h.next || request.Key == "" || request.Prompt == "" || len(request.Key) > 4096 {
		h.mu.Unlock()
		return WorkerReply{Error: "invalid or retired worker admission"}
	}
	if len(h.executions) >= workerMaxExecutions {
		h.mu.Unlock()
		return WorkerReply{Error: "worker execution retention is full; consume completed executions"}
	}
	file, err := os.OpenFile(filepath.Join(h.directory, strconv.FormatUint(request.Sequence, 10)+".jsonl"), os.O_RDWR|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		h.mu.Unlock()
		return WorkerReply{Error: "create worker journal failed"}
	}
	execution := &workerExecution{WorkerExecution: WorkerExecution{Sequence: request.Sequence, Key: request.Key}, file: file, digest: digest}
	h.executions[request.Sequence] = execution
	h.next++
	h.mu.Unlock()
	// The worker lifetime, not a controller request, owns every admitted turn.
	threadID, steered, err := h.supervisor.SendConversation(h.supervisor.ctx, request.Key, request.Prompt, request.Message, func(event core.Event) {
		h.capture(execution, event)
	})
	h.mu.Lock()
	execution.ThreadID, execution.Steered = threadID, steered
	if err != nil {
		execution.Error, execution.Done = err.Error(), true
	}
	reply := WorkerReply{Executions: []WorkerExecution{execution.WorkerExecution}}
	h.mu.Unlock()
	return reply
}

func (h *workerHost) capture(execution *workerExecution, event core.Event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.failure != nil || execution.Done {
		return
	}
	data, err := json.Marshal(WorkerEvent{Cursor: uint64(len(execution.offsets)) + 1, Event: event})
	if err == nil && (len(data) > workerEventBytes || h.bytes+int64(len(data)+1) > workerJournalBytes || len(execution.offsets) >= workerMaxEvents) {
		err = errors.New("worker event journal capacity exceeded")
	}
	if err == nil {
		data = append(data, '\n')
		var written int
		written, err = execution.file.Write(data)
		if err == nil && written != len(data) {
			err = io.ErrShortWrite
		}
		if err == nil {
			err = execution.file.Sync()
		}
	}
	if err != nil {
		// Unrecorded events cannot become successful delivery or a new turn.
		h.failure = errors.New("worker event journal unavailable")
		h.stopOnce.Do(func() { close(h.stop) })
		return
	}
	execution.offsets = append(execution.offsets, execution.bytes)
	execution.bytes += int64(len(data))
	h.bytes += int64(len(data))
	if event.Done && !event.Continues {
		execution.Done = true
	}
}

func (h *workerHost) events(request WorkerRequest) WorkerReply {
	h.mu.Lock()
	defer h.mu.Unlock()
	execution := h.executions[request.Sequence]
	if execution == nil || request.After > uint64(len(execution.offsets)) {
		return WorkerReply{Error: "unknown worker execution or cursor"}
	}
	reply := WorkerReply{Executions: []WorkerExecution{execution.WorkerExecution}}
	var bytes int64
	for index := int(request.After); index < len(execution.offsets) && len(reply.Events) < workerPageEvents; index++ {
		start, end := execution.offsets[index], execution.bytes
		if index+1 < len(execution.offsets) {
			end = execution.offsets[index+1]
		}
		if len(reply.Events) > 0 && bytes+end-start > workerPageBytes {
			break
		}
		data := make([]byte, end-start)
		if _, err := execution.file.ReadAt(data, start); err != nil {
			return WorkerReply{Error: "read worker journal failed"}
		}
		var event WorkerEvent
		if err := json.Unmarshal(data, &event); err != nil || event.Cursor != uint64(index+1) {
			return WorkerReply{Error: "invalid worker journal record"}
		}
		reply.Events = append(reply.Events, event)
		bytes += end - start
	}
	return reply
}
