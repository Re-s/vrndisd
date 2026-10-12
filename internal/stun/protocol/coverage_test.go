package protocol

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func TestParseShortRejected(t *testing.T) {
	for _, n := range []int{0, 1, 19} {
		if _, err := Parse(make([]byte, n)); err == nil {
			t.Fatalf("Parse(%d bytes) should error", n)
		}
	}
}

func TestParseFields(t *testing.T) {
	data := make([]byte, 20)
	binary.BigEndian.PutUint16(data[0:], uint16(BindingRequest))
	binary.BigEndian.PutUint16(data[2:], 8)
	binary.BigEndian.PutUint32(data[4:], MagicCookie)
	for i := 0; i < 12; i++ {
		data[8+i] = byte(i)
	}
	m, err := Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	if m.Type != BindingRequest {
		t.Fatalf("Type=%d", m.Type)
	}
	if m.Length != 8 {
		t.Fatalf("Length=%d", m.Length)
	}
	if m.MagicCookie != MagicCookie {
		t.Fatalf("cookie=%x", m.MagicCookie)
	}
	if !bytes.Equal(m.TransactionID[:], data[8:20]) {
		t.Fatal("txid mismatch")
	}
	if m.Attributes == nil {
		t.Fatal("attributes map should be allocated")
	}
}

func TestTypeNameAll(t *testing.T) {
	cases := []struct {
		t    MsgType
		want string
	}{
		{BindingRequest, "BIND_REQUEST"},
		{BindingResponse, "BIND_RESPONSE"},
		{BindingError, "BIND_ERROR"},
		{MsgType(0x9999), "?UNKNOWN"},
	}
	for _, c := range cases {
		if got := TypeName(c.t); got != c.want {
			t.Errorf("TypeName(%x)=%q want %q", c.t, got, c.want)
		}
	}
}
