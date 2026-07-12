package webpush

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// marshalEC renders a key as a SEC 1 "EC PRIVATE KEY" PEM block.
func marshalEC(key *ecdsa.PrivateKey) ([]byte, error) {
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}), nil
}

func b64must(t *testing.T, s string) []byte {
	t.Helper()
	b, err := b64Field(s)
	if err != nil {
		t.Fatalf("bad base64 %q: %v", s, err)
	}
	return b
}

// TestRFC8291AppendixAVector reproduces the worked example from RFC 8291
// Appendix A byte-for-byte: fixed subscriber keys, auth secret, sender key
// and salt must produce exactly the published message.
func TestRFC8291AppendixAVector(t *testing.T) {
	uaPublic := b64must(t, "BCVxsr7N_eNgVRqvHtD0zTZsEc6-VV-JvLexhqUzORcxaOzi6-AYWXvTBHm4bjyPjs7Vd8pZGH6SRpkNtoIAiw4")
	authSecret := b64must(t, "BTBZMqHH6r4Tts7J_aSIgg")
	salt := b64must(t, "DGv6ra1nlYgDCS1FRnbzlw")
	asPrivate := b64must(t, "yfWPiYE-n46HLnH0KqZOF1fJJU3MYrct3AELtAQ-oRw")
	plaintext := []byte("When I grow up, I want to be a watermelon")
	const expected = "DGv6ra1nlYgDCS1FRnbzlwAAEABBBP4z9KsN6nGRTbVYI_c7VJSPQTBtkgcy27ml" +
		"mlMoZIIgDll6e3vCYLocInmYWAmS6TlzAC8wEqKK6PBru3jl7A_yl95bQpu6cVPT" +
		"pK4Mqgkf1CXztLVBSt2Ks3oZwbuwXPXLWyouBWLVWGNWQexSgSxsj_Qulcy4a-fN"

	asKey, err := ecdh.P256().NewPrivateKey(asPrivate)
	if err != nil {
		t.Fatal(err)
	}
	// Sanity: the derived sender public key must match the RFC's as_public.
	if got := base64.RawURLEncoding.EncodeToString(asKey.PublicKey().Bytes()); got != "BP4z9KsN6nGRTbVYI_c7VJSPQTBtkgcy27mlmlMoZIIgDll6e3vCYLocInmYWAmS6TlzAC8wEqKK6PBru3jl7A8" {
		t.Fatalf("as_public mismatch: %s", got)
	}

	got, err := encrypt(uaPublic, authSecret, salt, asKey, plaintext)
	if err != nil {
		t.Fatal(err)
	}
	if enc := base64.RawURLEncoding.EncodeToString(got); enc != expected {
		t.Fatalf("ciphertext mismatch:\ngot  %s\nwant %s", enc, expected)
	}

	// Cross-check from the receiver side: decrypting the RFC's message with
	// the RFC's subscriber PRIVATE key must yield the plaintext. This guards
	// against a mirrored sender-side bug reproducing itself.
	uaKey, err := ecdh.P256().NewPrivateKey(b64must(t, "q1dXpw3UpT5VOmu_cf_v6ih07Aems3njxI-JWgLcM94"))
	if err != nil {
		t.Fatal(err)
	}
	pt, err := decrypt(t, uaKey, authSecret, got)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(pt, plaintext) {
		t.Fatalf("round-trip plaintext = %q", pt)
	}
}

// decrypt implements the receiver side of RFC 8291 for tests.
func decrypt(t *testing.T, uaKey *ecdh.PrivateKey, authSecret, message []byte) ([]byte, error) {
	t.Helper()
	if len(message) < 21 {
		t.Fatal("short message")
	}
	salt := message[:16]
	rs := binary.BigEndian.Uint32(message[16:20])
	idlen := int(message[20])
	if rs != recordSize || idlen != 65 {
		t.Fatalf("unexpected header rs=%d idlen=%d", rs, idlen)
	}
	asPublicBytes := message[21 : 21+idlen]
	body := message[21+idlen:]

	asPub, err := ecdh.P256().NewPublicKey(asPublicBytes)
	if err != nil {
		return nil, err
	}
	secret, err := uaKey.ECDH(asPub)
	if err != nil {
		return nil, err
	}
	prkKey, err := hkdf.Extract(sha256.New, secret, authSecret)
	if err != nil {
		return nil, err
	}
	keyInfo := "WebPush: info\x00" + string(uaKey.PublicKey().Bytes()) + string(asPublicBytes)
	ikm, err := hkdf.Expand(sha256.New, prkKey, keyInfo, 32)
	if err != nil {
		return nil, err
	}
	prk, err := hkdf.Extract(sha256.New, ikm, salt)
	if err != nil {
		return nil, err
	}
	cek, err := hkdf.Expand(sha256.New, prk, "Content-Encoding: aes128gcm\x00", 16)
	if err != nil {
		return nil, err
	}
	nonce, err := hkdf.Expand(sha256.New, prk, "Content-Encoding: nonce\x00", 12)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(cek)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	record, err := gcm.Open(nil, nonce, body, nil)
	if err != nil {
		return nil, err
	}
	i := len(record) - 1
	for i >= 0 && record[i] == 0 {
		i--
	}
	if i < 0 || record[i] != 0x02 {
		t.Fatalf("bad padding delimiter")
	}
	return record[:i], nil
}

