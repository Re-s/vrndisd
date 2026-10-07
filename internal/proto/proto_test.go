package proto

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"testing"
)

func TestMessageRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteMessage(&buf, TypeData, []byte("hello")); err != nil {
		t.Fatal(err)
	}
	msg, err := ReadMessage(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if msg.Type != TypeData || string(msg.Body) != "hello" {
		t.Fatalf("got %d %q", msg.Type, msg.Body)
	}
}

func TestJSONRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	in := Assignment{IP: "10.77.0.2", Prefix: 24, MAC: "02:00:00:00:00:01", DNS: []string{"8.8.8.8"}}
	if err := WriteJSON(&buf, TypeAssign, in); err != nil {
		t.Fatal(err)
	}
	var out Assignment
	typ, err := ReadJSON(&buf, &out)
	if err != nil || typ != TypeAssign {
		t.Fatalf("typ=%d err=%v", typ, err)
	}
	if out.IP != in.IP || out.Prefix != in.Prefix || out.MAC != in.MAC {
		t.Fatalf("mismatch: %+v vs %+v", out, in)
	}
}

func TestBackToBackMessages(t *testing.T) {
	var buf bytes.Buffer
	WriteMessage(&buf, TypeData, []byte("one"))
	WriteMessage(&buf, TypeKeepalive, nil)
	WriteMessage(&buf, TypeData, []byte("two"))
	for _, want := range []string{"one", "", "two"} {
		msg, err := ReadMessage(&buf)
		if err != nil {
			t.Fatal(err)
		}
		if string(msg.Body) != want {
			t.Fatalf("want %q got %q", want, msg.Body)
		}
	}
}

func TestOversizedWriteRejected(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteMessage(&buf, TypeData, make([]byte, MaxBody+1)); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("want ErrTooLarge, got %v", err)
	}
}

func TestOversizedHeaderRejected(t *testing.T) {
	var buf bytes.Buffer
	hdr := make([]byte, 5)
	binary.BigEndian.PutUint32(hdr, MaxBody+1)
	hdr[4] = TypeData
	buf.Write(hdr)
	if _, err := ReadMessage(&buf); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("want ErrTooLarge, got %v", err)
	}
}

func TestEOFAndPartialData(t *testing.T) {
	if _, err := ReadMessage(bytes.NewReader(nil)); !errors.Is(err, io.EOF) {
		t.Fatalf("want EOF, got %v", err)
	}
	// 只有一半 header
	if _, err := ReadMessage(bytes.NewReader([]byte{0, 0, 0})); err == nil {
		t.Fatal("want error on partial header")
	}
	// header 说有 body 但对端断开
	hdr := []byte{0, 0, 0, 5, TypeData}
	if _, err := ReadMessage(bytes.NewReader(hdr)); err == nil {
		t.Fatal("want error on truncated body")
	}
}

func TestUnknownTypeTolerated(t *testing.T) {
	var buf bytes.Buffer
	WriteMessage(&buf, 200, []byte("{}"))
	msg, err := ReadMessage(&buf)
	if err != nil || msg.Type != 200 {
		t.Fatalf("unknown type should parse: %v", err)
	}
	if got := TypeName(200); got == "" {
		t.Fatal("TypeName should describe unknown types")
	}
}

// FuzzReadMessage 模糊测试：任意字节流不得 panic，只能返回错误或合法消息。
func FuzzReadMessage(f *testing.F) {
	f.Add([]byte{0, 0, 0, 1, TypeData, 'x'})
	f.Add([]byte{})
	f.Add(bytes.Repeat([]byte{0xff}, 8))
	f.Fuzz(func(t *testing.T, data []byte) {
		msg, err := ReadMessage(bytes.NewReader(data))
		if err == nil && len(msg.Body) > MaxBody {
			t.Fatalf("accepted oversized body: %d", len(msg.Body))
		}
	})
}
