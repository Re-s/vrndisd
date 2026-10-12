package proto

import (
	"bytes"
	"encoding/json"
	"testing"
)

// TestTypeNameAllTypes 覆盖 TypeName 所有已知分支。
func TestTypeNameAllTypes(t *testing.T) {
	cases := []struct {
		typ  byte
		want string
	}{
		{TypeHello, "HELLO"},
		{TypeAssign, "ASSIGN"},
		{TypeData, "DATA"},
		{TypeKeepalive, "KEEPALIVE"},
		{TypeBye, "BYE"},
		{TypePing, "PING"},
		{TypePong, "PONG"},
		{TypeManagement, "MANAGEMENT"},
		{99, "UNKNOWN(99)"},
	}
	for _, c := range cases {
		if got := TypeName(c.typ); got != c.want {
			t.Errorf("TypeName(%d)=%q, want %q", c.typ, got, c.want)
		}
	}
}

// TestClientInfoSerialize ClientInfo.Serialize 输出合法 JSON 且字段完整。
func TestClientInfoSerialize(t *testing.T) {
	ci := ClientInfo{IP: "10.77.0.2", MAC: "02:00:00:00:00:01", Peer: "p", Name: "n"}
	data := ci.Serialize()
	var back ClientInfo
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatalf("Serialize produced invalid JSON: %v", err)
	}
	if back.IP != ci.IP || back.MAC != ci.MAC || back.Name != ci.Name || back.Peer != ci.Peer {
		t.Fatalf("roundtrip mismatch: %+v vs %+v", back, ci)
	}
}

// TestManagementResponseRoundTrip ManagementResponse 经 WriteJSON/ReadJSON 往返。
func TestManagementResponseRoundTrip(t *testing.T) {
	in := ManagementResponse{
		Version: "1.0",
		Clients: 3,
		List: []ClientInfo{
			{IP: "10.0.0.1", MAC: "aa:bb:cc:dd:ee:ff", Name: "c1"},
			{IP: "10.0.0.2", MAC: "aa:bb:cc:dd:ee:01", Name: "c2"},
		},
	}
	var buf bytes.Buffer
	if err := WriteJSON(&buf, TypeManagement, in); err != nil {
		t.Fatal(err)
	}
	var out ManagementResponse
	typ, err := ReadJSON(&buf, &out)
	if err != nil {
		t.Fatal(err)
	}
	if typ != TypeManagement {
		t.Fatalf("typ=%d want %d", typ, TypeManagement)
	}
	if out.Version != in.Version || out.Clients != in.Clients || len(out.List) != 2 {
		t.Fatalf("mismatch: %+v vs %+v", out, in)
	}
	if out.List[1].IP != "10.0.0.2" {
		t.Fatalf("list roundtrip failed: %+v", out.List)
	}
}

// TestReadJSONBadBody ReadJSON 遇到非法 JSON body 返回 ErrBadMessage。
func TestReadJSONBadBody(t *testing.T) {
	var buf bytes.Buffer
	WriteMessage(&buf, TypeAssign, []byte("{not json"))
	if _, err := ReadJSON(&buf, new(Assignment)); err == nil {
		t.Fatal("want error on bad JSON body")
	}
}

// TestWriteMessageEmptyBody 空 body 消息往返（TypeKeepalive）。
func TestWriteMessageEmptyBody(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteMessage(&buf, TypeKeepalive, nil); err != nil {
		t.Fatal(err)
	}
	msg, err := ReadMessage(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if msg.Type != TypeKeepalive || len(msg.Body) != 0 {
		t.Fatalf("got type=%d body=%d bytes", msg.Type, len(msg.Body))
	}
}
