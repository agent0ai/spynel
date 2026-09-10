package updater

import "errors"

func lockInstall(string) (func(), error) {
	return nil, errors.New("standalone installation is temporarily unsupported on Windows")
}
