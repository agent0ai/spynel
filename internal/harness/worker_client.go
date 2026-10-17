package harness

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"github.com/agent0ai/spynel/internal/fsx"
	"github.com/agent0ai/spynel/internal/instance"
	"github.com/agent0ai/spynel/internal/updater"
)

type workerRecord struct {
	Info  WorkerInfo
	Token string
}

// WorkerClient is a connection to one verified worker, not an execution owner.
// Call never retries admissions; a caller must retain its exact sequence/payload.
type WorkerClient struct {
	record     workerRecord
	instanceID string
	termToken  string
	client     *http.Client
}

// OpenWorker discovers a live worker before considering a new launch. Unknown
// identity, inaccessible endpoints and incompatible config fail closed; only a
// missing record or confirmed dead process permits a fresh worker. It must not
// be called from inside an election ownership callback.
func OpenWorker(ctx context.Context, cfg HarnessConfig, instanceID, termToken string) (*WorkerClient, error) {
	root, err := filepath.EvalSymlinks(cfg.Cwd)
	if err != nil || !filepath.IsAbs(root) {
		return nil, errors.New("worker requires an absolute existing workspace")
	}
	cfg.Cwd = root
	lockPath, err := ProcessOwnershipPath(root)
	if err != nil {
		return nil, err
	}
	path, err := workerRecordPath(lockPath)
	if err != nil {
		return nil, err
	}
	election, err := instance.New(filepath.Join(root, ".spynel"))
	if err != nil {
		return nil, err
	}
	owned, err := election.RunForOwner(instanceID, termToken, func() error { return ctx.Err() })
	if err != nil || !owned {
		return nil, errors.New("worker controller ownership unavailable")
	}
	discover := func(ctx context.Context) (*WorkerClient, error) {
		record, err := readWorkerRecord(path)
		if err != nil {
			return nil, err
		}
		if err := updater.VerifyProcess(record.Info.Process); err != nil {
			if errors.Is(err, syscall.ESRCH) {
				return nil, syscall.ESRCH
			}
			return nil, errors.New("worker native identity unavailable")
		}
		// shortcut: startup settings must match exactly, replace when worker-owned live commits are connected.
		if record.Info.EnvironmentID != election.EnvironmentID() || record.Info.WorkspaceID != filepath.Base(lockPath) || record.Info.ConfigDigest != workerConfigDigest(cfg) {
			return nil, errors.New("worker environment, workspace or configuration changed")
		}
		client := newWorkerClient(record, instanceID, termToken)
		if _, err := client.Call(ctx, WorkerRequest{Action: "inspect"}); err != nil {
			client.Close()
			return nil, err
		}
		return client, nil
	}
	client, err := discover(ctx)
	if err == nil {
		return client, nil
	}
	if !errors.Is(err, os.ErrNotExist) && !errors.Is(err, syscall.ESRCH) {
		return nil, err
	}
	token, err := election.NewToken()
	if err != nil {
		return nil, err
	}
	data, err := json.Marshal(WorkerBootstrap{Protocol: WorkerProtocol, Token: token, InstanceID: instanceID, TermToken: termToken, Harness: cfg})
	if err != nil || len(data) > workerRequestBytes {
		return nil, errors.New("invalid worker bootstrap size")
	}
	executable, err := os.Executable()
	if err != nil {
		return nil, err
	}
	command := exec.Command(executable, "__worker")
	command.Dir, command.Stdin = root, bytes.NewReader(data)
	// No inherited controller stream can become a worker lifetime pipe/SIGPIPE.
	if err := detachWorker(command); err != nil {
		return nil, err
	}
	if err := command.Start(); err != nil {
		return nil, errors.New("worker launch failed")
	}
	exited := make(chan struct{})
	go func() { _ = command.Wait(); close(exited) }()
	startup, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		client, err = discover(startup)
		if err == nil {
			return client, nil
		}
		if !errors.Is(err, os.ErrNotExist) && !errors.Is(err, syscall.ESRCH) {
			return nil, err
		}
		select {
		case <-startup.Done():
			// The worker may already be discoverable by another controller. Its
			// own authenticated detached grace owns cleanup, never this caller.
			return nil, errors.New("worker startup did not become available before the deadline")
		case <-exited:
			// A concurrent opener may have won the shared harness lock.
			if client, err := discover(startup); err == nil {
				return client, nil
			}
			return nil, errors.New("worker startup failed; prior provider cleanup may still hold ownership")
		case <-ticker.C:
		}
	}
}

