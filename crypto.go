package main

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// encPrefix markerer en krypteret værdi i databasen. v1 = AES-256-GCM med 12 bytes nonce foran.
const encPrefix = "vdenc:v1:"

// secretBox krypterer hemmeligheder, før de gemmes. Nøglen ligger uden for databasen, så en kopi
// af databasen (eller en backup af den) ikke afslører hemmelighederne alene.
type secretBox struct{ aead cipher.AEAD }

// newSecretBox laver en secretBox ud fra en nøgle på 32 bytes.
func newSecretBox(key []byte) (*secretBox, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("nøglen skal være 32 bytes, men er %d", len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &secretBox{aead: aead}, nil
}

// seal krypterer v. ad (fx «secret:<slug>:<navn>») bindes til værdien, så en krypteret værdi
// ikke kan flyttes over i en anden app eller under et andet navn.
func (b *secretBox) seal(v, ad string) string {
	nonce := make([]byte, b.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		panic(err)
	}
	ct := b.aead.Seal(nonce, nonce, []byte(v), []byte(ad))
	return encPrefix + base64.StdEncoding.EncodeToString(ct)
}

// open dekrypterer en værdi fra seal. Værdier uden encPrefix (fra før krypteringen) returneres,
// som de er; de krypteres næste gang, de gemmes.
func (b *secretBox) open(v, ad string) (string, error) {
	rest, ok := strings.CutPrefix(v, encPrefix)
	if !ok {
		return v, nil
	}
	raw, err := base64.StdEncoding.DecodeString(rest)
	if err != nil || len(raw) < b.aead.NonceSize() {
		return "", errors.New("ugyldig krypteret værdi")
	}
	n := b.aead.NonceSize()
	pt, err := b.aead.Open(nil, raw[:n], raw[n:], []byte(ad))
	if err != nil {
		return "", errors.New("kunne ikke dekryptere (forkert nøgle?)")
	}
	return string(pt), nil
}

// loadKey læser nøglen (base64) fra path. Findes filen ikke, og create er sand, laves en ny
// tilfældig nøgle med rettigheder 0600.
func loadKey(path string, create bool) ([]byte, error) {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) && create {
		key := make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			return nil, err
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return nil, err
		}
		if err := os.WriteFile(path, []byte(base64.StdEncoding.EncodeToString(key)+"\n"), 0o600); err != nil {
			return nil, err
		}
		return key, nil
	}
	if err != nil {
		return nil, err
	}
	key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(b)))
	if err != nil || len(key) != 32 {
		return nil, fmt.Errorf("%s skal indeholde 32 bytes som base64 (lav en med: head -c 32 /dev/urandom | base64)", path)
	}
	return key, nil
}
