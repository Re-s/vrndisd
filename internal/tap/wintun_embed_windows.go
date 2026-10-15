//go:build windows

package tap

import (
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"syscall"
)

// wintun.dll 与可执行文件打包为单文件（部署无需额外带 dll）：
// 内嵌数据在 ensureWintun 时释放到磁盘再显式加载。
// 优先级：exe 同目录已有 wintun.dll（用户手动放置或上次释放）→
// 释放内嵌 dll 到 exe 同目录 → 失败回退 %TEMP%。
//
//go:embed wintun.dll
var wintunDLLData []byte

var (
	wintunOnce    sync.Once
	wintunLoadErr error
)

// ensureWintun 加载 wintun.dll 并初始化全部 proc（仅一次）。
// 由 Open 调用；失败后不再重试（进程内 dll 加载状态不可逆）。
func ensureWintun() error {
	wintunOnce.Do(func() {
		path, err := wintunDLLPath()
		if err != nil {
			wintunLoadErr = err
			return
		}
		dll, err := syscall.LoadDLL(path)
		if err != nil {
			wintunLoadErr = fmt.Errorf("%w: 加载 wintun.dll 失败（%s）: %v", ErrUnsupported, path, err)
			return
		}
		// 显式绑定 proc（替代 syscall.NewLazyDLL 的包级延迟绑定）。
		binds := map[string]**syscall.Proc{
			"WintunCreateAdapter":        &procWintunCreateAdapter,
			"WintunOpenAdapter":          &procWintunOpenAdapter,
			"WintunCloseAdapter":         &procWintunCloseAdapter,
			"WintunStartSession":         &procWintunStartSession,
			"WintunEndSession":           &procWintunEndSession,
			"WintunGetReadWaitEvent":     &procWintunGetReadWait,
			"WintunReceivePacket":        &procWintunReceivePacket,
			"WintunReleaseReceivePacket": &procWintunReleasePacket,
			"WintunAllocateSendPacket":   &procWintunAllocSend,
			"WintunSendPacket":           &procWintunSendPacket,
		}
		for name, p := range binds {
			proc, err := dll.FindProc(name)
			if err != nil {
				wintunLoadErr = fmt.Errorf("%w: wintun.dll 缺少导出 %s: %v", ErrUnsupported, name, err)
				return
			}
			*p = proc
		}
	})
	return wintunLoadErr
}

// wintunDLLPath 确保磁盘上存在 wintun.dll 并返回其绝对路径。
func wintunDLLPath() (string, error) {
	if exe, err := os.Executable(); err == nil {
		p := filepath.Join(filepath.Dir(exe), "wintun.dll")
		if _, err := os.Stat(p); err == nil {
			return p, nil // 已存在：直接用（兼容用户手动放置的 dll）
		}
		if err := os.WriteFile(p, wintunDLLData, 0644); err == nil {
			return p, nil
		}
		// exe 同目录不可写（只读位置/权限不足）→ 回退 temp
	}
	p := filepath.Join(os.TempDir(), "vrndis-wintun.dll")
	if _, err := os.Stat(p); err != nil {
		if err := os.WriteFile(p, wintunDLLData, 0644); err != nil {
			return "", fmt.Errorf("%w: 释放 wintun.dll 失败（%s）: %v", ErrUnsupported, p, err)
		}
	}
	return p, nil
}
