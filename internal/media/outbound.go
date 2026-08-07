package media

import (
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/agent0ai/spynel/internal/core"
)

var outboundDirective = regexp.MustCompile(`(?m)^[\t ]*\[Send (attachment|photo)\]\(<([^>\r\n]+)>\)[\t ]*(?:\r?\n|$)`)

// ParseOutbound extracts explicit file-delivery directives from an agent's
// final response. Files must use absolute paths, resolve to readable regular
// files, and fit within maxBytes. The returned text has directive lines removed.
func ParseOutbound(text string, maxBytes int64) (string, []core.OutboundAttachment, error) {
	matches := outboundDirective.FindAllStringSubmatchIndex(text, -1)
	if len(matches) == 0 {
		return text, nil, nil
	}
	attachments := make([]core.OutboundAttachment, 0, len(matches))
	for _, match := range matches {
		kind := text[match[2]:match[3]]
		path := text[match[4]:match[5]]
		attachment, err := validateOutbound(kind, path, maxBytes)
		if err != nil {
			return text, nil, err
		}
		attachments = append(attachments, attachment)
	}
	cleaned := strings.TrimSpace(outboundDirective.ReplaceAllString(text, ""))
	return cleaned, attachments, nil
}

func validateOutbound(kind, path string, maxBytes int64) (core.OutboundAttachment, error) {
	if maxBytes <= 0 {
		return core.OutboundAttachment{}, fmt.Errorf("outbound attachments require a positive byte limit")
	}
	if !filepath.IsAbs(path) {
		return core.OutboundAttachment{}, fmt.Errorf("outbound attachment path must be absolute: %s", path)
	}
	resolved, err := filepath.EvalSymlinks(filepath.Clean(path))
	if err != nil {
		return core.OutboundAttachment{}, fmt.Errorf("resolve outbound attachment %s: %w", filepath.Base(path), err)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return core.OutboundAttachment{}, fmt.Errorf("inspect outbound attachment %s: %w", filepath.Base(path), err)
	}
	if !info.Mode().IsRegular() {
		return core.OutboundAttachment{}, fmt.Errorf("outbound attachment is not a regular file: %s", filepath.Base(path))
	}
	if info.Size() > maxBytes {
		return core.OutboundAttachment{}, fmt.Errorf("outbound attachment %s exceeds the %d byte limit", filepath.Base(path), maxBytes)
	}
	mediaType, err := detectMediaType(resolved)
	if err != nil {
		return core.OutboundAttachment{}, err
	}
	if kind == "photo" && !strings.HasPrefix(mediaType, "image/") {
		return core.OutboundAttachment{}, fmt.Errorf("outbound photo is not an image: %s", filepath.Base(path))
	}
	return core.OutboundAttachment{Kind: kind, Name: filepath.Base(resolved), Path: resolved, MediaType: mediaType, MaxBytes: maxBytes}, nil
}

// OpenOutbound reopens a validated outbound file at delivery time and reapplies
// its byte policy to the opened handle. This prevents replacement or growth
// between parsing the agent response and streaming the native attachment.
func OpenOutbound(attachment core.OutboundAttachment) (io.ReadCloser, error) {
	if attachment.MaxBytes <= 0 {
		return nil, fmt.Errorf("outbound attachment %s has no byte limit", attachment.Name)
	}
	file, err := os.Open(attachment.Path)
	if err != nil {
		return nil, fmt.Errorf("open outbound attachment %s: %w", attachment.Name, err)
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("inspect outbound attachment %s: %w", attachment.Name, err)
	}
	if !info.Mode().IsRegular() {
		_ = file.Close()
		return nil, fmt.Errorf("outbound attachment is no longer a regular file: %s", attachment.Name)
	}
	if info.Size() > attachment.MaxBytes {
		_ = file.Close()
		return nil, fmt.Errorf("outbound attachment %s exceeds the %d byte limit at delivery time", attachment.Name, attachment.MaxBytes)
	}
	return &outboundReadCloser{file: file, remaining: attachment.MaxBytes, name: attachment.Name}, nil
}

type outboundReadCloser struct {
	file      *os.File
	remaining int64
	name      string
}

func (reader *outboundReadCloser) Read(buffer []byte) (int, error) {
	if reader.remaining > 0 {
		if int64(len(buffer)) > reader.remaining {
			buffer = buffer[:reader.remaining]
		}
		count, err := reader.file.Read(buffer)
		reader.remaining -= int64(count)
		return count, err
	}
	var probe [1]byte
	count, err := reader.file.Read(probe[:])
	if count > 0 {
		return 0, fmt.Errorf("outbound attachment %s grew beyond its byte limit during delivery", reader.name)
	}
	return 0, err
}

func (reader *outboundReadCloser) Close() error { return reader.file.Close() }

func detectMediaType(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open outbound attachment %s: %w", filepath.Base(path), err)
	}
	defer file.Close()
	buffer := make([]byte, 512)
	count, err := file.Read(buffer)
	if err != nil && err != io.EOF {
		return "", fmt.Errorf("read outbound attachment %s: %w", filepath.Base(path), err)
	}
	mediaType := http.DetectContentType(buffer[:count])
	if mediaType == "application/octet-stream" {
		if extensionType := mime.TypeByExtension(strings.ToLower(filepath.Ext(path))); extensionType != "" {
			mediaType = extensionType
		}
	}
	return mediaType, nil
}
