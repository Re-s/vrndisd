// Package client 实现虚拟 RNDIS 客户端。
//
// 连接服务器（TCP）→ 收 ASSIGN（服务器分配的地址）→ 配置本地虚拟网卡 →
// 双向搬运以太网帧。对操作系统而言本地网卡就是一块"插上就联网"的网卡。
package client

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"sync"
	"time"

	"vrndis/internal/ethernet"
	"vrndis/internal/proto"
	"vrndis/internal/tap"
)

// Config 客户端配置。
type Config struct {
	Server       string        // 服务器地址 host:port
	TapName      string        // 本地虚拟网卡名
	Name         string        // 客户端名称（日志用途）
	DefaultRoute bool          // 把系统默认路由指向虚拟网卡
	DialTimeout  time.Duration // 建连超时
	Device       tap.Device    // 可选：注入设备（测试用）
}

// Client 虚拟 RNDIS 客户端。
type Client struct {
	cfg    Config
	conn   net.Conn
	dev    tap.Device
	logger *log.Logger

	assignMu sync.RWMutex
	assign   *proto.Assignment

	writeMu   sync.Mutex
	stopCh    chan struct{}
	closeOnce sync.Once
	wg        sync.WaitGroup
}

// New 创建客户端（未连接）。
func New(cfg Config) *Client {
	if cfg.TapName == "" {
		cfg.TapName = "vrndis-c0"
	}
	if cfg.DialTimeout == 0 {
		cfg.DialTimeout = 10 * time.Second
	}
	return &Client{cfg: cfg, logger: log.Default(), stopCh: make(chan struct{})}
}

// Connect 建立连接、接收地址分配、配置本地网卡并启动搬运协程。
func (c *Client) Connect() (*proto.Assignment, error) {
	conn, err := net.DialTimeout("tcp", c.cfg.Server, c.cfg.DialTimeout)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", c.cfg.Server, err)
	}
	if tc, ok := conn.(*net.TCPConn); ok {
		tc.SetNoDelay(true)
	}
	helloBody, _ := json.Marshal(proto.Hello{Version: proto.ProtocolVersion, Name: c.cfg.Name})
	if err := proto.WriteMessage(conn, proto.TypeHello, helloBody); err != nil {
		conn.Close()
		return nil, err
	}
	msg, err := proto.ReadMessage(conn)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("read assignment: %w", err)
	}
	if msg.Type != proto.TypeAssign {
		conn.Close()
		return nil, fmt.Errorf("期望 ASSIGN，收到 %s", proto.TypeName(msg.Type))
	}
	var assign proto.Assignment
	if err := json.Unmarshal(msg.Body, &assign); err != nil {
		conn.Close()
		return nil, fmt.Errorf("解析 ASSIGN 失败: %w", err)
	}

	dev := c.cfg.Device
	if dev == nil {
		opened, err := tap.Open(c.cfg.TapName)
		if err != nil {
			conn.Close()
			return nil, err
		}
		dev = opened
	}
	mac, err := ethernet.MACFromString(assign.MAC)
	if err != nil {
		conn.Close()
		return nil, err
	}
	ip := net.ParseIP(assign.IP).To4()
	if ip == nil {
		conn.Close()
		return nil, fmt.Errorf("ASSIGN 中的 IP 无效: %q", assign.IP)
	}
	if err := dev.SetMAC(mac); err != nil {
		conn.Close()
		return nil, fmt.Errorf("set mac: %w", err)
	}
	if err := dev.SetIPv4(ip, assign.Prefix); err != nil {
		conn.Close()
		return nil, fmt.Errorf("set ipv4: %w", err)
	}
	if err := dev.Up(); err != nil {
		conn.Close()
		return nil, fmt.Errorf("tap up: %w", err)
	}
	if c.cfg.DefaultRoute {
		gw := net.ParseIP(assign.Gateway)
		if err := dev.AddDefaultRoute(gw); err != nil {
			c.logger.Printf("vrndis client: 设置默认路由失败（继续运行）: %v", err)
		}
	}

	c.conn = conn
	c.dev = dev
	c.assignMu.Lock()
	c.assign = &assign
	c.assignMu.Unlock()

	c.wg.Add(3)
	go c.sockToDev()
	go c.devToSock()
	go c.managementLoop()

	c.logger.Printf("vrndis client: 已连接 %s，本机地址 %s/%d 网关 %s",
		c.cfg.Server, assign.IP, assign.Prefix, assign.Gateway)
	return &assign, nil
}

// Assignment 当前分配结果（未连接时为 nil）。
func (c *Client) Assignment() *proto.Assignment {
	c.assignMu.RLock()
	defer c.assignMu.RUnlock()
	return c.assign
}

// Run 连接并运行直到 ctx 取消或连接断开。
func (c *Client) Run(ctx context.Context) error {
	if _, err := c.Connect(); err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		c.Stop()
		return ctx.Err()
	case <-c.stopCh:
		return nil
	}
}

// Stop 优雅下线并回收资源。可重复调用、可从任意错误路径调用。
func (c *Client) Stop() {
	// 尽力而为发 BYE（带写超时，避免慢对端卡住退出）
	if c.conn != nil {
		_ = c.conn.SetWriteDeadline(time.Now().Add(500 * time.Millisecond))
		c.writeMu.Lock()
		_ = proto.WriteMessage(c.conn, proto.TypeBye, nil)
		c.writeMu.Unlock()
	}
	c.teardown()
	c.wg.Wait()
}

// teardown 幂等回收：关闭连接与网卡、发出关闭信号。
// 关键：所有错误路径都走这里，绝不会"吞掉"资源清理。
func (c *Client) teardown() {
	c.closeOnce.Do(func() {
		close(c.stopCh)
		if c.conn != nil {
			c.conn.Close()
		}
		if c.dev != nil {
			c.dev.Close()
		}
	})
}

// Closed 连接是否已终止（断线检测）。
func (c *Client) Closed() bool {
	select {
	case <-c.stopCh:
		return true
	default:
		return false
	}
}

// ---- 内部 ----

func (c *Client) send(typ byte, body []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return proto.WriteMessage(c.conn, typ, body)
}

func (c *Client) sockToDev() {
	defer c.wg.Done()
	for {
		msg, err := proto.ReadMessage(c.conn)
		if err != nil {
			c.logger.Printf("vrndis client: 连接关闭: %v", err)
			c.teardown()
			return
		}
		switch msg.Type {
		case proto.TypeData:
			if err := c.dev.Write(msg.Body); err != nil {
				c.logger.Printf("vrndis client: 写入本地网卡失败: %v", err)
				c.teardown()
				return
			}
		case proto.TypeBye:
			c.teardown()
			return
		case proto.TypePing:
			// 响应 PING
			c.send(proto.TypePong, nil)
		case proto.TypeManagement:
			// 处理管理消息，当前忽略
		}
	}
}

func (c *Client) devToSock() {
	defer c.wg.Done()
	for {
		frame, err := c.dev.Read()
		if err != nil {
			select {
			case <-c.stopCh:
			default:
				c.logger.Printf("vrndis client: 读取本地网卡失败: %v", err)
			}
			c.teardown()
			return
		}
		c.writeMu.Lock()
		err = proto.WriteMessage(c.conn, proto.TypeData, frame)
		c.writeMu.Unlock()
		if err != nil {
			c.logger.Printf("vrndis client: 发送失败，连接已断开: %v", err)
			c.teardown()
			return
		}
	}
}

// managementLoop 当前未启用，保留处理位置
func (c *Client) managementLoop() {
	defer c.wg.Done()
	// 发送 PING 响应由 sockToDev 处理
}
