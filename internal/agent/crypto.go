// crypto.go implements RSA asymmetric encryption for secure transport of Git
// credentials.
//
// This file provides a secure credential transport mechanism between the Agent
// Daemon and the Server, mainly including:
//   - GenerateRSAKeyPair: generates a 2048-bit RSA key pair (PEM-encoded); the
//     public key is sent to the server to encrypt credentials
//   - DecryptWithPrivateKey: uses the private key to decrypt Git credentials
//     (PAT) RSA-OAEP-encrypted by the server
//
// Encryption flow: the Server encrypts the Git PAT using the Agent's public key
// -> the Agent decrypts it with the local private key -> injected into the Git
// askpass script.
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

// GenerateRSAKeyPair generates a 2048-bit RSA key pair and returns the
// PEM-encoded public and private keys.
// The public key is sent to the server to encrypt Git credentials; the private
// key is kept locally for decryption.
//
// Returns:
//   - publicKeyPEM: PEM-encoded public key
//   - privateKeyPEM: PEM-encoded private key
//   - error: returned on generation failure
func GenerateRSAKeyPair() (publicKeyPEM string, privateKeyPEM string, err error) {
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return "", "", fmt.Errorf("generate RSA key: %w", err)
	}

	// Encode the private key to PEM format
	privBytes := x509.MarshalPKCS1PrivateKey(privateKey)
	privPEM := pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: privBytes,
	})

	// Encode the public key to PEM format
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

// DecryptWithPrivateKey uses the given private key PEM to decrypt base64-encoded
// RSA-OAEP ciphertext.
// Used to decrypt Git credentials (personal access tokens) encrypted by the
// server.
//
// Parameters:
//   - privateKeyPEM: PEM-encoded RSA private key
//   - ciphertextBase64: base64-encoded ciphertext
//
// Returns:
//   - string: the decrypted plaintext
//   - error: returned on decryption failure
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
