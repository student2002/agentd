// crypto.go 实现 RSA 非对称加密功能，用于 Git 凭据的安全传输。
//
// 本文件提供 Agent Daemon 与 Server 之间的安全凭据传输机制，主要包括：
//   - GenerateRSAKeyPair：生成 2048 位 RSA 密钥对（PEM 编码），公钥发给服务端加密凭据
//   - DecryptWithPrivateKey：使用私钥解密服务端 RSA-OAEP 加密的 Git 凭据（PAT）
//
// 加密流程：Server 使用 Agent 的公钥加密 Git PAT → Agent 使用本地私钥解密 → 注入 Git askpass 脚本。
package agent

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"fmt"
)

// GenerateRSAKeyPair 生成 2048 位 RSA 密钥对，返回 PEM 编码的公钥和私钥。
// 公钥用于发送给服务端加密 Git 凭据，私钥在本地用于解密。
//
// 返回：
//   - publicKeyPEM: PEM 编码的公钥
//   - privateKeyPEM: PEM 编码的私钥
//   - error: 生成失败时返回错误
func GenerateRSAKeyPair() (publicKeyPEM string, privateKeyPEM string, err error) {
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return "", "", fmt.Errorf("generate RSA key: %w", err)
	}

	// 将私钥编码为 PEM 格式
	privBytes := x509.MarshalPKCS1PrivateKey(privateKey)
	privPEM := pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: privBytes,
	})

	// 将公钥编码为 PEM 格式
	pubBytes, err := x509.MarshalPKIXPublicKey(&privateKey.PublicKey)
	if err != nil {
		return "", "", fmt.Errorf("marshal public key: %w", err)
	}
	pubPEM := pem.EncodeToMemory(&pem.Block{
		Type:  "PUBLIC KEY",
		Bytes: pubBytes,
	})

	return string(pubPEM), string(privPEM), nil
}

// DecryptWithPrivateKey 使用给定的私钥 PEM 解密 base64 编码的 RSA-OAEP 密文。
// 用于解密服务端加密的 Git 凭据（个人访问令牌）。
//
// 参数：
//   - privateKeyPEM: PEM 编码的 RSA 私钥
//   - ciphertextBase64: base64 编码的密文
//
// 返回：
//   - string: 解密后的明文
//   - error: 解密失败时返回错误
func DecryptWithPrivateKey(privateKeyPEM string, ciphertextBase64 string) (string, error) {
	block, _ := pem.Decode([]byte(privateKeyPEM))
	if block == nil {
		return "", fmt.Errorf("failed to decode PEM block")
	}

	privateKey, err := x509.ParsePKCS1PrivateKey(block.Bytes)
	if err != nil {
		return "", fmt.Errorf("parse private key: %w", err)
	}

	ciphertext, err := base64.StdEncoding.DecodeString(ciphertextBase64)
	if err != nil {
		return "", fmt.Errorf("decode base64 ciphertext: %w", err)
	}

	hash := sha256.New()
	plaintext, err := rsa.DecryptOAEP(hash, rand.Reader, privateKey, ciphertext, nil)
	if err != nil {
		return "", fmt.Errorf("RSA decrypt: %w", err)
	}

	return string(plaintext), nil
}
