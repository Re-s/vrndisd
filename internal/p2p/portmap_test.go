package p2p

import (
	"encoding/binary"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// 假 IGD：SSDP 由单独 UDP 套接字模拟（省略，仅测描述解析与 SOAP 层）。

// 解析描述文档时 serviceType 与 controlURL 下标对齐、相对路径拼接。
func TestIGDServicesParse(t *testing.T) {
	desc := `<?xml version="1.0"?>
<root xmlns="urn:schemas-upnp-org:device-1-0">
<device><serviceList>
<service>
<serviceType>urn:schemas-upnp-org:service:WANIPConnection:1</serviceType>
<controlURL>/ctl/IPConn</controlURL>
</service>
<service>
<serviceType>urn:schemas-upnp-org:service:Foo:1</serviceType>
<controlURL>/ctl/Foo</controlURL>
</service>
</serviceList></device>
</root>`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(desc))
	}))
	defer srv.Close()
	svcs := igdServices(srv.URL + "/rootDesc.xml")
	if len(svcs) != 1 {
		t.Fatalf("want 1 WANIP service, got %v", svcs)
	}
	if svcs[0].serviceURI != "urn:schemas-upnp-org:service:WANIPConnection:1" {
		t.Errorf("service type: %s", svcs[0].serviceURI)
	}
	// 相对 controlURL 必须拼接描述文档的 scheme://host
	if !strings.HasPrefix(svcs[0].controlURL, srv.URL) || !strings.HasSuffix(svcs[0].controlURL, "/ctl/IPConn") {
		t.Errorf("controlURL not joined: %s", svcs[0].controlURL)
	}
}

// SOAP AddPortMapping 契约：响应 200 返回请求端口；503 不映射。
func TestUPnPAddMapping(t *testing.T) {
	var gotSOAPAction, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotSOAPAction = r.Header.Get("SOAPAction")
		buf := make([]byte, 4096)
		n, _ := r.Body.Read(buf)
		gotBody = string(buf[:n])
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	svc := igdService{controlURL: srv.URL, serviceURI: "urn:schemas-upnp-org:service:WANIPConnection:1"}
	port := upnpAddMapping(svc, net.ParseIP("192.168.1.100"), 54321)
	if port != 54321 {
		t.Fatalf("want mapped port 54321, got %d", port)
	}
	if !strings.Contains(gotSOAPAction, "WANIPConnection:1#AddPortMapping") {
		t.Errorf("SOAPAction: %q", gotSOAPAction)
	}
	for _, want := range []string{"<NewProtocol>UDP</NewProtocol>", "<NewInternalPort>54321</NewInternalPort>", "<NewInternalClient>192.168.1.100</NewInternalClient>"} {
		if !strings.Contains(gotBody, want) {
			t.Errorf("envelope missing %q", want)
		}
	}

	// 失败响应不映射
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv2.Close()
	if port := upnpAddMapping(igdService{controlURL: srv2.URL, serviceURI: svc.serviceURI}, net.ParseIP("192.168.1.100"), 54321); port != 0 {
		t.Errorf("500 response must not map, got %d", port)
	}
}

// NAT-PMP 契约：假网关回映射成功响应（op=0x81, result=0），解析外部端口。
func TestPMPMap(t *testing.T) {
	var once sync.Once
	ready := make(chan struct{})
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer conn.Close()
	go func() {
		once.Do(func() { close(ready) })
		buf := make([]byte, 64)
		for {
			_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
			n, addr, err := conn.ReadFromUDP(buf)
			if err != nil {
				return
			}
			if n < 12 {
				continue
			}
			inner := binary.BigEndian.Uint16(buf[2:4])
			resp := make([]byte, 16)
			resp[0] = 0
			resp[1] = 0x81
			binary.BigEndian.PutUint16(resp[2:], 0) // result=0 成功
			binary.BigEndian.PutUint32(resp[4:], 12345)
			binary.BigEndian.PutUint16(resp[8:], inner)
			binary.BigEndian.PutUint16(resp[10:], 40000) // 网关分配的外部端口
			binary.BigEndian.PutUint32(resp[12:], pmpLifetime)
			_, _ = conn.WriteToUDP(resp, addr)
		}
	}()
	<-ready
	port := pmpMap(&net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: conn.LocalAddr().(*net.UDPAddr).Port}, 54321)
	if port != 40000 {
		t.Fatalf("want external port 40000, got %d", port)
	}
}

// PMP 错误响应（result=3 未授权）不得映射。
func TestPMPMapDenied(t *testing.T) {
	ready := make(chan struct{})
	var once sync.Once
	conn, _ := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	defer conn.Close()
	go func() {
		once.Do(func() { close(ready) })
		buf := make([]byte, 64)
		for {
			_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
			n, addr, err := conn.ReadFromUDP(buf)
			if err != nil {
				return
			}
			if n < 12 {
				continue
			}
			resp := make([]byte, 16)
			resp[1] = 0x81
			binary.BigEndian.PutUint16(resp[2:], 3) // 未授权
			_, _ = conn.WriteToUDP(resp, addr)
		}
	}()
	<-ready
	if port := pmpMap(&net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: conn.LocalAddr().(*net.UDPAddr).Port}, 54321); port != 0 {
		t.Fatalf("denied must not map, got %d", port)
	}
}

// guessGateway 推断网段 .1。
func TestGuessGateway(t *testing.T) {
	gw := guessGateway(net.ParseIP("192.168.1.100"))
	if !gw.Equal(net.ParseIP("192.168.1.1")) {
		t.Fatalf("want 192.168.1.1, got %v", gw)
	}
	if gw2 := guessGateway(net.ParseIP("10.77.0.5")); !gw2.Equal(net.ParseIP("10.77.0.1")) {
		t.Fatalf("want 10.77.0.1, got %v", gw2)
	}
	if gw3 := guessGateway(net.ParseIP("192.168.1.1")); !gw3.Equal(net.ParseIP("192.168.1.1")) {
		t.Fatalf("self .1: got %v", gw3)
	}
	if gw4 := guessGateway(net.ParseIP("::1")); gw4 != nil {
		t.Fatalf("v6 want nil, got %v", gw4)
	}
}

// SSDP LOCATION 头解析（大小写不敏感）。
func TestLocateHeader(t *testing.T) {
	resp := []byte("HTTP/1.1 200 OK\r\nCACHE-CONTROL: max-age=100\r\nLOCATION: http://192.168.1.1:1900/igd.xml\r\nST: urn:x\r\n\r\n")
	if got := locateHeader(resp); got != "http://192.168.1.1:1900/igd.xml" {
		t.Fatalf("got %q", got)
	}
	if got := locateHeader([]byte("HTTP/1.1 200 OK\r\n\r\n")); got != "" {
		t.Fatalf("want empty, got %q", got)
	}
	// 头名小写变体
	resp2 := []byte("HTTP/1.1 200 OK\r\nlocation: http://gw/root.xml\r\n\r\n")
	if got := locateHeader(resp2); got != "http://gw/root.xml" {
		t.Fatalf("lowercase got %q", got)
	}
}
