package main

import (
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
