// Package server 实现虚拟 RNDIS 服务器。
package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"vrndis/internal/addr"
	"vrndis/internal/ethernet"
	"vrndis/internal/ethswitch"
	"vrndis/internal/proto"
	"vrndis/internal/tap"
)

const (
	tapPort       = "tap"
	defaultPingInterval = 30 * time.Second
	pingTimeout     = 10 * time.Second
	maxDormantTime  = 3 * time.Minute
)

type ClientStatus string

const (
	StatusOnline    ClientStatus = "online"
	StatusOffline   ClientStatus = "offline"
	StatusSuspect   ClientStatus = "suspect"
)

// ClientInfo 客户端详细信息
type ClientInfo struct {
	IP           net.IP
	MAC          net.HardwareAddr
	Peer         string
	Name         string
	Status       ClientStatus
	FirstConnect time.Time
	LastActive   time.Time
	Expires      time.Time
	LeaseID      uint64
	Prefix       int
	Gateway      net.IP
	DNS          []string
	MTU          int
	ServerMac    net.HardwareAddr
}

// ClientManager 客户端管理器
type ClientManager struct {
	servers *Server
	mu      sync.RWMutex
	sessions map[string]*sessionInfo // key: mac or IP
	lastSeen time.Time
}

type sessionInfo struct {
	*session
	clientInfo *ClientInfo
	(startTime  time.Time
	lastActive time.Time
	lastPing   time.Time
	pingOutstanding bool
}

func (s *sessionInfo) updateActive(t time.Time) {
	s.lastActive = t
	s.clientInfo.LastActive = t
	if s.clientInfo.Expires.IsZero() || t.After(s.clientInfo.Expires) {
		// 租约不超过 1 小时
		expires := t.Add(1 * time.Hour)
		s.clientInfo.Expires = expires
		s.session.lease.Expiry = expires
	}
}

func NewClientManager(server *Server) *ClientManager {
	return &ClientManager{
		servers: server,
		sessions: make(map[string]*sessionInfo),
	}
}

// GetClientByIP 通过 IP 获取客户端信息
func (m *ClientManager) GetClientByIP(ip net.IP) *ClientInfo {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, s := range m.sessions {
		if s.clientInfo.IP.Equal(ip) {
			return s.clientInfo
		}
	}
	return nil
}

// GetClientByMAC 通过 MAC 获取客户端信息
func (m *ClientManager) GetClientByMAC(mac net.HardwareAddr) *ClientInfo {
	m.mu.RLock()
	defer m.mu.RUnlock()
	macStr := mac.String()
	s, ok := m.sessions[macStr]
	if !ok {
		return nil
	}
	return s.clientInfo
}

// GetClientByPeer 通过 peer 字符串获取客户端信息
func (m *ClientManager) GetClientByPeer(peer string) *ClientInfo {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, s := range m.sessions {
		if s.clientInfo.Peer == peer {
			return s.clientInfo
		}
	}
	return nil
}

// ListClients 列出所有在线客户端
func (m *ClientManager) ListClients() []*ClientInfo {
	m.mu.RLock()
	defer m.mu.RUnlock()
	clients := make([]*ClientInfo, 0, len(m.sessions))
	for _, s := range m.sessions {
		// 更新状态
		s.clientInfo.Status = StatusOnline
		clients = append(clients, s.clientInfo)
	}
	return clients
}

// GetClientCount 获取在线客户端数量
func (m *ClientManager) GetClientCount() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.sessions)
}

// SendCommandToClient 向指定客户端发送管理命令
func (m *ClientManager) SendCommandToClient(mac string, cmd *ManagementCommand) error {
	m.mu.RLock()
	sess, ok := m.sessions[mac]
	m.mu.RUnlock()
	if !ok {
		return fmt.Errorf("客户端未找到 (mac=%s)", mac)
	}
	return sess.send(proto.TypeManagement, mustJSON(cmd))
}

