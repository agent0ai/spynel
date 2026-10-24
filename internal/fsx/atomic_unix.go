//go:build !windows

package fsx

import "os"

// AtomicCreateOpenFile publishes a complete new file and returns its open handle.
// The caller must close it after any cleanup that relies on its inode identity.
func AtomicCreateOpenFile(path string, data []byte, mode os.FileMode) (*os.File, error) {
	file, err := prepareCreateFile(path, data, mode)
	if err != nil {
		return nil, err
	}
	defer os.Remove(file.Name())
	if err := os.Link(file.Name(), path); err != nil {
		file.Close()
		return nil, err
	}
	return file, nil
}
