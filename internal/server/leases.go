package server

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"sync"

	"github.com/Re-s/vrndisd/internal/addr"
)

// leaseEntry 一条按客户端 name 持久化的租约记录。
type leaseEntry struct {
	IP  string `json:"ip"`
	MAC string `json:"mac"`
}

// leaseBook 按客户端 name 记录其上一次使用的 IP/MAC，并落盘持久化。
//
// 缺陷 A 的根因修复：地址池按连接顺序分配 IP/MAC，服务器重启后各客户端
// 重连顺序随机，MAC 随之重排（如 win-pub/hk100m 的 ...02/...03 互换）；
// 而客户端的 Tunnel.routes 以 MAC 为键且跨重启存活，旧键与新 MAC 冲突，
// 只能重启客户端清表。按 name 粘滞后，同一客户端无论第几个重连都拿回
// 原 IP/MAC，ARP 与路由键全部稳定。
//
// 断开不移除记录：持久化的意义正是跨服务器重启保留。记录的 IP 被他人
// 占用（同名冲突或地址已被分配）时回退顺序分配，并更新记录指向新租约，
// 保证记录收敛到该 name 的当前实际租约。
type leaseBook struct {
	mu      sync.Mutex
	path    string
	entries map[string]leaseEntry
}

// loadLeaseBook 从 path 载入租约簿；path 为空时返回仅内存的禁用实例，
// 文件不存在或损坏时从空记录开始（不阻断启动）。
func loadLeaseBook(path string) *leaseBook {
	b := &leaseBook{path: path, entries: make(map[string]leaseEntry)}
	if path == "" {
		return b
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return b // 首次启动无文件
	}
	_ = json.Unmarshal(data, &b.entries)
	return b
}

// lookup 查询 name 上次的 IP/MAC；记录缺失或损坏返回 false。
func (b *leaseBook) lookup(name string) (net.IP, net.HardwareAddr, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	e, ok := b.entries[name]
	if !ok {
		return nil, nil, false
	}
	ip := net.ParseIP(e.IP).To4()
	mac, err := net.ParseMAC(e.MAC)
	if ip == nil || err != nil {
		return nil, nil, false
	}
	return ip, mac, true
}

// record 登记 name 当前租约并持久化（path 为空时仅更新内存，
// 仍支持同进程内断线重连复用）。持久化失败仅告警，不影响分配。
func (b *leaseBook) record(name string, lease *addr.Lease) {
	b.mu.Lock()
	b.entries[name] = leaseEntry{IP: lease.IP.String(), MAC: lease.MAC.String()}
	path := b.path
	b.mu.Unlock()
	if path != "" {
		b.persist(path)
	}
}

// persist 原子写盘：临时文件 + rename，避免崩溃截断。
func (b *leaseBook) persist(path string) {
	b.mu.Lock()
	data, err := json.MarshalIndent(b.entries, "", "  ")
	b.mu.Unlock()
	if err != nil {
		return
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	tmp, err := os.CreateTemp(dir, ".leases-*.json")
	if err != nil {
		return
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return
	}
	if err := tmp.Close(); err != nil {
		return
	}
	if err := os.Chmod(tmp.Name(), 0o600); err != nil {
		return
	}
	_ = os.Rename(tmp.Name(), path)
}
