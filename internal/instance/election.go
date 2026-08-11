// Package instance coordinates the single workspace server owner shared by
// every Spynel process using the same state directory.
package instance

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/agent0ai/spynel/internal/fsx"
)

const (
	HeartbeatInterval     = 5 * time.Second
	StaleAfter            = 30 * time.Second
	RetryInterval         = time.Second
	HandoffTimeout        = 10 * time.Second
	OwnerlessCleanupGrace = time.Minute
	environmentTokenBytes = 32
)

// Lease is the atomically published rendezvous record for the current
// workspace server. Token authenticates loopback clients and also fences a
// former owner whose process-local instance ID somehow survives a restart.
type Lease struct {
	InstanceID    string    `json:"instance_id"`
	PID           int       `json:"pid"`
	Endpoint      string    `json:"endpoint"`
	EnvironmentID string    `json:"environment_id"`
	Token         string    `json:"token"`
	StartedAt     time.Time `json:"started_at"`
	HeartbeatAt   time.Time `json:"heartbeat_at"`
	HandoffTo     string    `json:"handoff_to,omitempty"`
	HandoffAt     time.Time `json:"handoff_at,omitempty"`
}

// Election serializes lease compare-and-replace operations with a short-held
// operating-system file lock. The lease itself is never held open, so another
// process can replace a stale owner even when that owner is alive but stalled.
type Election struct {
	leasePath        string
	releaseFencePath string
	lockPath         string
	id               string
	pid              int
	now              func() time.Time
	environmentID    string
}

type releaseFence struct {
	ReleasedAt time.Time `json:"released_at"`
}

func New(stateDirectory string) (*Election, error) {
	environmentID, err := EnvironmentID()
	if err != nil {
		return nil, err
	}
	return NewWithEnvironmentID(stateDirectory, environmentID)
}

// NewWithEnvironmentID constructs an election participant with an injected
// connectivity identity. It is primarily useful for deterministic topology
// tests that simulate a shared bind mount across host/container boundaries.
func NewWithEnvironmentID(stateDirectory, environmentID string) (*Election, error) {
	if !validEnvironmentID(environmentID) {
		return nil, errors.New("environment ID must be 64 lowercase hexadecimal characters")
	}
	id, err := randomHex(16)
	if err != nil {
		return nil, fmt.Errorf("create instance ID: %w", err)
	}
	runtimeDirectory := filepath.Join(stateDirectory, "runtime")
	return &Election{
		leasePath:        filepath.Join(runtimeDirectory, "primary.json"),
		releaseFencePath: filepath.Join(runtimeDirectory, "primary-release.json"),
		lockPath:         filepath.Join(runtimeDirectory, "primary.lock"),
		id:               id,
		pid:              os.Getpid(),
		now:              func() time.Time { return time.Now().UTC() },
		environmentID:    environmentID,
	}, nil
}

func (e *Election) ID() string            { return e.id }
func (e *Election) EnvironmentID() string { return e.environmentID }

// EnvironmentID returns a non-secret digest of a private random token stored
// in the operating system's per-user configuration directory. This models the
// supported loopback-connectivity boundary: ordinary processes for one local
// installation share it, while a host and its containers (and normally two
// containers) use different configuration homes. It deliberately does not use
// hostnames, paths, MAC addresses, machine IDs, boot IDs, or Linux namespace
// inode values.
func EnvironmentID() (string, error) {
	directory, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("locate environment identity directory: %w", err)
	}
	path := filepath.Join(directory, "spynel", "environment-token")
	token, err := readEnvironmentToken(path)
	if errors.Is(err, os.ErrNotExist) {
		data := make([]byte, environmentTokenBytes)
		if _, randomErr := rand.Read(data); randomErr != nil {
			return "", fmt.Errorf("create environment identity: %w", randomErr)
		}
		candidate := hex.EncodeToString(data)
		if createErr := fsx.AtomicCreateFile(path, []byte(candidate+"\n"), 0o600); createErr != nil && !errors.Is(createErr, os.ErrExist) {
			return "", fmt.Errorf("persist environment identity: %w", createErr)
		}
		token, err = readEnvironmentToken(path)
	}
	if err != nil {
		return "", fmt.Errorf("read environment identity: %w", err)
	}
	digest := sha256.Sum256([]byte("spynel-loopback-environment-v1\x00" + token))
	return hex.EncodeToString(digest[:]), nil
}

