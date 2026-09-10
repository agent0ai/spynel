package updater

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func candidateArchive(t *testing.T, version string, mutate func(map[string]string), extra *tar.Header) (string, string) {
	t.Helper()
	files := map[string]string{
		"spynel":  "#!/bin/sh\nprintf 'spynel " + version + "\\n'\n",
		"LICENSE": "license", "THIRD_PARTY_NOTICES.md": "notices",
	}
	for _, name := range []string{"sherpa-onnx", "onnxruntime", "miniaudio", "pion-opus", "bubbletea", "bubbles-textarea"} {
		files["licenses/"+name+"/LICENSE"] = "license"
	}
	if runtime.GOOS == "darwin" {
		files["lib/libsherpa-onnx-c-api.dylib"] = "library"
		files["lib/libonnxruntime.1.27.0.dylib"] = "library"
	} else {
		files["lib/libsherpa-onnx-c-api.so"] = "library"
		files["lib/libonnxruntime.so"] = "library"
	}
	if mutate != nil {
		mutate(files)
	}
	var buffer bytes.Buffer
	gzipWriter := gzip.NewWriter(&buffer)
	writer := tar.NewWriter(gzipWriter)
	for name, content := range files {
		if err := writer.WriteHeader(&tar.Header{Name: "./" + name, Typeflag: tar.TypeReg, Mode: 0755, Size: int64(len(content))}); err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if extra != nil {
		if err := writer.WriteHeader(extra); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	archive := filepath.Join(directory, archiveName(version))
	checksums := filepath.Join(directory, "checksums.txt")
	if err := os.WriteFile(archive, buffer.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(checksums, []byte(fmt.Sprintf("%x  %s\n", sha256.Sum256(buffer.Bytes()), archiveName(version))), 0600); err != nil {
		t.Fatal(err)
	}
	return archive, checksums
}

func TestInstallRetainsWorkingBundleAndRejectsInvalidCandidates(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("standalone distribution excludes Windows")
	}
	parent, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(parent, "installation with spaces Ω")
	ctx := context.Background()
	archive, sums := candidateArchive(t, "1.2.0", nil, nil)
	launcher, err := InstallArchive(ctx, root, archive, sums, "1.2.0")
	if err != nil {
		t.Fatal(err)
	}
	oldExecutable, err := filepath.EvalSymlinks(launcher)
	if err != nil {
		t.Fatal(err)
	}
	// Model a process started through the launcher, including macOS where
	// os.Executable keeps that symlink path. Detection must use its startup target.
	previousExecutable := processExecutable
	processExecutable = oldExecutable
	t.Cleanup(func() { processExecutable = previousExecutable })
	if got := Detect("1.2.0").InstallRoot; got != root {
		t.Fatalf("startup owner = %q", got)
	}
	if got := scriptRootFromExecutable(oldExecutable, "1.2.0"); got != root {
		t.Fatalf("owner = %q", got)
	}
	if got := scriptRootFromExecutable(oldExecutable, "9.0.0"); got != "" {
		t.Fatalf("accepted foreign version: %q", got)
	}
	tests := []struct {
		name    string
		mutate  func(map[string]string)
		header  *tar.Header
		corrupt bool
	}{
		{name: "checksum", corrupt: true},
		{name: "missing libraries", mutate: func(files map[string]string) {
			for name := range files {
				if strings.HasPrefix(name, "lib/") {
					delete(files, name)
				}
			}
		}},
		{name: "wrong version", mutate: func(files map[string]string) { files["spynel"] = "#!/bin/sh\necho spynel 7.0.0\n" }},
		{name: "traversal", header: &tar.Header{Name: "../escape", Typeflag: tar.TypeReg}},
		{name: "absolute", header: &tar.Header{Name: "/escape", Typeflag: tar.TypeReg}},
		{name: "symlink", header: &tar.Header{Name: "link", Typeflag: tar.TypeSymlink, Linkname: "/tmp"}},
		{name: "hardlink", header: &tar.Header{Name: "link", Typeflag: tar.TypeLink, Linkname: "./spynel"}},
		{name: "duplicate", header: &tar.Header{Name: "./spynel", Typeflag: tar.TypeReg}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			archive, sums := candidateArchive(t, "1.3.0", test.mutate, test.header)
			if test.corrupt {
				if err := os.WriteFile(archive, []byte("incomplete"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := InstallArchive(ctx, root, archive, sums, "1.3.0"); err == nil {
				t.Fatal("accepted invalid candidate")
			}
			current, _ := filepath.EvalSymlinks(launcher)
			if current != oldExecutable {
				t.Fatal("changed current on failure")
			}
			if err := verifyBundle(ctx, filepath.Dir(oldExecutable), "1.2.0"); err != nil {
				t.Fatal(err)
			}
		})
	}
	unlock, err := lockInstall(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := InstallArchive(ctx, root, archive, sums, "1.2.0"); err == nil {
		t.Fatal("overlapping installation acquired lock")
	}
	unlock()
	// A leftover stage after an interrupted attempt is never adopted or published.
	if err := os.Mkdir(filepath.Join(root, ".stage-interrupted"), 0700); err != nil {
		t.Fatal(err)
	}
	archive, sums = candidateArchive(t, "1.3.0", nil, nil)
	if _, err := InstallArchive(ctx, root, archive, sums, "1.3.0"); err != nil {
		t.Fatal(err)
	}
	current, _ := filepath.EvalSymlinks(launcher)
	if current == oldExecutable {
		t.Fatal("update did not switch bundle")
	}
	if got := Detect("1.2.0"); got.InstallRoot != root || got.PackageRoot != "" {
		t.Fatalf("running process lost ownership after launcher switched: %#v", got)
	}
	if got := Detect("9.0.0").InstallRoot; got != "" {
		t.Fatalf("accepted foreign version after switch: %q", got)
	}
	if got := scriptRootFromExecutable(launcher, "1.2.0"); got != "" {
		t.Fatalf("fresh detection adopted a different bundle version: %q", got)
	}
	if err := verifyBundle(ctx, filepath.Dir(oldExecutable), "1.2.0"); err != nil {
		t.Fatal("old runtime removed:", err)
	}
	if got := RestartExecutable(oldExecutable); got != launcher {
		t.Fatalf("restart pinned to old bundle: %q", got)
	}
	if scriptRootFromExecutable(current, "1.3.0") != root {
		t.Fatal("ownership lost after update")
	}
	archive, sums = candidateArchive(t, "1.2.0", nil, nil)
	if _, err := InstallArchive(ctx, root, archive, sums, "1.2.0"); err == nil {
		t.Fatal("allowed downgrade")
	}
	unmanaged := t.TempDir()
	if err := os.WriteFile(filepath.Join(unmanaged, "unrelated"), []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := InstallArchive(ctx, unmanaged, archive, sums, "1.2.0"); err == nil {
		t.Fatal("adopted unmanaged directory")
	}
}

func TestGitHubDiscoveryAndDownloadFailures(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("standalone distribution excludes Windows")
	}
	archive, sums := candidateArchive(t, "1.2.0", nil, nil)
	root := filepath.Join(t.TempDir(), "install")
	if _, err := InstallArchive(context.Background(), root, archive, sums, "1.2.0"); err != nil {
		t.Fatal(err)
	}
	next, nextSums := candidateArchive(t, "1.3.0", nil, nil)
	var state atomic.Value
	state.Store("valid")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mode := state.Load().(string)
		if r.URL.Path == "/latest" {
			if mode == "timeout" {
				<-r.Context().Done()
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"tag_name": "v1.3.0", "prerelease": mode == "prerelease"})
		} else if r.URL.Path == "/checksums.txt" {
			http.ServeFile(w, r, nextSums)
		} else {
			if mode == "incomplete" {
				w.Header().Set("Content-Length", "1000")
				_, _ = w.Write([]byte("truncated"))
				return
			}
			http.ServeFile(w, r, next)
		}
	}))
	defer server.Close()
	t.Setenv("SPYNEL_DOWNLOAD_BASE", server.URL)
	manager := &Manager{CurrentVersion: "1.2.0", InstallRoot: root, GitHubURL: server.URL + "/latest", CheckTimeout: 20 * time.Millisecond}
	result, err := manager.Check(context.Background())
	if err != nil || result.Source != "GitHub" || !result.Available || !result.CanAutoInstall || result.InstalledViaNPM {
		t.Fatalf("result %#v, %v", result, err)
	}
	for _, bad := range []string{"prerelease", "timeout"} {
		state.Store(bad)
		if _, err := manager.Check(context.Background()); err == nil {
			t.Fatalf("accepted %s", bad)
		}
	}
	state.Store("incomplete")
	before, _ := os.Readlink(filepath.Join(root, "current"))
	if err := manager.Install(context.Background(), "1.3.0"); err == nil {
		t.Fatal("accepted incomplete download")
	}
	after, _ := os.Readlink(filepath.Join(root, "current"))
	if before != after {
		t.Fatal("incomplete download changed installation")
	}
	state.Store("valid")
	if err := manager.Install(context.Background(), "1.3.0"); err != nil {
		t.Fatal(err)
	}
}

func TestNPMEnvironmentCannotClaimUnrelatedExecutable(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "npm", "vendor"), 0700); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(root, "package.json"), []byte(`{"name":"spynel","version":"1.2.0"}`), 0600)
	_ = os.WriteFile(filepath.Join(root, "npm", "vendor", ".installed.json"), []byte(`{"version":"1.2.0"}`), 0600)
	_ = os.WriteFile(filepath.Join(root, "npm", "vendor", "spynel"), []byte("unrelated"), 0700)
	t.Setenv("SPYNEL_NPM_PACKAGE_ROOT", root)
	t.Setenv("SPYNEL_NPM_LAUNCHER_MANAGED", "1")
	if got := Detect("1.2.0"); got.PackageRoot != "" {
		t.Fatal("inherited npm environment claimed unrelated binary")
	}
}

func TestInstallLockIsReleasedAfterCrash(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("standalone distribution excludes Windows")
	}
	if root := os.Getenv("SPYNEL_TEST_INSTALL_LOCK"); root != "" {
		unlock, err := lockInstall(root)
		if err != nil {
			t.Fatal(err)
		}
		defer unlock()
		if err := os.WriteFile(filepath.Join(root, "ready"), []byte("ready"), 0600); err != nil {
			t.Fatal(err)
		}
		time.Sleep(time.Minute)
		return
	}
	root := t.TempDir()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(executable, "-test.run=^TestInstallLockIsReleasedAfterCrash$")
	command.Env = append(os.Environ(), "SPYNEL_TEST_INSTALL_LOCK="+root)
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = command.Process.Kill(); _ = command.Wait() }()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(root, "ready")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("lock fixture did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if unlock, err := lockInstall(root); err == nil {
		unlock()
		t.Fatal("acquired live installer's lock")
	}
	if err := command.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = command.Wait()
	unlock, err := lockInstall(root)
	if err != nil {
		t.Fatal("crashed installer retained lock:", err)
	}
	unlock()
}
