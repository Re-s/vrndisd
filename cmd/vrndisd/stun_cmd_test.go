package main

import (
	"bytes"
	"os"
	"reflect"
	"testing"
)

func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stdout = w
	fn()
	w.Close()
	os.Stdout = old
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(r); err != nil {
		t.Fatalf("read: %v", err)
	}
	return buf.String()
}

func TestCmdStunServerVersion(t *testing.T) {
	out := captureStdout(t, func() {
		if err := cmdStunServer([]string{"--version"}); err != nil {
			t.Errorf("cmdStunServer --version err=%v", err)
		}
	})
	if !bytes.Contains([]byte(out), []byte("vrndisd stun-server")) {
		t.Fatalf("stdout=%q", out)
	}
	if !bytes.Contains([]byte(out), []byte(Version)) {
		t.Fatalf("stdout missing version: %q", out)
	}
}

func TestCmdStunServerFlagParse(t *testing.T) {
	out := captureStdout(t, func() {
		if err := cmdStunServer([]string{"--listen", "127.0.0.1:13478", "--version"}); err != nil {
			t.Errorf("err=%v", err)
		}
	})
	if !bytes.Contains([]byte(out), []byte("vrndisd stun-server")) {
		t.Fatalf("stdout=%q", out)
	}
}

func TestExtractGlobalConfigStunServer(t *testing.T) {
	rest, cfg := extractGlobalConfig([]string{"--config", "/etc/vrndisd.conf", "stun-server", "--listen", ":13478"})
	if cfg != "/etc/vrndisd.conf" {
		t.Fatalf("cfg=%q", cfg)
	}
	want := []string{"stun-server", "--listen", ":13478"}
	if !reflect.DeepEqual(rest, want) {
		t.Fatalf("rest=%v want %v", rest, want)
	}
}
