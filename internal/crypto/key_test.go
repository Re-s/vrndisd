package crypto

import (
	"crypto/rand"
	"testing"
)

func TestKeySymmetry(t *testing.T) {
	c, err := NewKeyPair(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	s, err := NewKeyPair(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	psk := []byte("my-secret-psk-1234567890123456")

	cs, cMac, err := c.PSKHandshake(s.Public(), psk, c.Public(), true)
	if err != nil {
		t.Fatal(err)
	}
	ss, sMac, err := s.PSKHandshake(c.Public(), psk, s.Public(), false)
	if err != nil {
		t.Fatal(err)
	}

	if cs[0] != ss[1] {
		t.Fatalf("client write != server read: %x vs %x", cs[0], ss[1])
	}
	if cs[1] != ss[0] {
		t.Fatalf("client read != server write: %x vs %x", cs[1], ss[0])
	}
	if cs[0] == cs[1] {
		t.Fatal("same direction keys")
	}
	if len(cMac) != macLen || len(sMac) != macLen {
		t.Fatalf("mac length wrong: %d %d", len(cMac), len(sMac))
	}
}

func TestKeyUniqueness(t *testing.T) {
	a, _ := NewKeyPair(rand.Reader)
	b, _ := NewKeyPair(rand.Reader)
	if string(a.Public()) == string(b.Public()) {
		t.Fatal("public keys should differ")
	}
}

func TestPublicKeySize(t *testing.T) {
	k, _ := NewKeyPair(rand.Reader)
	if len(k.Public()) != 32 {
		t.Fatalf("public key len %d != 32", len(k.Public()))
	}
}

func TestBadPublicKey(t *testing.T) {
	k, _ := NewKeyPair(rand.Reader)
	if _, _, err := k.PSKHandshake([]byte("short"), []byte("psk"), k.Public(), true); err == nil {
		t.Fatal("expected error for short peer key")
	}
}
