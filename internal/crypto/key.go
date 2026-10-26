// Package crypto 为 vrndis 线路提供加密能力。
//
// 安全模型：PSK + 一次性 ECDH（WireGuard 模式）
//
//   - PSK（预共享密钥）仅用于**对端身份认证**：中间人不知道 PSK，
//     无法伪造握手响应，也无法完成密钥派生。
//   - 一次性 ECDH（ephemeral X25519）提供**前向安全**：即使 PSK 泄露，
//     历史会话的流量也无法被解密，因为每条线路的密钥对只用一次即丢弃。
//   - 二者缺一不可：无 PSK = 无身份认证 = 可 MITM；无 ECDH = 无前向安全。
//
// 握手流程（v3 协议）：
//
//  1. 客户端 -> 服务器：E_KC  (客户端临时 X25519 公钥, 32B)
//  2. 服务器 -> 客户端：E_KS || HMAC-PSK(E_KC || E_KS)
//     - 服务器用 PSK 对 (E_KC || E_KS) 做 HMAC；客户端独立计算并比对，
//     比对失败则拒绝连接（对端不是合法服务器）。
//  3. 双方计算 shared = X25519(priv, peerPublic)
//  4. prk   = HMAC-PSK(shared)                   (PSK 作 salt)
//     keys   = HKDF-Expand(prk, info="vrndis-v3" || E_KC || E_KS, 64B)
//     两端派生出的 64 字节完全一致。
//  5. 方向语义：客户端取 [0:32] 为写密钥、[32:64] 为读密钥；
//     服务器取 [32:64] 为写密钥、[0:32] 为读密钥。
//     于是 客户端写 == 服务器读，客户端读 == 服务器写。
//  6. 后续所有 proto.Message 经 AEAD 加密后发送
//
// 注意：本包只负责密钥派生。握手消息的发送/接收由 caller（client/server）
// 负责，调用方在验证 HMAC 后才应继续数据面。
package crypto

import (
	"crypto/ecdh"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/sha256"
	"errors"
	"io"
)

// domain 握手 HKDF 的 info 前缀，隔离不同版本/用途的密钥。
const domain = "vrndis-v3/key-exchange"

// macLen PSK-HMAC 截断长度。完整 HMAC-SHA256 为 32 字节，截断到 16 字节
// 仍提供 2^128 的安全性（远超暴力破解），同时节省握手带宽。
const macLen = 16

// KeyPair 一条线路的临时 X25519 密钥对。
type KeyPair struct {
	priv *ecdh.PrivateKey
	pub  *ecdh.PublicKey
}

// NewKeyPair 生成一次性密钥对。
func NewKeyPair(rand io.Reader) (*KeyPair, error) {
	priv, err := ecdh.X25519().GenerateKey(rand)
	if err != nil {
		return nil, err
	}
	pub, ok := priv.Public().(*ecdh.PublicKey)
	if !ok {
		return nil, errNotECDH
	}
	return &KeyPair{priv: priv, pub: pub}, nil
}

// Public 原始公钥字节（32 字节压缩点）。
func (k *KeyPair) Public() []byte { return k.pub.Bytes() }

// PSKHandshake 完成一次 PSK 认证的密钥派生。
//
// peerPublic 是对端临时公钥（32 字节 E_KC 或 E_KS）。
// psk 是预共享密钥，由管理员在服务器与所有客户端之间预先分发。
// selfPublic 是本端临时公钥（32 字节）。
// clientFirst 表明本端是否是"先发送公钥的一方"（即客户端）。
//
// 返回 [2][32]byte：第一个为"该端写出（write）"密钥，第二个为"该端读入（read）"
// 密钥。调用约定：
//   - 客户端：PSKHandshake(serverPub, psk, clientPub, true) -> [ 客户端写, 客户端读 ]
//   - 服务器：PSKHandshake(clientPub, psk, serverPub, false) -> [ 服务器写, 服务器读 ]
//
// 因此客户端写密钥 == 服务器读密钥，反之亦然——方向语义由交换顺序自然保证。
//
// 验证 MAC：握手阶段，发起方（客户端）收到 E_KS 后应自行计算
// HMAC-PSK(E_KC || E_KS) 并与收到的 MAC 比对；本函数返回的 mac 是**本端
// 期望的值**，调用方用它与对端发来的 MAC 做恒时比较（hmac.Equal）。
func (k *KeyPair) PSKHandshake(peerPublic, psk, selfPublic []byte, clientFirst bool) ([2][32]byte, []byte, error) {
	var keys [2][32]byte

	pub, err := ecdh.X25519().NewPublicKey(peerPublic)
	if err != nil {
		return keys, nil, err
	}
	shared, err := k.priv.ECDH(pub)
	if err != nil {
		return keys, nil, err
	}
	if len(shared) != 32 {
		return keys, nil, errSharedSecretSize
	}

	// info 中 E_KC 始终在前、E_KS 始终在后，保证两端派生一致
	var clientPub, serverPub []byte
	if clientFirst {
		clientPub, serverPub = selfPublic, peerPublic
	} else {
		clientPub, serverPub = peerPublic, selfPublic
	}
	if len(clientPub) != 32 || len(serverPub) != 32 {
		return keys, nil, errBadPublicKey
	}

	// info = E_KC || E_KS || domain（86 字节），直接字符串拼接避免截断
	info := string(clientPub) + string(serverPub) + domain

	// HMAC 输入：E_KC || E_KS（顺序与 info 一致）
	macInput := append([]byte(nil), clientPub...)
	macInput = append(macInput, serverPub...)
	expectedMac := hmacSHA256(psk, macInput)

	// HKDF：以 shared 为 IKM，PSK 为 salt 提取 PRK，再展开 64 字节
	prk := hmacSHA256(psk, shared)
	// hkdf.Expand 返回 io.Reader，读满 64 字节即可。
	keysBytes, err := hkdf.Expand(sha256.New, prk, info, 64)
	if err != nil {
		return keys, nil, err
	}
	// clientFirst==true（客户端）：keys[0]=写密钥，keys[1]=读密钥
	// clientFirst==false（服务器）：对端是"先发者"，其写密钥即本端读密钥，
	// 因此需要反转，保证：客户端写密钥 == 服务器读密钥。
	if clientFirst {
		copy(keys[0][:], keysBytes[:32])
		copy(keys[1][:], keysBytes[32:])
	} else {
		copy(keys[0][:], keysBytes[32:])
		copy(keys[1][:], keysBytes[:32])
	}

	return keys, expectedMac[:macLen], nil
}

func hmacSHA256(key, data []byte) []byte {
	m := hmac.New(sha256.New, key)
	m.Write(data)
	return m.Sum(nil)
}

var (
	errSharedSecretSize = errors.New("github.com/Re-s/vrndisd/crypto: shared secret not 32 bytes")
	errNotECDH          = errors.New("github.com/Re-s/vrndisd/crypto: public key is not an ECDH key")
	errBadPublicKey     = errors.New("github.com/Re-s/vrndisd/crypto: public key must be 32 bytes")
)
