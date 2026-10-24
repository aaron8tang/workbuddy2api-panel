//go:build windows

// zsys_windows.go DPAPI 系统调用（仅 Windows 构建）。
package autoclaw

import (
	"fmt"
	"os"
	"runtime"
	"syscall"
	"unsafe"
)

var (
	kernel32          = syscall.NewLazyDLL("kernel32.dll")
	procGetLocalFree  = kernel32.NewProc("LocalFree")
	crypt32           = syscall.NewLazyDLL("crypt32.dll")
	procUnprotectData = crypt32.NewProc("CryptUnprotectData")
)

// goosWindows 编译期平台标记（isWindows 桥用）。
var goosWindows = runtime.GOOS == "windows"

func readFileBridge(p string) ([]byte, error) { return os.ReadFile(p) }
func getenvBridge(k string) string            { return os.Getenv(k) }

type dataBlob struct {
	cbData uint32
	pbData *byte
}

// cryptUnprotectDataWin CurrentUser 作用域、描述符与标志全零（crypto.rs:123-183 同参）。
func cryptUnprotectDataWin(data []byte) ([]byte, error) {
	if len(data) == 0 {
		return nil, fmt.Errorf("空数据")
	}
	in := dataBlob{cbData: uint32(len(data)), pbData: &data[0]}
	var out dataBlob
	r1, _, err := procUnprotectData.Call(
		uintptr(unsafe.Pointer(&in)), 0, 0, 0, 0,
		uintptr(unsafe.Pointer(&out)), 0)
	if r1 == 0 {
		return nil, fmt.Errorf("CryptUnprotectData: %v", err)
	}
	defer procGetLocalFree.Call(uintptr(unsafe.Pointer(out.pbData)))
	outBytes := make([]byte, out.cbData)
	copy(outBytes, unsafe.Slice(out.pbData, out.cbData))
	return outBytes, nil
}
