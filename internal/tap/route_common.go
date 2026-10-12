package tap

import "strings"

// isAlreadyExists 判断 ip 命令失败是否只是"路由/地址已存在"（RTNETLINK answers: File exists），
// 这类错误属幂等正常情形，应忽略；其余 RTNETLINK 错误（Network is unreachable、
// Operation not permitted、No such device 等）都要向上传递，不能误吞。
//
// 供 linux / android 真实 TAP 实现共享（它们都用 `ip route add/...` 配直连与默认路由）。
func isAlreadyExists(err error, out []byte) bool {
	if err == nil {
		return false
	}
	return strings.Contains(string(out), "File exists")
}
