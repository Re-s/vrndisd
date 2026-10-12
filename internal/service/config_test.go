package service

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"
	"time"
)

func TestDefaultConfigServer(t *testing.T) {
	c := DefaultConfig("server")
	if c.Mode != "server" || c.Listen != "0.0.0.0:7777" ||
		c.Network != "10.77.0.0/24" || c.Gateway != "10.77.0.1" ||
		c.Tap != "vrndis0" || c.MTU != 1500 {
		t.Fatalf("server default: %+v", c)
	}
	if len(c.DNS) != 2 {
		t.Fatalf("server dns: %v", c.DNS)
	}
	// 新增的服务器侧字段默认值
	if c.STUNListen != ":5478" {
		t.Fatalf("server stun listen default: %q", c.STUNListen)
	}
	if c.P2P || c.STUN {
		t.Fatalf("server p2p/stun should default off: %+v", c)
	}
}

func TestDefaultConfigClient(t *testing.T) {
	c := DefaultConfig("client")
	if c.Mode != "client" || c.Tap != "vrndis-c0" {
		t.Fatalf("client default: %+v", c)
	}
	// 客户端自愈参数默认值
	if !c.Reconnect {
		t.Fatal("client reconnect should default true")
	}
	if c.ReconnectBackoff != Duration(time.Second) {
		t.Fatalf("client reconnect backoff default: %v", c.ReconnectBackoff)
	}
}

func TestValidate(t *testing.T) {
	ok := DefaultConfig("server")
	if err := ok.Validate(); err != nil {
		t.Fatalf("valid server rejected: %v", err)
	}
	badServer := Config{Mode: "server", Network: "10.0.0.0/24", Gateway: "10.0.0.1"}
	if err := badServer.Validate(); err == nil {
		t.Fatal("server missing listen should fail")
	}
	badClient := Config{Mode: "client"}
	if err := badClient.Validate(); err == nil {
		t.Fatal("client missing server should fail")
	}
	badMode := Config{Mode: "bogus"}
	if err := badMode.Validate(); err == nil {
		t.Fatal("unknown mode should fail")
	}
	// 带全部新字段的 client 配置也应通过校验
	full := Config{
		Mode: "client", Server: "1.2.3.4:7777", P2P: true, STUNServer: "1.2.3.4:5478",
		Reconnect: true, MaxRetries: 3, ReconnectBackoff: Duration(2 * time.Second),
		HeartbeatInterval: Duration(10 * time.Second), HeartbeatTimeout: Duration(30 * time.Second),
	}
	if err := full.Validate(); err != nil {
		t.Fatalf("valid full client rejected: %v", err)
	}
}

func TestConfigSaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "vrndisd.conf")
	c := Config{
		Mode:    "server",
		Listen:  "0.0.0.0:9999",
		Network: "10.10.0.0/24",
		Gateway: "10.10.0.1",
		DNS:     []string{"1.1.1.1"},
		Tap:     "vrndis1",
		TapMAC:  "02:00:00:00:00:01",
		MTU:     1400,
	}
	if err := c.Save(path); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("config perm=%v want 0600", perm)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(*loaded, c) {
		t.Fatalf("roundtrip mismatch:\n got %+v\nwant %+v", *loaded, c)
	}
}

// TestConfigSaveLoadRoundTripAllFields 覆盖扩充后的全部字段（P2P/STUN/自愈参数），
// 确保序列化完整往返、Duration 字符串形式可还原。
func TestConfigSaveLoadRoundTripAllFields(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "vrndisd.conf")
	c := Config{
		Mode:         "client",
		Server:       "1.2.3.4:7777",
		Tap:          "vrndis-c0",
		Name:         "win-test",
		DefaultRoute: true,
		P2P:          true,
		STUNServer:   "1.2.3.4:5478",
		Reconnect:    true,
		MaxRetries:   5,
		// 显式非零时长，验证 "30s" 形式的序列化往返
		ReconnectBackoff:  Duration(2 * time.Second),
		HeartbeatInterval: Duration(10 * time.Second),
		HeartbeatTimeout:  Duration(30 * time.Second),
	}
	if err := c.Save(path); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(*loaded, c) {
		t.Fatalf("roundtrip mismatch:\n got %+v\nwant %+v", *loaded, c)
	}
}

// TestDurationJSON 验证 Duration 的字符串序列化与双形式反序列化（字符串/纳秒数字）。
func TestDurationJSON(t *testing.T) {
	cases := []time.Duration{0, time.Second, 30 * time.Second, time.Minute + 500*time.Millisecond}
	for _, d := range cases {
		b, err := json.Marshal(Duration(d))
		if err != nil {
			t.Fatal(err)
		}
		var got Duration
		if err := json.Unmarshal(b, &got); err != nil {
			t.Fatalf("unmarshal %s: %v", b, err)
		}
		if time.Duration(got) != d {
			t.Fatalf("duration roundtrip: got %v want %v", got, d)
		}
	}
	// 裸数字（纳秒）形式必须兼容
	var got Duration
	if err := json.Unmarshal([]byte("2500000000"), &got); err != nil ||
		time.Duration(got) != 2500*time.Millisecond {
		t.Fatalf("numeric duration: got %v err %v", got, err)
	}
	// 非法形式必须报错
	if err := json.Unmarshal([]byte(`"abc"`), &got); err == nil {
		t.Fatal("invalid duration string should fail")
	}
}

// TestConfigMarshalCompleteness 验证 MarshalJSON 产出包含全部可配置键。
func TestConfigMarshalCompleteness(t *testing.T) {
	c := DefaultConfig("server")
	c.P2P = true
	c.STUN = true
	b, err := json.Marshal(&c)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"mode", "listen", "network", "gateway", "dns", "tap", "tap_mac", "mtu",
		"stun_listen", "p2p", "stun",
	}
	for _, key := range want {
		if _, ok := m[key]; !ok {
			t.Fatalf("marshaled config missing key %q in %s", key, b)
		}
	}
}

func TestLoadBadFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.conf")
	if err := os.WriteFile(path, []byte("not json{{{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("bad json should fail")
	}
	if _, err := Load(filepath.Join(dir, "nope.conf")); err == nil {
		t.Fatal("missing file should fail")
	}
	if err := os.WriteFile(path, []byte(`{"mode":"nope"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("invalid mode should fail")
	}
}

// TestDefaultConfigPath 验证平台规范路径（配置写入位置的单一事实来源）。
func TestDefaultConfigPath(t *testing.T) {
	p := DefaultConfigPath()
	if p == "" {
		t.Fatal("empty config path")
	}
	if runtime.GOOS != "windows" && p != "/etc/vrndisd.conf" {
		t.Fatalf("linux config path: %q", p)
	}
	if runtime.GOOS == "windows" && filepath.Base(p) != "vrndisd.conf" {
		t.Fatalf("windows config path base: %q", p)
	}
}

func TestAbsExecutable(t *testing.T) {
	exe, err := absExecutable()
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(exe) {
		t.Fatalf("absExecutable returned non-abs: %q", exe)
	}
}

func TestServiceNameConstant(t *testing.T) {
	if ServiceName != "vrndisd" {
		t.Fatalf("ServiceName=%q", ServiceName)
	}
}
