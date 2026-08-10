package secure

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"golang.org/x/crypto/argon2"
)

type Vault struct{ aead cipher.AEAD }

func NewVault(key []byte) (*Vault, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Vault{aead: aead}, nil
}

func (v *Vault) Encrypt(plaintext string) (string, error) {
	if plaintext == "" {
		return "", nil
	}
	nonce := make([]byte, v.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	sealed := v.aead.Seal(nil, nonce, []byte(plaintext), nil)
	return base64.RawURLEncoding.EncodeToString(append(nonce, sealed...)), nil
}

func (v *Vault) Decrypt(encoded string) (string, error) {
	if encoded == "" {
		return "", nil
	}
	data, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return "", err
	}
	if len(data) < v.aead.NonceSize() {
		return "", errors.New("encrypted value is truncated")
	}
	plain, err := v.aead.Open(nil, data[:v.aead.NonceSize()], data[v.aead.NonceSize():], nil)
	if err != nil {
		return "", errors.New("cannot decrypt value")
	}
	return string(plain), nil
}

func RandomToken(bytes int) (raw, hash string, err error) {
	buf := make([]byte, bytes)
	if _, err = io.ReadFull(rand.Reader, buf); err != nil {
		return "", "", err
	}
	raw = base64.RawURLEncoding.EncodeToString(buf)
	return raw, TokenHash(raw), nil
}

func TokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func HashPassword(password string) (string, error) {
	if len(password) < 12 {
		return "", errors.New("password must be at least 12 characters")
	}
	salt := make([]byte, 16)
	if _, err := io.ReadFull(rand.Reader, salt); err != nil {
		return "", err
	}
	hash := argon2.IDKey([]byte(password), salt, 1, 64*1024, 4, 32)
	return fmt.Sprintf("$argon2id$v=19$m=65536,t=1,p=4$%s$%s",
		base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(hash)), nil
}

func VerifyPassword(encoded, password string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" || parts[2] != "v=19" {
		return false
	}
	var memory uint64
	var iterations uint64
	var parallelism uint64
	for _, parameter := range strings.Split(parts[3], ",") {
		pair := strings.SplitN(parameter, "=", 2)
		if len(pair) != 2 {
			return false
		}
		value, err := strconv.ParseUint(pair[1], 10, 32)
		if err != nil {
			return false
		}
		switch pair[0] {
		case "m":
			memory = value
		case "t":
			iterations = value
		case "p":
			parallelism = value
		}
	}
	if memory == 0 || iterations == 0 || parallelism == 0 || parallelism > 255 {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return false
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return false
	}
	got := argon2.IDKey([]byte(password), salt, uint32(iterations), uint32(memory), uint8(parallelism), uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1
}
