package gitbridge

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"os"
	"strings"
	"testing"
	"time"
)

func TestLoadPrivateKeyPKCS1(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	})
	path := writeTempKey(t, pemBytes)
	t.Cleanup(func() { _ = os.Remove(path) })

	loaded, err := loadPrivateKey(path)
	if err != nil {
		t.Fatalf("loadPrivateKey: %v", err)
	}
	if loaded.D.Cmp(key.D) != 0 {
		t.Errorf("loaded key D does not match original")
	}
}

func TestLoadPrivateKeyPKCS8(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	pkcs8, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8})
	path := writeTempKey(t, pemBytes)
	t.Cleanup(func() { _ = os.Remove(path) })

	if _, err := loadPrivateKey(path); err != nil {
		t.Fatalf("loadPrivateKey PKCS#8: %v", err)
	}
}

func TestLoadPrivateKeyBadPEM(t *testing.T) {
	path := writeTempKey(t, []byte("not a pem file"))
	t.Cleanup(func() { _ = os.Remove(path) })
	if _, err := loadPrivateKey(path); err == nil {
		t.Errorf("expected error for bad PEM")
	}
}

func TestLoadPrivateKeyMissing(t *testing.T) {
	if _, err := loadPrivateKey("/nonexistent/key.pem"); err == nil {
		t.Errorf("expected error for missing file")
	}
}

func TestMintJWT_StructureAndTiming(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	tok, err := mintJWT(key, 12345)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		t.Fatalf("JWT must have 3 parts, got %d", len(parts))
	}
	hdr, pl := decodeJWTForTest(t, tok)
	if hdr["alg"] != "RS256" {
		t.Errorf("alg = %v, want RS256", hdr["alg"])
	}
	if hdr["typ"] != "JWT" {
		t.Errorf("typ = %v, want JWT", hdr["typ"])
	}
	iat := int64(pl["iat"].(float64))
	exp := int64(pl["exp"].(float64))
	iss := int64(pl["iss"].(float64))
	now := time.Now().Unix()
	if iat > now {
		t.Errorf("iat in future: %d > %d", iat, now)
	}
	if exp <= now {
		t.Errorf("exp not in future: %d <= %d", exp, now)
	}
	if exp-iat < 300 {
		t.Errorf("exp-iat too small: %d", exp-iat)
	}
	if exp-iat > 600 {
		t.Errorf("exp-iat too large: %d", exp-iat)
	}
	if iss != 12345 {
		t.Errorf("iss = %d, want 12345", iss)
	}
}

func TestMintJWT_NeverContainsKeyMaterial(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	tok, err := mintJWT(key, 1)
	if err != nil {
		t.Fatal(err)
	}
	// The token must never contain the key bytes. Cheap sanity check:
	// the encoded token is short and contains no "BEGIN" or "PRIVATE".
	if strings.Contains(tok, "BEGIN") || strings.Contains(tok, "PRIVATE") {
		t.Errorf("JWT string looks like it contains key material: %s", tok)
	}
}

// helpers

func writeTempKey(t *testing.T, data []byte) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "key-*.pem")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return f.Name()
}

func decodeJWTForTest(t *testing.T, tok string) (map[string]any, map[string]any) {
	t.Helper()
	parts := strings.Split(tok, ".")
	hdrJSON, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		t.Fatal(err)
	}
	plJSON, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	var hdr map[string]any
	if err := json.Unmarshal(hdrJSON, &hdr); err != nil {
		t.Fatal(err)
	}
	var pl map[string]any
	if err := json.Unmarshal(plJSON, &pl); err != nil {
		t.Fatal(err)
	}
	return hdr, pl
}