func readEnvironmentToken(path string) (string, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return "", errors.New("environment identity token must be a private regular file")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	token := strings.TrimSpace(string(data))
	if len(token) != environmentTokenBytes*2 {
		return "", errors.New("environment identity token is invalid")
	}
	if _, err := hex.DecodeString(token); err != nil || token != strings.ToLower(token) {
		return "", errors.New("environment identity token is invalid")
	}
	return token, nil
}

func validEnvironmentID(value string) bool {
	if len(value) != sha256.Size*2 || value != strings.ToLower(value) {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

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
			InstanceID: e.id, PID: e.pid, Endpoint: endpoint, EnvironmentID: e.environmentID, Token: token,
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
		// A matching owner term is authoritative for this process. Restore the
		// validated local identifier if an older or externally damaged record was
		// normalized to the legacy/unknown form while reading it.
		current.EnvironmentID = e.environmentID
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
		current.EnvironmentID = e.environmentID
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

// Release removes only this exact ownership term. It first records a durable
// transition fence so destructive ownerless work cannot enter the short gap
// before an already-running secondary publishes and rebuilds live-client
// protection. It is safe to call after a takeover because it cannot delete the
// successor's record.
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
		if err := e.writeReleaseFence(e.now()); err != nil {
			return fmt.Errorf("persist primary release fence: %w", err)
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

// RunWhileNoPrimaryLease holds the ownership mutation boundary while action
// runs, but only when no primary lease exists at all. This is deliberately
// stricter than takeover eligibility: destructive ownerless work must fail
// closed around a stale or malformed lease and during the durable grace period
// after a clean release because a former owner or an already-running secondary
// may still have live-client state that is not represented by the current
// process.
//
// Holding the election lock for the action prevents a new primary from being
// published until the ownerless operation has finished. The returned boolean
// reports whether action ran.
func (e *Election) RunWhileNoPrimaryLease(action func() error) (bool, error) {
	ran := false
	err := e.withLock(func() error {
		_, err := e.readLease()
		if err == nil {
			return nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("inspect primary lease before ownerless operation: %w", err)
		}
		fence, fenceErr := e.readReleaseFence()
		if fenceErr == nil {
			if e.now().Before(fence.ReleasedAt.Add(OwnerlessCleanupGrace)) {
				return nil
			}
		} else if !errors.Is(fenceErr, os.ErrNotExist) {
			return fmt.Errorf("inspect primary release fence before ownerless operation: %w", fenceErr)
		}
		ran = true
		return action()
	})
	return ran, err
}

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
	// Missing or invalid environment IDs identify an older/incompatible owner,
	// not an ownerless workspace. Preserve its endpoint and heartbeat so a fresh
	// lease remains fenced and clients can make only a bounded compatibility
	// attempt. Every lease written by this version contains a validated ID.
	if !validEnvironmentID(lease.EnvironmentID) {
		lease.EnvironmentID = ""
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

func (e *Election) readReleaseFence() (releaseFence, error) {
	data, err := os.ReadFile(e.releaseFencePath)
	if err != nil {
		return releaseFence{}, err
	}
	var fence releaseFence
	if err := json.Unmarshal(data, &fence); err != nil {
		return releaseFence{}, fmt.Errorf("decode primary release fence: %w", err)
	}
	if fence.ReleasedAt.IsZero() {
		return releaseFence{}, errors.New("primary release fence is incomplete")
	}
	return fence, nil
}

func (e *Election) writeReleaseFence(releasedAt time.Time) error {
	data, err := json.MarshalIndent(releaseFence{ReleasedAt: releasedAt.UTC()}, "", "  ")
	if err != nil {
		return err
	}
	return fsx.AtomicWriteFile(e.releaseFencePath, append(data, '\n'), 0o600)
}

func randomHex(size int) (string, error) {
	data := make([]byte, size)
	if _, err := rand.Read(data); err != nil {
		return "", err
	}
	return hex.EncodeToString(data), nil
}
