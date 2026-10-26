package server

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Re-s/vrndisd/internal/addr"
)

func mustPool(t *testing.T) *addr.Pool {
	t.Helper()
	p, err := addr.New("10.77.0.0/24", "10.77.0.1")
	if err != nil {
		t.Fatalf("New pool: %v", err)
	}
	return p
}

func TestLeaseBookRoundtrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "leases.json")
	b := loadLeaseBook(path)
	p := mustPool(t)

	// 无记录
	if _, _, ok := b.lookup("devA"); ok {
		t.Fatal("空簿不应命中")
	}
	// 登记 + 落盘 + 重新载入
	l, err := p.Allocate()
	if err != nil {
		t.Fatal(err)
	}
	b.record("devA", l)
	b2 := loadLeaseBook(path)
	ip, mac, ok := b2.lookup("devA")
	if !ok || !ip.Equal(l.IP) || mac.String() != l.MAC.String() {
		t.Fatalf("重载后记录不一致: ip=%v mac=%v ok=%v", ip, mac, ok)
	}
	// 文件存在且非空
	if fi, err := os.Stat(path); err != nil || fi.Size() == 0 {
		t.Fatalf("持久化文件异常: %v", err)
	}
}

func TestLeaseBookCorruptIgnored(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "leases.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	b := loadLeaseBook(path)
	if _, _, ok := b.lookup("devA"); ok {
		t.Fatal("损坏文件应视为空簿")
	}
	// 登记后仍能覆盖损坏文件
	p := mustPool(t)
	l, _ := p.Allocate()
	b.record("devA", l)
	b2 := loadLeaseBook(path)
	if _, _, ok := b2.lookup("devA"); !ok {
		t.Fatal("覆盖写入后应能读到")
	}
}

func TestLeaseBookNestedDirCreated(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "sub", "leases.json")
	b := loadLeaseBook(path)
	p := mustPool(t)
	l, _ := p.Allocate()
	b.record("devA", l)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("嵌套目录未创建: %v", err)
	}
}

// 空路径 = 禁用持久化，但内存簿仍可用（同进程断线重连复用）。
func TestLeaseBookEmptyPath(t *testing.T) {
	b := loadLeaseBook("")
	p := mustPool(t)
	l, _ := p.Allocate()
	b.record("devA", l)
	ip, mac, ok := b.lookup("devA")
	if !ok || !ip.Equal(l.IP) || mac.String() != l.MAC.String() {
		t.Fatal("内存簿应命中")
	}
}
