//go:build !linux && !windows && !android

package tap

// 其他平台暂无真实网卡后端；测试用 Fake 网卡仍可用。

// Open 返回不支持错误。
func Open(name string) (Device, error) {
	return nil, ErrUnsupported
}
