package gitbridge

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"time"
)

// jwtHeader is the canonical RS256 header for a GitHub App JWT.
type jwtHeader struct {
	Alg string `json:"alg"`
	Typ string `json:"typ"`
}

// jwtPayload is the minimal claim set GitHub accepts. iss is the numeric
// App ID. GitHub requires iat to be no more than 60 seconds in the past
// and exp to be no more than 10 minutes in the future.
type jwtPayload struct {
	Iat int64 `json:"iat"`
	Exp int64 `json:"exp"`
	Iss int64 `json:"iss"`
}

// loadPrivateKey reads a PEM-encoded RSA private key from path. Both
// PKCS#1 ("RSA PRIVATE KEY") and PKCS#8 ("PRIVATE KEY") forms are accepted.
func loadPrivateKey(path string) (*rsa.PrivateKey, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read private key: %w", err)
	}
	block, _ := pem.Decode(b)
	if block == nil {
		return nil, errors.New("no PEM block found in private key file")
	}
	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse private key: %w", err)
	}
	rsaKey, ok := k.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("private key is not RSA")
	}
	return rsaKey, nil
}

// mintJWT produces a fresh RS256 JWT for the GitHub App. The token is
// signed in-process; the resulting string is never logged by the broker.
// The function is called once per installation-token mint, so the JWT
// has the shortest possible lifetime.
func mintJWT(key *rsa.PrivateKey, appID int64) (string, error) {
	now := time.Now()
	hdr := jwtHeader{Alg: "RS256", Typ: "JWT"}
	pl := jwtPayload{
		Iat: now.Add(-60 * time.Second).Unix(),
		Exp: now.Add(9 * time.Minute).Unix(),
		Iss: appID,
	}
	hdrJSON, err := json.Marshal(hdr)
	if err != nil {
		return "", fmt.Errorf("marshal jwt header: %w", err)
	}
	plJSON, err := json.Marshal(pl)
	if err != nil {
		return "", fmt.Errorf("marshal jwt payload: %w", err)
	}
	hdrEnc := base64.RawURLEncoding.EncodeToString(hdrJSON)
	plEnc := base64.RawURLEncoding.EncodeToString(plJSON)
	signingInput := hdrEnc + "." + plEnc
	digest := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		return "", fmt.Errorf("sign jwt: %w", err)
	}
	sigEnc := base64.RawURLEncoding.EncodeToString(sig)
	return signingInput + "." + sigEnc, nil
}
