// Package instance coordinates the single workspace server owner shared by
// every Spynel process using the same state directory.
package instance

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/agent0ai/spynel/internal/fsx"
)

const (
	HeartbeatInterval = 5 * time.Second
	StaleAfter        = 30 * time.Second
	RetryInterval     = time.Second
	HandoffTimeout    = 10 * time.Second
)

// Lease is the atomically published rendezvous record for the current
// workspace server. Token authenticates loopback clients and also fences a
// former owner whose process-local instance ID somehow survives a restart.
type Lease struct {
	InstanceID  string    `json:"instance_id"`
	PID         int       `json:"pid"`
	Endpoint    string    `json:"endpoint"`
	Token       string    `json:"token"`
	StartedAt   time.Time `json:"started_at"`
	HeartbeatAt time.Time `json:"heartbeat_at"`
	HandoffTo   string    `json:"handoff_to,omitempty"`
	HandoffAt   time.Time `json:"handoff_at,omitempty"`
}

// Election serializes lease compare-and-replace operations with a short-held
// operating-system file lock. The lease itself is never held open, so another
// process can replace a stale owner even when that owner is alive but stalled.
type Election struct {
	leasePath string
	lockPath  string
	id        string
	pid       int
	now       func() time.Time
}

func New(stateDirectory string) (*Election, error) {
	id, err := randomHex(16)
	if err != nil {
		return nil, fmt.Errorf("create instance ID: %w", err)
	}
	runtimeDirectory := filepath.Join(stateDirectory, "runtime")
	return &Election{
		leasePath: filepath.Join(runtimeDirectory, "primary.json"),
		lockPath:  filepath.Join(runtimeDirectory, "primary.lock"),
		id:        id,
		pid:       os.Getpid(),
		now:       func() time.Time { return time.Now().UTC() },
	}, nil
}

func (e *Election) ID() string { return e.id }

// NewToken returns a fresh secret for one ownership term. A process that loses
// and later regains ownership uses a different token and endpoint.
func (e *Election) NewToken() (string, error) {
	data := make([]byte, 32)
	if _, err := rand.Read(data); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(data), nil
}

// TryAcquire publishes this process as owner only when there is no healthy
// owner. The cross-process mutex makes the stale check and replacement one
// indivisible decision among all contenders.
func (e *Election) TryAcquire(endpoint, token string) (Lease, bool, error) {
	if endpoint == "" || token == "" {
		return Lease{}, false, errors.New("instance endpoint and token are required")
	}
	var result Lease
	var acquired bool
	err := e.withLock(func() error {
		now := e.now()
		current, err := e.readLease()
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			// Atomic replacement prevents partial records. A malformed record cannot
			// represent a renewable owner, so recover it under the election lock.
			current = Lease{}
		}
		if current.InstanceID != "" && !stale(current, now) {
			targeted := current.HandoffTo == e.id
			expired := current.HandoffTo != "" && !now.Before(current.HandoffAt.Add(HandoffTimeout))
			if !targeted && !expired {
				result = current
				return nil
			}
		}
		result = Lease{
			InstanceID: e.id, PID: e.pid, Endpoint: endpoint, Token: token,
			StartedAt: now, HeartbeatAt: now,
		}
		if err := e.writeLease(result); err != nil {
			return err
		}
		acquired = true
		return nil
	})
	return result, acquired, err
}

// Renew refreshes an ownership term only if both its process identity and
// secret token are still current. A resumed stale process therefore observes
// the winner of a takeover instead of overwriting it.
func (e *Election) Renew(token string) (Lease, bool, error) {
	var result Lease
	var owned bool
	err := e.withLock(func() error {
		current, err := e.readLease()
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			return err
		}
		result = current
		if current.InstanceID != e.id || current.Token != token || current.HandoffTo != "" {
			return nil
		}
		now := e.now()
		if stale(current, now) {
			return nil
		}
		current.HeartbeatAt = now
		if err := e.writeLease(current); err != nil {
			return err
		}
		result = current
		owned = true
		return nil
	})
	return result, owned, err
}

