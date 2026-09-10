//go:build windows

package localapi

import (
	"errors"
	"net"
)

type socketDescriptor struct{ Token, WorkspaceID string }

func ListenSocket(string, string, string) (net.Listener, error) {
	return nil, errors.New("Unix socket integration is unsupported on Windows")
}
func NewSocketClient(string) (*Client, error) {
	return nil, errors.New("Unix socket integration is unsupported on Windows")
}
func readSocketDescriptor(string) (socketDescriptor, error) {
	return socketDescriptor{}, errors.New("Unix socket integration is unsupported on Windows")
}
