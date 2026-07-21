//go:build !cgo || windows

package main

import (
	"errors"
	"unsafe"
)

type nativeBridge struct{}

func newNativeBridge(path string) *nativeBridge { return &nativeBridge{} }
func (b *nativeBridge) load() error {
	return errors.New("当前平台未启用 Codex Bridge 动态链接")
}
func (b *nativeBridge) loginStart(config []byte) (unsafe.Pointer, string, error) {
	return nil, "", b.load()
}
func (b *nativeBridge) loginWait(session unsafe.Pointer) (string, error) { return "", b.load() }
func (b *nativeBridge) loginCancel(session unsafe.Pointer) error         { return b.load() }
func (b *nativeBridge) sessionFree(session unsafe.Pointer)               {}
func (b *nativeBridge) status(auth, baseURL []byte) (string, error)      { return "", b.load() }
func (b *nativeBridge) tokenRefresh(auth []byte) (string, error)         { return "", b.load() }
func (b *nativeBridge) modelsList(auth []byte) (string, error)           { return "", b.load() }
func (b *nativeBridge) responsesSchema() (string, error)                 { return "", b.load() }
