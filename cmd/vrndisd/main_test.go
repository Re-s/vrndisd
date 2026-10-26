package main

import (
	"flag"
	"reflect"
	"testing"
)

func TestExtractGlobalConfigBefore(t *testing.T) {
	rest, cfg := extractGlobalConfig([]string{"--config", "/tmp/x.conf", "server", "--listen", ":1"})
	if cfg != "/tmp/x.conf" {
		t.Fatalf("cfg=%q", cfg)
	}
	want := []string{"server", "--listen", ":1"}
	if !reflect.DeepEqual(rest, want) {
		t.Fatalf("rest=%v want %v", rest, want)
	}
}

func TestExtractGlobalConfigEquals(t *testing.T) {
	rest, cfg := extractGlobalConfig([]string{"--config=/a/b.conf", "version"})
	if cfg != "/a/b.conf" {
		t.Fatalf("cfg=%q", cfg)
	}
	if !reflect.DeepEqual(rest, []string{"version"}) {
		t.Fatalf("rest=%v", rest)
	}
}

func TestExtractGlobalConfigNoConfig(t *testing.T) {
	rest, cfg := extractGlobalConfig([]string{"client", "--server", "h:1"})
	if cfg != "" {
		t.Fatalf("cfg=%q want empty", cfg)
	}
	if !reflect.DeepEqual(rest, []string{"client", "--server", "h:1"}) {
		t.Fatalf("rest=%v", rest)
	}
}

func TestExtractGlobalConfigConfigTrailing(t *testing.T) {
	// --config 后面没有值：不应越界
	rest, cfg := extractGlobalConfig([]string{"--config"})
	if cfg != "" {
		t.Fatalf("cfg=%q", cfg)
	}
	if !reflect.DeepEqual(rest, []string{"--config"}) {
		t.Fatalf("rest=%v", rest)
	}
}

func TestSplitCSV(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"8.8.8.8", []string{"8.8.8.8"}},
		{"8.8.8.8,1.1.1.1", []string{"8.8.8.8", "1.1.1.1"}},
		{" a , b ,c ", []string{"a", "b", "c"}},
		{",a,,b,", []string{"a", "b"}},
	}
	for _, c := range cases {
		if got := splitCSV(c.in); !reflect.DeepEqual(got, c.want) {
			t.Errorf("splitCSV(%q)=%v want %v", c.in, got, c.want)
		}
	}
}

func TestDefaultConfigPathLinux(t *testing.T) {
	// 在 Linux 上固定返回 /etc/vrndisd.conf
	if got := defaultConfigPath(); got != "/etc/vrndisd.conf" {
		t.Fatalf("defaultConfigPath=%q", got)
	}
}

func TestParseLooseUnknownFlagNoPanic(t *testing.T) {
	// --help 等未注册 flag 不应因 fs.Lookup 返回 nil 而 panic
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.String("server", "", "")
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("parseLoose panicked: %v", r)
		}
	}()
	pos := parseLoose(fs, []string{"--help", "10.77.0.4"})
	if len(pos) != 2 || pos[0] != "--help" || pos[1] != "10.77.0.4" {
		t.Fatalf("positional=%v want [--help 10.77.0.4]", pos)
	}
}

func TestParseLoosePositionalBeforeFlags(t *testing.T) {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	server := fs.String("server", "", "")
	count := fs.Int("count", 5, "")
	pos := parseLoose(fs, []string{"10.77.0.4", "--count", "3"})
	if len(pos) != 1 || pos[0] != "10.77.0.4" {
		t.Fatalf("positional=%v want [10.77.0.4]", pos)
	}
	if *count != 3 {
		t.Errorf("count=%d want 3", *count)
	}
	if *server != "" {
		t.Errorf("server=%q want empty", *server)
	}
}

func TestIsHelpToken(t *testing.T) {
	for _, a := range []string{"-h", "--help"} {
		if !isHelpToken(a) {
			t.Errorf("isHelpToken(%q)=false want true", a)
		}
	}
	for _, a := range []string{"", "-x", "--count", "10.77.0.4"} {
		if isHelpToken(a) {
			t.Errorf("isHelpToken(%q)=true want false", a)
		}
	}
}

func TestCmdClientRejectsInvalidLogLevel(t *testing.T) {
	// 非法 --log-level 应在建连前报错（校验位于 client.New/Run 之前）。
	err := cmdClient([]string{"--server", "127.0.0.1:1", "--log-level", "trace"})
	if err == nil {
		t.Fatal("非法 log-level 应返回错误")
	}
}

func TestCmdServerRejectsInvalidLogLevel(t *testing.T) {
	err := cmdServer([]string{"--listen", "127.0.0.1:1", "--log-level", "verbose"})
	if err == nil {
		t.Fatal("非法 log-level 应返回错误")
	}
}
