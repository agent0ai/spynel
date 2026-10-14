package harness

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"sync"
	"time"

	"github.com/agent0ai/spynel/internal/fsx"
)

// A dead executor cannot prove its providers or their external effects ended.
// Never age out this marker: only the owning orderly shutdown may release it.
type processOwnership struct {
	path, identity string
}

func (o *processOwnership) acquire() error {
	if o.path == "" || o.identity != "" {
		return nil
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return err
	}
	identity := hex.EncodeToString(nonce[:]) + "\n"
	if err := fsx.AtomicCreateFile(o.path, []byte(identity), 0600); err != nil {
		return errors.New("harness ownership unavailable: a previous provider may still be running; inspect and stop prior work before clearing the private harness-owner marker")
	}
	o.identity = identity
	return nil
}

func (o *processOwnership) release() error {
	if o.identity == "" {
		return nil
	}
	info, err := os.Lstat(o.path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() != int64(len(o.identity)) {
		return errors.New("harness ownership changed during shutdown")
	}
	data, err := os.ReadFile(o.path)
	if err != nil {
		return err
	}
	if string(data) != o.identity {
		return errors.New("harness ownership changed during shutdown")
	}
	return os.Remove(o.path)
}

func waitProviderProcesses(group *sync.WaitGroup) error {
	done := make(chan struct{})
	go func() { group.Wait(); close(done) }()
	return waitProviderProcess(done)
}

func waitProviderProcess(done <-chan struct{}) error {
	if done == nil {
		return nil
	}
	select {
	case <-done:
		return nil
	case <-time.After(8 * time.Second):
		return errors.New("provider shutdown was not confirmed; recovery remains fenced")
	}
}