func TestEncryptRoundTripRandomKeys(t *testing.T) {
	uaKey, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	authSecret := make([]byte, 16)
	if _, err := rand.Read(authSecret); err != nil {
		t.Fatal(err)
	}
	sub := Subscription{
		Endpoint: "https://push.example/x",
		P256dh:   base64.RawURLEncoding.EncodeToString(uaKey.PublicKey().Bytes()),
		Auth:     base64.RawURLEncoding.EncodeToString(authSecret),
	}
	payload := []byte(`{"title":"Business round trip open: LON ⇄ TYO","body":"1 new date: Mon 12 Oct"}`)
	msg, err := Encrypt(sub, payload)
	if err != nil {
		t.Fatal(err)
	}
	pt, err := decrypt(t, uaKey, authSecret, msg)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(pt, payload) {
		t.Fatalf("round trip = %q", pt)
	}
	// Fresh salt + ephemeral key: two encryptions of the same payload differ.
	msg2, err := Encrypt(sub, payload)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(msg, msg2) {
		t.Fatal("encryption must be randomized")
	}
}

func TestVapidAuthorization(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	v, err := NewVapid(key, "mailto:alerts@rewardflights.lucy.sh")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1783810462, 0)
	header, err := v.Authorization("https://fcm.googleapis.com/fcm/send/abc123", now)
	if err != nil {
		t.Fatal(err)
	}

	if !strings.HasPrefix(header, "vapid t=") || !strings.Contains(header, ", k=") {
		t.Fatalf("header shape: %s", header)
	}
	jwt := header[len("vapid t="):strings.Index(header, ", k=")]
	pub := header[strings.Index(header, ", k=")+len(", k="):]

	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		t.Fatalf("jwt parts = %d", len(parts))
	}
	var hdr struct{ Alg, Typ string }
	if err := json.Unmarshal(b64must(t, parts[0]), &hdr); err != nil || hdr.Alg != "ES256" || hdr.Typ != "JWT" {
		t.Fatalf("jwt header = %+v (%v)", hdr, err)
	}
	var claims struct {
		Aud string `json:"aud"`
		Exp int64  `json:"exp"`
		Sub string `json:"sub"`
	}
	if err := json.Unmarshal(b64must(t, parts[1]), &claims); err != nil {
		t.Fatal(err)
	}
	if claims.Aud != "https://fcm.googleapis.com" {
		t.Errorf("aud = %q, want the endpoint origin only", claims.Aud)
	}
	if claims.Sub != "mailto:alerts@rewardflights.lucy.sh" {
		t.Errorf("sub = %q", claims.Sub)
	}
	if claims.Exp != now.Add(TokenTTL).Unix() {
		t.Errorf("exp = %d, want now+12h = %d", claims.Exp, now.Add(TokenTTL).Unix())
	}

	// Verify the ES256 signature with the public key advertised in k=.
	pubPoint, err := ecdh.P256().NewPublicKey(b64must(t, pub))
	if err != nil {
		t.Fatal(err)
	}
	x, y := elliptic.Unmarshal(elliptic.P256(), pubPoint.Bytes())
	sig := b64must(t, parts[2])
	if len(sig) != 64 {
		t.Fatalf("signature length = %d", len(sig))
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if !ecdsa.Verify(&ecdsa.PublicKey{Curve: elliptic.P256(), X: x, Y: y},
		digest[:], new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])) {
		t.Error("ES256 signature does not verify")
	}
}

func TestLoadVapidKeyFormats(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()

	// Bare base64url 32-byte scalar (the common generator output).
	scalar := make([]byte, 32)
	key.D.FillBytes(scalar)
	rawPath := filepath.Join(dir, "vapid.txt")
	if err := os.WriteFile(rawPath, []byte(base64.RawURLEncoding.EncodeToString(scalar)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// PEM (SEC 1).
	der, err := marshalEC(key)
	if err != nil {
		t.Fatal(err)
	}
	pemPath := filepath.Join(dir, "vapid.pem")
	if err := os.WriteFile(pemPath, der, 0o600); err != nil {
		t.Fatal(err)
	}

	for _, path := range []string{rawPath, pemPath} {
		v, err := LoadVapid(path, "mailto:x@y")
		if err != nil {
			t.Fatalf("LoadVapid(%s): %v", path, err)
		}
		if v.key.D.Cmp(key.D) != 0 {
			t.Errorf("%s: loaded scalar differs", path)
		}
	}
	if _, err := LoadVapid(filepath.Join(dir, "missing"), "s"); err == nil {
		t.Error("missing key file must error")
	}
	bad := filepath.Join(dir, "bad")
	if err := os.WriteFile(bad, []byte("not a key"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadVapid(bad, "s"); err == nil {
		t.Error("garbage key file must error")
	}
}
