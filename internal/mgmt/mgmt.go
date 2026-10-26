// Package mgmt 提供本机管理面：仅回环 TCP 监听，以 JSON 应答
// clients/status 等本机查询。server 与 client 实例共用同一套实现，
// 同机多实例由默认端口的自动避让区分（7790、7791、7792…）。
package mgmt

import (
	"encoding/json"
	"errors"
	"log"
	"net"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/Re-s/vrndisd/internal/proto"
)

// DefaultListen 默认管理监听地址（仅本机回环，避免外网暴露）。
const DefaultListen = "127.0.0.1:7790"

// portTries 默认端口避让尝试个数（7790..7799）。
const portTries = 10

// watchInterval 监听全部失败后的重试间隔（包级变量便于测试调小）。
var watchInterval = 10 * time.Second

// ioTimeout 单个管理连接的读写超时（管理查询应秒级完成）。
const ioTimeout = 5 * time.Second

// StateProvider 返回当前实时管理状态快照（调用方保证并发安全）。
type StateProvider func() proto.ManagementState

// Server 本机管理面。Serve 返回后后台 accept 循环响应查询；Close 后停止。
// 禁用（"-"）时不监听任何端口，Addr() 恒为空。
type Server struct {
	provider StateProvider
	logger   *log.Logger

	mu   sync.RWMutex
	ln   net.Listener
	addr string

	stopCh    chan struct{}
	closeOnce sync.Once
}

// Serve 启动管理面（后台循环，非阻塞）。
//
// listen 语义：
//   - "-"：禁用管理面（返回占位实例，Addr() 恒空）。
//   - ""：用 DefaultListen；默认端口被占时自动尝试端口 +1（共 portTries 个），
//     适配同机多实例——第一个实例占 7790，第二个占 7791，依此类推。
//   - "host:port"：显式指定，不自动避让；占用等失败按 watchInterval
//     退避重试（保持与历史行为一致，不悄悄换掉用户指定的端口）。
func Serve(listen string, provider StateProvider, logger *log.Logger) *Server {
	m := &Server{provider: provider, logger: logger, stopCh: make(chan struct{})}
	if listen == "-" {
		return m
	}
	addr, allowFallback := resolveListen(listen)
	go m.loop(addr, allowFallback)
	return m
}

// resolveListen 解析监听地址，返回（地址, 是否允许端口避让）。
// 仅默认（空）配置启用避让：显式指定即视为用户锁定该端口。
func resolveListen(listen string) (string, bool) {
	if listen == "" {
		return DefaultListen, true
	}
	return listen, false
}

// Addr 返回当前实际监听地址（未监听 / 已停止 / 已禁用时返回空）。
// Close 之后即使后台循环尚未退出也立即返回空。
func (m *Server) Addr() string {
	select {
	case <-m.stopCh:
		return ""
	default:
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.ln == nil {
		return ""
	}
	return m.addr
}

// Close 停止管理面并关闭监听。可重复调用、可与其他方法并发调用。
func (m *Server) Close() {
	m.closeOnce.Do(func() {
		close(m.stopCh)
		m.mu.Lock()
		if m.ln != nil {
			_ = m.ln.Close()
			m.ln = nil
		}
		m.mu.Unlock()
	})
}

// loop 监听-接受循环：listener 失效时重建，全部端口被占时按 watchInterval 重试。
func (m *Server) loop(addr string, allowFallback bool) {
	for {
		select {
		case <-m.stopCh:
			return
		default:
		}
		ln, actual := m.tryListen(addr, allowFallback)
		if ln == nil {
			select {
			case <-m.stopCh:
				return
			case <-time.After(watchInterval):
				continue
			}
		}
		m.mu.Lock()
		m.ln, m.addr = ln, actual
		m.mu.Unlock()
		if !m.accept(ln) {
			return // 收到停止信号
		}
		m.mu.Lock()
		m.ln, m.addr = nil, ""
		m.mu.Unlock()
	}
}

// tryListen 尝试创建 listener。允许避让时从基准端口起逐个 +1 跳过被占端口；
// 其余错误（权限不足等）不避让。全部端口被占返回 nil。
func (m *Server) tryListen(addr string, allowFallback bool) (net.Listener, string) {
	if !allowFallback {
		return m.listenAt(addr)
	}
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return m.listenAt(addr)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return m.listenAt(addr)
	}
	for i := 0; i < portTries; i++ {
		candidate := net.JoinHostPort(host, strconv.Itoa(port+i))
		ln, listenErr := net.Listen("tcp", candidate)
		if listenErr == nil {
			if i > 0 {
				m.logger.Printf("github.com/Re-s/vrndisd/mgmt: 管理端口 %s 被占用，改用 %s", addr, candidate)
			}
			return ln, ln.Addr().String()
		}
		if !isAddrInUse(listenErr) {
			m.logger.Printf("github.com/Re-s/vrndisd/mgmt: 管理端口监听失败 %s: %v", addr, listenErr)
			return nil, ""
		}
	}
	m.logger.Printf("github.com/Re-s/vrndisd/mgmt: 管理端口 %s~%d 均被占用，%v 后重试", addr, port+portTries-1, watchInterval)
	return nil, ""
}

// listenAt 在单个地址上监听，失败返回 nil 并记录日志。
func (m *Server) listenAt(addr string) (net.Listener, string) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		m.logger.Printf("github.com/Re-s/vrndisd/mgmt: 管理端口监听失败 %s: %v", addr, err)
		return nil, ""
	}
	return ln, ln.Addr().String()
}

// isAddrInUse 判断是否"地址已占用"错误（仅此类错误触发端口避让）。
func isAddrInUse(err error) bool {
	if errors.Is(err, syscall.EADDRINUSE) {
		return true
	}
	return strings.Contains(strings.ToLower(err.Error()), "in use")
}

// accept 接受并同步处理查询连接，直到 listener 失效或收到停止信号。
// 返回 false 表示已停止（循环应退出）；true 表示 listener 失效（应重建）。
func (m *Server) accept(ln net.Listener) bool {
	for {
		select {
		case <-m.stopCh:
			ln.Close()
			return false
		default:
		}
		conn, err := ln.Accept()
		if err == nil {
			m.handle(conn)
			continue
		}
		select {
		case <-m.stopCh:
			ln.Close()
			return false
		default:
		}
		return true // listener 被关闭等：重建
	}
}

// handle 处理单个管理查询连接（同步短连接）：读一行请求 JSON，
// 原样回 provider 生成的状态快照。非法请求直接关闭。
func (m *Server) handle(conn net.Conn) {
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(ioTimeout))
	var req proto.ManagementRequest
	if err := json.NewDecoder(conn).Decode(&req); err != nil {
		return
	}
	state := m.provider()
	_ = conn.SetWriteDeadline(time.Now().Add(ioTimeout))
	_ = json.NewEncoder(conn).Encode(state)
}

// SetWatchIntervalForTest 仅供测试：调整监听失败重试间隔。
func SetWatchIntervalForTest(d time.Duration) { watchInterval = d }
