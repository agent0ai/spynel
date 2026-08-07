package orchestrator

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/agent0ai/spynel/internal/fsx"
	"gopkg.in/yaml.v3"
)

type Document struct {
	FrontMatter map[string]any
	Body        string
}

func ReadDocument(path string) (Document, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Document{}, err
	}
	return ParseDocument(data)
}

func ParseDocument(data []byte) (Document, error) {
	normalized := strings.ReplaceAll(string(data), "\r\n", "\n")
	if !strings.HasPrefix(normalized, "---\n") {
		return Document{}, errors.New("markdown document must begin with YAML front matter")
	}
	end := strings.Index(normalized[4:], "\n---\n")
	if end < 0 {
		return Document{}, errors.New("markdown front matter is not terminated")
	}
	front := normalized[4 : 4+end]
	body := normalized[4+end+5:]
	metadata := map[string]any{}
	if err := yaml.Unmarshal([]byte(front), &metadata); err != nil {
		return Document{}, err
	}
	return Document{FrontMatter: metadata, Body: body}, nil
}

func (d Document) Bytes() ([]byte, error) {
	front, err := yaml.Marshal(d.FrontMatter)
	if err != nil {
		return nil, err
	}
	var result bytes.Buffer
	result.WriteString("---\n")
	result.Write(front)
	result.WriteString("---\n")
	result.WriteString(d.Body)
	if !strings.HasSuffix(d.Body, "\n") {
		result.WriteByte('\n')
	}
	return result.Bytes(), nil
}

func WriteDocument(path string, document Document) error {
	data, err := document.Bytes()
	if err != nil {
		return err
	}
	return fsx.AtomicWriteFile(path, data, 0o600)
}

func ClaimDocument(source, target, status string, now time.Time) (Document, error) {
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return Document{}, err
	}
	if _, err := os.Stat(target); err == nil {
		return Document{}, fmt.Errorf("claim target already exists: %s", target)
	}
	if err := os.Rename(source, target); err != nil {
		return Document{}, err
	}
	document, err := ReadDocument(target)
	if err != nil {
		_ = os.Rename(target, source)
		return Document{}, err
	}
	document.FrontMatter["status"] = status
	document.FrontMatter["updated_at"] = now.UTC().Format(time.RFC3339)
	attempt := 0
	switch value := document.FrontMatter["attempt"].(type) {
	case int:
		attempt = value
	case int64:
		attempt = int(value)
	case float64:
		attempt = int(value)
	}
	document.FrontMatter["attempt"] = attempt + 1
	if err := WriteDocument(target, document); err != nil {
		return Document{}, err
	}
	return document, nil
}

func DocumentDue(path string, now time.Time) (bool, error) {
	document, err := ReadDocument(path)
	if err != nil {
		return false, err
	}
	for _, field := range []string{"not_before", "next_dispatch_at", "next_review_at"} {
		var when time.Time
		switch value := document.FrontMatter[field].(type) {
		case string:
			when, _ = time.Parse(time.RFC3339, value)
		case time.Time:
			when = value
		}
		if when.IsZero() {
			continue
		}
		if when.After(now) {
			return false, nil
		}
	}
	return true, nil
}