// Handoff fences the current ownership term and reserves immediate takeover
// for one known contender. The reservation expires so a vanished target cannot
// strand the workspace. Callers must stop all owner-only work before invoking
// this method.
func (e *Election) Handoff(token, targetID string) (Lease, bool, error) {
	if targetID == "" || targetID == e.id {
		return Lease{}, false, errors.New("handoff requires a different target instance")
	}
	var result Lease
	var handedOff bool
	err := e.withLock(func() error {
		current, err := e.readLease()
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			return err
		}
		result = current
		if current.InstanceID != e.id || current.Token != token || current.HandoffTo != "" {
			return nil
		}
		now := e.now()
		if stale(current, now) {
			return nil
		}
		current.HeartbeatAt = now
		current.HandoffTo = targetID
		current.HandoffAt = now
		if err := e.writeLease(current); err != nil {
			return err
		}
		result = current
		handedOff = true
		return nil
	})
	return result, handedOff, err
}

// Release removes only this exact ownership term. It is safe to call after a
// takeover because it cannot delete the successor's record.
func (e *Election) Release(token string) error {
	return e.withLock(func() error {
		current, err := e.readLease()
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if current.InstanceID != e.id || current.Token != token {
			return nil
		}
		if err := os.Remove(e.leasePath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	})
}

func (e *Election) Current() (Lease, error) {
	// Lease writes use atomic replacement, so readers always observe either the
	// previous complete record or the next complete record. Discovery therefore
	// does not need the mutation lock. In particular, a secondary can connect to
	// a healthy owner's API during startup while another contender is briefly
	// serializing an acquire, renewal, release, or handoff.
	return e.readLease()
}

func (e *Election) IsStale(lease Lease) bool { return stale(lease, e.now()) }

// CanTakeOver reports whether this contender should attempt a serialized
// acquisition now. Targeted handoffs bypass the ordinary stale wait; other
// contenders remain excluded until the reservation expires.
func (e *Election) CanTakeOver(lease Lease) bool {
	now := e.now()
	return stale(lease, now) || lease.HandoffTo == e.id ||
		(lease.HandoffTo != "" && !now.Before(lease.HandoffAt.Add(HandoffTimeout)))
}

func stale(lease Lease, now time.Time) bool {
	return lease.InstanceID == "" || lease.HeartbeatAt.IsZero() || !now.Before(lease.HeartbeatAt.Add(StaleAfter))
}

func (e *Election) withLock(action func() error) error {
	if err := os.MkdirAll(filepath.Dir(e.lockPath), 0o700); err != nil {
		return err
	}
	file, err := os.OpenFile(e.lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer file.Close()
	if err := lockFile(file); err != nil {
		return err
	}
	defer unlockFile(file)
	return action()
}

func (e *Election) readLease() (Lease, error) {
	data, err := os.ReadFile(e.leasePath)
	if err != nil {
		return Lease{}, err
	}
	var lease Lease
	if err := json.Unmarshal(data, &lease); err != nil {
		return Lease{}, fmt.Errorf("decode primary lease: %w", err)
	}
	if lease.InstanceID == "" || lease.Endpoint == "" || lease.Token == "" || lease.HeartbeatAt.IsZero() {
		return Lease{}, errors.New("primary lease is incomplete")
	}
	if (lease.HandoffTo == "") != lease.HandoffAt.IsZero() {
		return Lease{}, errors.New("primary lease handoff is incomplete")
	}
	return lease, nil
}

func (e *Election) writeLease(lease Lease) error {
	data, err := json.MarshalIndent(lease, "", "  ")
	if err != nil {
		return err
	}
	return fsx.AtomicWriteFile(e.leasePath, append(data, '\n'), 0o600)
}

func randomHex(size int) (string, error) {
	data := make([]byte, size)
	if _, err := rand.Read(data); err != nil {
		return "", err
	}
	return hex.EncodeToString(data), nil
}
