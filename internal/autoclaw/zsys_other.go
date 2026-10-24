//go:build !windows

// zsys_other.go 非 Windows 构建：DPAPI 不可用（enc: 来源明确报错降级）。
package autoclaw

import (
	"errors"
	"os"
)

var goosWindows = false

func readFileBridge(p string) ([]byte, error) { return os.ReadFile(p) }
func getenvBridge(k string) string            { return os.Getenv(k) }

func cryptUnprotectDataWin([]byte) ([]byte, error) {
	return nil, errors.New("DPAPI 解密仅支持 Windows 平台")
}