func newWorkerClient(record workerRecord, instanceID, termToken string) *WorkerClient {
	return &WorkerClient{record: record, instanceID: instanceID, termToken: termToken, client: &http.Client{
		Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			if err := validateWorkerSocket(record.Info.Socket); err != nil {
				return nil, err
			}
			return (&net.Dialer{}).DialContext(ctx, "unix", record.Info.Socket)
		}},
		Timeout:       30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

func (c *WorkerClient) Info() WorkerInfo { return c.record.Info }

// Close detaches only. Destructive shutdown is an explicit fenced operation.
func (c *WorkerClient) Close() { c.client.CloseIdleConnections() }

func (c *WorkerClient) Call(ctx context.Context, request WorkerRequest) (WorkerReply, error) {
	if err := updater.VerifyProcess(c.record.Info.Process); err != nil {
		return WorkerReply{}, err
	}
	request.WorkerID, request.InstanceID, request.TermToken = c.record.Info.ID, c.instanceID, c.termToken
	data, err := json.Marshal(request)
	if err != nil || len(data) > workerRequestBytes {
		return WorkerReply{}, errors.New("invalid worker request size")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://worker/worker", bytes.NewReader(data))
	if err != nil {
		return WorkerReply{}, errors.New("invalid worker request")
	}
	req.Header.Set("Authorization", "Bearer "+c.record.Token)
	response, err := c.client.Do(req)
	if err != nil {
		return WorkerReply{}, errors.New("verified worker connection unavailable; execution status is unknown")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return WorkerReply{}, errors.New("worker ownership, identity or journal unavailable")
	}
	const maxReply = workerEventBytes + workerPageBytes
	data, err = io.ReadAll(io.LimitReader(response.Body, maxReply+1))
	if err != nil || len(data) > maxReply {
		return WorkerReply{}, errors.New("invalid worker response size")
	}
	var reply WorkerReply
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&reply); err != nil {
		return WorkerReply{}, errors.New("invalid worker response")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return WorkerReply{}, errors.New("invalid worker response")
	}
	if reply.Error != "" {
		return reply, errors.New(reply.Error)
	}
	return reply, nil
}

func workerConfigDigest(cfg HarnessConfig) [32]byte {
	data, _ := json.Marshal(cfg)
	return sha256.Sum256(data)
}

func workerRecordPath(lockPath string) (string, error) {
	for _, directory := range []string{filepath.Dir(filepath.Dir(lockPath)), filepath.Dir(lockPath)} {
		if err := os.MkdirAll(directory, 0700); err != nil {
			return "", err
		}
		info, err := os.Lstat(directory)
		if err != nil || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
			return "", errors.New("worker directory must be private and not a symlink")
		}
	}
	return lockPath + ".worker.json", nil
}

func readWorkerRecord(path string) (workerRecord, error) {
	var record workerRecord
	info, err := os.Lstat(path)
	if err != nil {
		return record, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > 8192 {
		return record, errors.New("worker record must be a bounded private regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return record, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return record, errors.New("worker record changed while opening")
	}
	data, err := io.ReadAll(io.LimitReader(file, 8193))
	if err != nil || len(data) > 8192 {
		return record, errors.New("invalid worker record size")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&record); err != nil {
		return record, errors.New("invalid worker record")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return record, errors.New("invalid worker record")
	}
	native := record.Info.Process
	if record.Info.Protocol != WorkerProtocol || len(record.Info.ID) < 32 || len(record.Token) < 32 || len(record.Token) > 128 ||
		!filepath.IsAbs(record.Info.Socket) || record.Info.PID <= 1 || native.PID != record.Info.PID || !native.HarnessWorker || native.Stopping ||
		len(native.Generation) != 32 || native.StartTime == "" || !filepath.IsAbs(native.Executable) {
		return record, errors.New("invalid worker identity")
	}
	return record, nil
}

func validateWorkerSocket(path string) error {
	info, err := os.Lstat(filepath.Dir(path))
	if err != nil || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return errors.New("worker endpoint directory is not private")
	}
	info, err = os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSocket == 0 || info.Mode().Perm()&0077 != 0 {
		return errors.New("worker endpoint is not a private socket")
	}
	return nil
}

func publishWorkerRecord(path string, record workerRecord) error {
	prior, err := readWorkerRecord(path)
	if err == nil {
		if err := updater.VerifyProcess(prior.Info.Process); !errors.Is(err, syscall.ESRCH) {
			return errors.New("prior worker identity is still live or unverified")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	data, err := json.Marshal(record)
	if err != nil || len(data) > 8192 {
		return errors.New("invalid worker record size")
	}
	return fsx.AtomicWriteFile(path, data, 0600)
}
