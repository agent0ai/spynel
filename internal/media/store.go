package media

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

var unsafeName = regexp.MustCompile(`[^a-zA-Z0-9._-]+`)

type Attachment struct {
	Name string
	Path string
}

func (s Store) CleanupOlderThan(age time.Duration) (int, error) {
	if age <= 0 {
		return 0, nil
	}
	entries, err := os.ReadDir(s.Directory)
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	cutoff := time.Now().Add(-age)
	removed := 0
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() || !info.ModTime().Before(cutoff) {
			continue
		}
		if err := os.Remove(filepath.Join(s.Directory, entry.Name())); err != nil {
			return removed, err
		}
		removed++
	}
	return removed, nil
}

type Transcriber interface {
	Transcribe(context.Context, string) (string, error)
}

func (a Attachment) Token() string {
	label := strings.ReplaceAll(a.Name, "]", "_")
	return "[Attachment " + label + "](<" + filepath.ToSlash(a.Path) + ">)"
}

type Store struct {
	Directory string
	MaxBytes  int64
}

// Save streams an attachment into a private, collision-safe file. The limit is
// enforced while copying, so an untrusted transport cannot make Spynel retain
// an unbounded body in memory or on disk.
func (s Store) Save(ctx context.Context, name string, source io.Reader) (Attachment, error) {
	return s.Create(ctx, name, func(destination *os.File) error {
		reader := source
		if s.MaxBytes > 0 {
			reader = io.LimitReader(source, s.MaxBytes+1)
		}
		_, err := copyContext(ctx, destination, reader)
		return err
	})
}

// Create lets an SDK stream encrypted media directly into the private
// temporary file. The completed size is checked before it becomes visible.
func (s Store) Create(ctx context.Context, name string, write func(*os.File) error) (Attachment, error) {
	if strings.TrimSpace(s.Directory) == "" {
		return Attachment{}, errors.New("attachment directory is not configured")
	}
	if err := os.MkdirAll(s.Directory, 0o700); err != nil {
		return Attachment{}, err
	}
	name = safeName(name)
	temporary, err := os.CreateTemp(s.Directory, ".attachment-*")
	if err != nil {
		return Attachment{}, err
	}
	temporaryName := temporary.Name()
	defer func() {
		_ = temporary.Close()
		_ = os.Remove(temporaryName)
	}()
	if err := temporary.Chmod(0o600); err != nil {
		return Attachment{}, err
	}
	if err := write(temporary); err != nil {
		return Attachment{}, err
	}
	info, err := temporary.Stat()
	if err != nil {
		return Attachment{}, err
	}
	if s.MaxBytes > 0 && info.Size() > s.MaxBytes {
		return Attachment{}, fmt.Errorf("attachment exceeds the %d byte limit", s.MaxBytes)
	}
	if err := temporary.Sync(); err != nil {
		return Attachment{}, err
	}
	if err := temporary.Close(); err != nil {
		return Attachment{}, err
	}
	destination, err := s.commit(temporaryName, name)
	if err != nil {
		return Attachment{}, err
	}
	_ = os.Remove(temporaryName)
	absolute, err := filepath.Abs(destination)
	if err != nil {
		return Attachment{}, err
	}
	return Attachment{Name: filepath.Base(destination), Path: absolute}, nil
}

func (s Store) commit(temporary, name string) (string, error) {
	extension := filepath.Ext(name)
	stem := strings.TrimSuffix(name, extension)
	for index := 0; index < 100000; index++ {
		candidateName := name
		if index > 0 {
			candidateName = fmt.Sprintf("%s-%d%s", stem, index+1, extension)
		}
		candidate := filepath.Join(s.Directory, candidateName)
		if err := os.Link(temporary, candidate); err == nil {
			return candidate, nil
		} else if !os.IsExist(err) {
			return "", err
		}
	}
	return "", errors.New("cannot allocate a unique attachment name")
}

func safeName(name string) string {
	name = filepath.Base(strings.TrimSpace(name))
	name = strings.Trim(unsafeName.ReplaceAllString(name, "_"), "._")
	if name == "" {
		return "attachment"
	}
	if len(name) > 180 {
		extension := filepath.Ext(name)
		name = strings.TrimSuffix(name, extension)
		if len(extension) > 20 {
			extension = extension[:20]
		}
		name = name[:max(1, 180-len(extension))] + extension
	}
	return name
}

func copyContext(ctx context.Context, destination io.Writer, source io.Reader) (int64, error) {
	buffer := make([]byte, 64*1024)
	var written int64
	for {
		if err := ctx.Err(); err != nil {
			return written, err
		}
		count, readErr := source.Read(buffer)
		if count > 0 {
			output, writeErr := destination.Write(buffer[:count])
			written += int64(output)
			if writeErr != nil {
				return written, writeErr
			}
			if output != count {
				return written, io.ErrShortWrite
			}
		}
		if readErr == io.EOF {
			return written, nil
		}
		if readErr != nil {
			return written, readErr
		}
	}
}
