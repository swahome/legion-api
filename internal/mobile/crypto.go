// Package mobile serves the legacy mobile app's API (mobile/<action> and
// mobile/<module>/<action>) with the request and response shapes the
// released app expects. See docs/mobile/legacy-mobile-api.md.
package mobile

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"strings"
)

// Keys hold the two halves of the app's hybrid encryption: the server's
// private key opens the AES key the app sends in the Secret header, and the
// app's public key wraps the AES key of each response.
type Keys struct {
	server *rsa.PrivateKey
	app    *rsa.PublicKey
}

// LoadKeys reads the server private key (legacy 10.184.38.64.key) and the
// app certificate (legacy mobile.crt) from PEM files. The certificate's
// dates are ignored, as the legacy server ignored them: it only carries the
// app's public key.
func LoadKeys(serverKeyPath, appCertPath string) (*Keys, error) {
	keyPEM, err := os.ReadFile(serverKeyPath)
	if err != nil {
		return nil, fmt.Errorf("read mobile server key: %w", err)
	}
	certPEM, err := os.ReadFile(appCertPath)
	if err != nil {
		return nil, fmt.Errorf("read mobile app certificate: %w", err)
	}
	return ParseKeys(keyPEM, certPEM)
}

func ParseKeys(keyPEM, certPEM []byte) (*Keys, error) {
	server, err := parsePrivateKey(keyPEM)
	if err != nil {
		return nil, fmt.Errorf("mobile server key: %w", err)
	}
	app, err := parsePublicKey(certPEM)
	if err != nil {
		return nil, fmt.Errorf("mobile app certificate: %w", err)
	}
	return &Keys{server: server, app: app}, nil
}

func parsePrivateKey(b []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(b)
	if block == nil {
		return nil, errors.New("no PEM block")
	}
	if k, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return k, nil
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	rk, ok := k.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("not an RSA key")
	}
	return rk, nil
}

func parsePublicKey(b []byte) (*rsa.PublicKey, error) {
	block, _ := pem.Decode(b)
	if block == nil {
		return nil, errors.New("no PEM block")
	}
	var pub any
	switch block.Type {
	case "CERTIFICATE":
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, err
		}
		pub = cert.PublicKey
	default:
		k, err := x509.ParsePKIXPublicKey(block.Bytes)
		if err != nil {
			return nil, err
		}
		pub = k
	}
	rk, ok := pub.(*rsa.PublicKey)
	if !ok {
		return nil, errors.New("not an RSA key")
	}
	return rk, nil
}

var errBadPayload = errors.New("payload can't be decrypted")

// DecryptRequest opens a request body, "base64(ciphertext):base64(iv)",
// using the AES key RSA-wrapped in the Secret header.
func (k *Keys) DecryptRequest(secret string, body []byte) ([]byte, error) {
	wrapped, err := base64.StdEncoding.DecodeString(strings.TrimSpace(secret))
	if err != nil {
		return nil, errBadPayload
	}
	key, err := rsa.DecryptPKCS1v15(nil, k.server, wrapped)
	if err != nil {
		return nil, errBadPayload
	}
	return openAES(aesKey(key), body)
}

// EncryptResponse seals v as the legacy encrypt_json_encode did:
// {"secret": base64(RSA(key)), "body": "base64(ciphertext):base64(iv)"}
// with a fresh 16-character key per response.
func (k *Keys) EncryptResponse(v any) ([]byte, error) {
	plain, err := marshal(v)
	if err != nil {
		return nil, err
	}
	key := randomAlnum(16)
	wrapped, err := rsa.EncryptPKCS1v15(rand.Reader, k.app, key)
	if err != nil {
		return nil, err
	}
	body, err := sealAES(key, plain)
	if err != nil {
		return nil, err
	}
	return json.Marshal(map[string]string{"secret": base64.StdEncoding.EncodeToString(wrapped), "body": body})
}

// aesKey mirrors the legacy key handling: the key string is padded with
// "0" and AES-128 uses its first 16 bytes.
func aesKey(k []byte) []byte {
	out := []byte("0000000000000000")
	copy(out, k)
	return out
}

func sealAES(key, plain []byte) (string, error) {
	block, err := aes.NewCipher(aesKey(key))
	if err != nil {
		return "", err
	}
	// The legacy IV was 16 ASCII characters; keep it printable in case the
	// app treats it as a string.
	iv := randomAlnum(aes.BlockSize)
	pad := aes.BlockSize - len(plain)%aes.BlockSize
	buf := append(append([]byte{}, plain...), bytes.Repeat([]byte{byte(pad)}, pad)...)
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(buf, buf)
	return base64.StdEncoding.EncodeToString(buf) + ":" + base64.StdEncoding.EncodeToString(iv), nil
}

func openAES(key, payload []byte) ([]byte, error) {
	ctB64, ivB64, ok := strings.Cut(strings.TrimSpace(string(payload)), ":")
	if !ok {
		return nil, errBadPayload
	}
	ct, err1 := base64.StdEncoding.DecodeString(ctB64)
	iv, err2 := base64.StdEncoding.DecodeString(ivB64)
	if err1 != nil || err2 != nil || len(iv) != aes.BlockSize || len(ct) == 0 || len(ct)%aes.BlockSize != 0 {
		return nil, errBadPayload
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	out := make([]byte, len(ct))
	cipher.NewCBCDecrypter(block, iv).CryptBlocks(out, ct)
	pad := int(out[len(out)-1])
	if pad == 0 || pad > aes.BlockSize || pad > len(out) || !bytes.Equal(out[len(out)-pad:], bytes.Repeat([]byte{byte(pad)}, pad)) {
		return nil, errBadPayload
	}
	return out[:len(out)-pad], nil
}

const alnum = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

// randomAlnum returns n random letters and digits, like CodeIgniter's
// random_string('alnum', n).
func randomAlnum(n int) []byte {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	for i := range b {
		b[i] = alnum[int(b[i])%len(alnum)]
	}
	return b
}

// marshal encodes like PHP's json_encode for the parts the app could
// notice: no HTML escaping of <, > and &.
func marshal(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}