// KickClient 踢掉客户端
func (m *ClientManager) KickClient(mac string) error {
	m.mu.RLock()
	sess, ok := m.sessions[mac]
	m.mu.RUnlock()
	if !ok {
		return fmt.Errorf("客户端未找到 (mac=%s)", mac)
	}
	// 重置保存逻辑会在 defer 中处理，先 close connection
	sess.conn.Close()
	return nil
}

// 客户端管理器的心跳处理
func (m *ClientManager) startHeartbeat(ctx context.Context) {
	ticker := time.NewTicker(defaultPingInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.pingClients()
		}
	}
}

func (m *ClientManager) pingClients() {
	m.mu.Lock()
	defer m.mu.Unlock()

	for mac, sess := range m.sessions {
		if sess.pingOutstanding {
			continue // 心跳未超时，跳过
		}
		// 发送 PING
		if sess.clientInfo.LastActive.Before(time.Now().Add(-maxDormantTime)) {
			// 客户端太久未活跃，标记为 suspect
			sess.clientInfo.Status = StatusSuspect
			log.Printf("vrndis server: 客户端 %s 长时间无活跃 (%s) -> suspect", sess.clientInfo.Peer, time.Since(sess.clientInfo.LastActive))
			continue
		}
		// 尝试发送 PING
		err := sess.send(proto.TypePing, nil)
		if err != nil {
			// 发送失败，直接断开
			log.Printf("vrndis server: PING 发送失败 %s: %v, 断开连接", mac, err)
			sess.conn.Close()
			delete(m.sessions, mac)
			continue
		}
		sess.pingOutstanding = true
		// 设置超时清理
		go func() {
			time.AfterFunc(pingTimeout, func() {
				m.mu.Lock()
				defer m.mu.Unlock()
				if s, ok := m.sessions[mac]; ok && s.pingOutstanding {
					// 超时未响应，断开连接
					log.Printf("vrndis server: PING 超时 %s, 断开连接", mac)
					s.conn.Close()
					delete(m.sessions, mac)
				}
			})
		}()
	}
}

type ManagementCommand struct {
	Command string            `json:"command"`
	Payload json.RawMessage   `json:"payload"`
}

// ---- 内部 ----

func New(cfg Config) (*Server, error) {
	if cfg.Listen == "" {
		cfg.Listen = "0.0.0.0:7777"
	}
	if cfg.MTU == 0 {
		cfg.MTU = 1500
	}
	if len(cfg.DNS) == 0 {
		cfg.DNS = []string{"8.8.8.8", "1.1.1.1"}
	}
	tapMAC, err := ethernet.MACFromString(cfg.TapMAC)
	if err != nil {
		return nil, err
	}
	pool, err := addr.New(cfg.Network, cfg.Gateway)
	if err != nil {
		return nil, err
	}
	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return nil, fmt.Errorf("listen %s: %w", cfg.Listen, err)
	}
	return &Server{
		cfg:      cfg,
		pool:     pool,
		sw:       ethswitch.New(),
		tapMAC:   tapMAC,
		ln:       ln,
		logger:   log.Default(),
		sessions: make(map[string]*session),
		stopCh:   make(chan struct{}),
		clientMgr: NewClientManager(nil), // 稍后手动设置
	}, nil
}

// 等等... 发包太多，中途中断。继续写入。

func extractSSL(mux *http.ServeMux, addr string) {
	// ...
}

func (s *Server) Start() error {
	// ...
}

func (s *Server) handleConn(conn net.Conn) {
	// ...
}

func (s *Server) acceptLoop() {
	// ...
}

func (s *Server) tapLoop() {
	// ...
}

func (s *Server) tapWriter() {
	// ...
}

func (s *Server) Deliver(frame []byte, targets map[string]bool) {
	// ...
}

func (s *Server) allPorts() map[string]bool {
	// ...
}

func (s *Server) SendToClient(mac string, frame []byte) error {
	// ...
}

func (s *Server) CloseClient(mac string) error {
	// ...
}

func (s *Server) GetClients() []*ClientInfo {
	mu.RLock()
	defer mu.RUnlock()
	// ...
}

func mustJSON(v any) []byte {
	// ...
}
