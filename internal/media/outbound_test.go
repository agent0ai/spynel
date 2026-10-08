package media

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseOutboundExtractsAttachmentAndPhoto(t *testing.T) {
	root := t.TempDir()
	document := filepath.Join(root, "report file.txt")
	photo := filepath.Join(root, "pixel.png")
	if err := os.WriteFile(document, []byte("report"), 0o600); err != nil {
		t.Fatal(err)
	}
	png := []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n', 0, 0, 0, 0}
	if err := os.WriteFile(photo, png, 0o600); err != nil {
		t.Fatal(err)
	}
	text := "Here are the files.\n\n[Send attachment](<" + document + ">)\n[Send photo](<" + photo + ">)\n"
	cleaned, attachments, err := ParseOutbound(text, 1024)
	if err != nil {
		t.Fatal(err)
	}
	if cleaned != "Here are the files." || len(attachments) != 2 {
		t.Fatalf("cleaned = %q, attachments = %#v", cleaned, attachments)
	}
	if attachments[0].Kind != "attachment" || attachments[0].Name != "report file.txt" || attachments[0].MediaType != "text/plain; charset=utf-8" {
		t.Fatalf("document = %#v", attachments[0])
	}
	if attachments[0].MaxBytes != 1024 || attachments[1].MaxBytes != 1024 {
		t.Fatalf("attachment limits = %d, %d", attachments[0].MaxBytes, attachments[1].MaxBytes)
	}
	if attachments[1].Kind != "photo" || attachments[1].MediaType != "image/png" {
		t.Fatalf("photo = %#v", attachments[1])
	}
}

func TestOpenOutboundBoundsGrowthAndRejectsNonregularReplacement(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fixture.txt")
	if err := os.WriteFile(path, []byte("exact"), 0600); err != nil {
		t.Fatal(err)
	}
	_, attachments, err := ParseOutbound("[Send attachment](<"+path+">)", 5)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := OpenOutbound(attachments[0])
	if err != nil {
		t.Fatal(err)
	}
	if data, err := io.ReadAll(reader); err != nil || string(data) != "exact" {
		t.Fatalf("exact limit = %q, %v", data, err)
	}
	_ = reader.Close()
	reader, err = OpenOutbound(attachments[0])
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	if err := os.WriteFile(path, []byte("grew beyond limit"), 0600); err != nil {
		t.Fatal(err)
	}
	if data, err := io.ReadAll(reader); err == nil || len(data) != 5 || !strings.Contains(err.Error(), "grew") {
		t.Fatalf("growth = %q, %v", data, err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenOutbound(attachments[0]); err == nil {
		t.Fatal("nonregular replacement accepted")
	}
}

func TestParseOutboundLeavesOrdinaryAttachmentLinksInert(t *testing.T) {
	text := "Inspect [Attachment report.txt](</workspace/report.txt>) and [a link](https://example.com)."
	cleaned, attachments, err := ParseOutbound(text, 1024)
	if err != nil || cleaned != text || len(attachments) != 0 {
		t.Fatalf("cleaned = %q, attachments = %#v, error = %v", cleaned, attachments, err)
	}
}

func TestParseOutboundAcceptsFileOutsideWorkspaceAndResolvesSymlink(t *testing.T) {
	workspaceRoot := t.TempDir()
	outside := filepath.Join(t.TempDir(), "report.txt")
	if err := os.WriteFile(outside, []byte("report"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(workspaceRoot, "linked-report.txt")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	_, attachments, err := ParseOutbound("[Send attachment](<"+link+">)", 1024)
	if err != nil {
		t.Fatal(err)
	}
	if len(attachments) != 1 || attachments[0].Path != outside || attachments[0].Name != "report.txt" {
		t.Fatalf("attachments = %#v", attachments)
	}
}

func TestParseOutboundRejectsRelativePathsAndDirectories(t *testing.T) {
	for name, directive := range map[string]string{
		"relative":  "[Send attachment](<secret.txt>)",
		"directory": "[Send attachment](<" + t.TempDir() + ">)",
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, err := ParseOutbound(directive, 1024); err == nil {
				t.Fatal("ParseOutbound succeeded")
			}
		})
	}
}

func TestParseOutboundEnforcesSizeAndPhotoType(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "large.txt")
	if err := os.WriteFile(path, []byte("not an image"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ParseOutbound("[Send attachment](<"+path+">)", 4); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("size error = %v", err)
	}
	if _, _, err := ParseOutbound("[Send photo](<"+path+">)", 1024); err == nil || !strings.Contains(err.Error(), "not an image") {
		t.Fatalf("photo error = %v", err)
	}
}

func TestOpenOutboundRechecksSizeAtDelivery(t *testing.T) {
	path := filepath.Join(t.TempDir(), "report.txt")
	if err := os.WriteFile(path, []byte("small"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, attachments, err := ParseOutbound("[Send attachment](<"+path+">)", 5)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("now too large"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenOutbound(attachments[0]); err == nil || !strings.Contains(err.Error(), "delivery time") {
		t.Fatalf("OpenOutbound size error = %v", err)
	}
}
