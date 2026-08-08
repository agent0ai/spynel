package media

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"strings"
)

const speechCacheVersion = "v1"

// SpeechCacheDir resolves and creates the stable per-user namespace for
// automatically managed speech assets. Composition injects this path once.
func SpeechCacheDir() (string, error) {
	return speechCacheDir(os.UserCacheDir)
}

func speechCacheDir(resolve func() (string, error)) (string, error) {
	base, err := resolve()
	if err != nil || strings.TrimSpace(base) == "" {
		if err == nil {
			err = errors.New("platform returned an empty path")
		}
		return "", fmt.Errorf("determine operating-system user cache directory for automatic speech assets: %w; configure speech.model_dir explicitly to avoid automatic model provisioning", err)
	}
	root := speechCachePath(base, runtime.GOOS)
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", fmt.Errorf("create shared speech cache %q: %w; check directory permissions or configure speech.model_dir explicitly", root, err)
	}
	return root, nil
}

func speechCachePath(base, goos string) string {
	separator := "/"
	if goos == "windows" {
		separator = `\`
	}
	trimmed := strings.TrimRight(base, `/\`)
	if trimmed == "" && strings.HasPrefix(base, separator) {
		trimmed = separator
	}
	suffix := strings.Join([]string{"spynel", "speech", speechCacheVersion, "parakeet"}, separator)
	if trimmed == separator {
		return trimmed + suffix
	}
	return trimmed + separator + suffix
}
