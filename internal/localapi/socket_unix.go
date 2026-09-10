//go:build !windows

package localapi

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/agent0ai/spynel/internal/fsx"
)

type socketDescriptor struct {
	Schema      string `json:"schema"`
	WorkspaceID string `json:"workspace_id"`
	Token       string `json:"token"`
}

type privateSocket struct {
	net.Listener
	path       string
	socketInfo os.FileInfo
	authInfo   os.FileInfo
}

func (l *privateSocket) Close() error {
	err := l.Listener.Close()
	removeOwnedFile(l.path, l.socketInfo)
	removeOwnedFile(l.path+".json", l.authInfo)
	return err
}

func removeOwnedFile(path string, owned os.FileInfo) {
	if current, err := os.Lstat(path); err == nil && owned != nil && os.SameFile(current, owned) {
		_ = os.Remove(path)
	}
}

func validateSocketDirectory(path string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || len(path) > 100 {
		return errors.New("socket requires a clean absolute path of at most 100 bytes")
	}
	directory := filepath.Dir(path)
	canonical, err := filepath.EvalSymlinks(directory)
	if err != nil || canonical != directory {
		return errors.New("socket directory must exist without symlinks")
	}
	info, err := os.Lstat(directory)
	if err != nil {
		return err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.IsDir() || info.Mode().Perm() != 0o700 || stat.Uid != uint32(os.Geteuid()) {
		return errors.New("socket directory must be owned by the current user with mode 0700")
	}
	return nil
}

// ListenSocket never unlinks an existing path, including a stale socket. This
// keeps accidental reuse across workspaces and unrelated files fail-closed.
func ListenSocket(path, root, token string) (net.Listener, error) {
	if err := validateSocketDirectory(path); err != nil {
		return nil, err
	}
	if token == "" {
		return nil, errors.New("socket requires authentication")
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		return nil, errors.New("socket path exists or is inaccessible; inspect it before removing stale files")
	}
	if _, err := os.Lstat(path + ".json"); !os.IsNotExist(err) {
		return nil, errors.New("socket descriptor exists or is inaccessible; inspect it before removing stale files")
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, err
	}
	listener.SetUnlinkOnClose(false)
	owned := &privateSocket{Listener: listener, path: path}
	owned.socketInfo, err = os.Lstat(path)
	if err != nil {
		_ = owned.Close()
		return nil, err
	}
	if err = os.Chmod(path, 0o600); err != nil {
		_ = owned.Close()
		return nil, err
	}
	data, err := json.Marshal(socketDescriptor{Schema: "spynel.socket/v1", WorkspaceID: fmt.Sprintf("%x", sha256.Sum256([]byte(root))), Token: token})
	if err == nil {
		err = fsx.AtomicCreateFile(path+".json", append(data, '\n'), 0o600)
	}
	if err != nil {
		_ = owned.Close()
		return nil, err
	}
	owned.authInfo, err = os.Lstat(path + ".json")
	if err != nil {
		_ = owned.Close()
		return nil, err
	}
	return owned, nil
}

func readSocketDescriptor(path string) (socketDescriptor, error) {
	var descriptor socketDescriptor
	if err := validateSocketDirectory(path); err != nil {
		return descriptor, err
	}
	for _, name := range []string{path, path + ".json"} {
		info, err := os.Lstat(name)
		if err != nil {
			return descriptor, errors.New("socket or descriptor unavailable")
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || stat.Uid != uint32(os.Geteuid()) || info.Mode().Perm() != 0o600 || name == path && info.Mode()&os.ModeSocket == 0 || name != path && (!info.Mode().IsRegular() || info.Size() > 4096) {
			return descriptor, errors.New("socket and descriptor require private ownership and correct file types")
		}
	}
	file, err := os.Open(path + ".json")
	if err != nil {
		return descriptor, err
	}
	defer file.Close()
	if err := decodeJSON(file, &descriptor); err != nil {
		return descriptor, errors.New("invalid socket descriptor")
	}
	if descriptor.Schema != "spynel.socket/v1" || len(descriptor.WorkspaceID) != 64 || len(descriptor.Token) < 32 || len(descriptor.Token) > 128 {
		return descriptor, errors.New("invalid socket descriptor")
	}
	return descriptor, nil
}

func NewSocketClient(path string) (*Client, error) {
	descriptor, err := readSocketDescriptor(path)
	if err != nil {
		return nil, err
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{Timeout: 2 * time.Second}).DialContext(ctx, "unix", path)
	}
	return &Client{HTTP: &http.Client{Transport: transport}, socketPath: path, workspaceID: descriptor.WorkspaceID}, nil
}
