package updater

import "errors"

func lockUpdate(string, bool) (func(), error) {
	return nil, errors.New("worker updates are temporarily unsupported on Windows")
}

func lockInstall(string) (func(), error) {
	return nil, errors.New("standalone installation is temporarily unsupported on Windows")
}
