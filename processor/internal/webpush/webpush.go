// Package webpush implements the standards Web Push sender-side crypto:
// RFC 8291 message encryption (aes128gcm content coding, ECDH P-256 +
// HKDF-SHA256 + AES-128-GCM with RFC 8188 framing) and RFC 8292 VAPID
// authorization (ES256 JWT). Stdlib only.
//
// Correctness is pinned by a test reproducing the RFC 8291 Appendix A worked
// example byte-for-byte: Web Push crypto has no partial-failure mode — any
// deviation means every notification silently fails to decrypt.
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
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// Subscription is one browser push subscription, as stored by the
// subscription Worker (p256dh and auth are base64url per PushSubscription).
type Subscription struct {
	Endpoint string `json:"endpoint"`
	P256dh   string `json:"p256dh"`
	Auth     string `json:"auth"`
}

// recordSize is the RFC 8188 record size we emit. Alert payloads are tiny,
// so everything fits one record.
const recordSize = 4096

// Encrypt seals plaintext for the subscription per RFC 8291 with a fresh
// random salt and ephemeral sender key. The result is the complete request
// body (aes128gcm header block + single ciphertext record).
func Encrypt(sub Subscription, plaintext []byte) ([]byte, error) {
	uaPublic, err := b64Field(sub.P256dh)
	if err != nil {
		return nil, fmt.Errorf("p256dh: %w", err)
	}
	authSecret, err := b64Field(sub.Auth)
	if err != nil {
		return nil, fmt.Errorf("auth: %w", err)
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return nil, err
	}
	asKey, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	return encrypt(uaPublic, authSecret, salt, asKey, plaintext)
}

// encrypt is the deterministic RFC 8291 core (fixed salt + sender key),
// exercised directly by the Appendix A vector test.
func encrypt(uaPublic, authSecret, salt []byte, asKey *ecdh.PrivateKey, plaintext []byte) ([]byte, error) {
	uaPub, err := ecdh.P256().NewPublicKey(uaPublic)
	if err != nil {
		return nil, fmt.Errorf("subscriber public key: %w", err)
	}
	ecdhSecret, err := asKey.ECDH(uaPub)
	if err != nil {
		return nil, err
	}
	asPublic := asKey.PublicKey().Bytes()

	// RFC 8291 §3.3–3.4: IKM = HKDF(auth_secret, ecdh_secret, key_info);
	// then RFC 8188 key derivation with the message salt.
	prkKey, err := hkdf.Extract(sha256.New, ecdhSecret, authSecret)
	if err != nil {
		return nil, err
	}
	keyInfo := "WebPush: info\x00" + string(uaPublic) + string(asPublic)
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

	if len(plaintext)+17 > recordSize { // padding delimiter + 16-byte tag
		return nil, errors.New("payload exceeds a single record")
	}
	block, err := aes.NewCipher(cek)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	record := make([]byte, 0, len(plaintext)+1)
	record = append(record, plaintext...)
	record = append(record, 0x02) // last-record padding delimiter

	// RFC 8188 header: salt(16) | rs(4) | idlen(1) | keyid(=as_public, 65).
	out := make([]byte, 0, 16+4+1+len(asPublic)+len(record)+16)
	out = append(out, salt...)
	out = binary.BigEndian.AppendUint32(out, recordSize)
	out = append(out, byte(len(asPublic)))
	out = append(out, asPublic...)
	return gcm.Seal(out, nonce, record, nil), nil
}

// Sender delivers encrypted messages to push services. Its Client is
// exported so callers (and tests) can supply their own transport/timeout.
type Sender struct {
	Client *http.Client
	Vapid  *Vapid
}

// SendTimeout bounds a single push delivery.
const SendTimeout = 5 * time.Second

// NewSender wraps a VAPID key with a 5s-timeout HTTP client.
func NewSender(vapid *Vapid) *Sender {
	return &Sender{Client: &http.Client{Timeout: SendTimeout}, Vapid: vapid}
}

// Send encrypts payload for sub (RFC 8291) and POSTs it to the subscription's
// endpoint with VAPID authorization. It returns the push service's HTTP status
// — callers decide what 404/410 (subscription gone) and 5xx (transient) mean.
// A non-nil error means the message never reached the service at all.
func (s *Sender) Send(sub Subscription, payload []byte) (int, error) {
	body, err := Encrypt(sub, payload)
	if err != nil {
		return 0, err
	}
	auth, err := s.Vapid.Authorization(sub.Endpoint, time.Now())
	if err != nil {
		return 0, err
	}
	req, err := http.NewRequest(http.MethodPost, sub.Endpoint, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("TTL", "86400")
	req.Header.Set("Content-Encoding", "aes128gcm")
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("Authorization", auth)
	resp, err := s.Client.Do(req)
	if err != nil {
		return 0, err
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp.StatusCode, nil
}

// Expired reports whether a push-service status means the subscription is
// permanently gone and should be dropped from the store.
func Expired(status int) bool {
	return status == http.StatusNotFound || status == http.StatusGone
}

// Vapid mints RFC 8292 Authorization header values for push endpoints.
type Vapid struct {
	key     *ecdsa.PrivateKey
	subject string
	public  string // base64url uncompressed P-256 public key
}

// TokenTTL is the lifetime of minted VAPID JWTs.
const TokenTTL = 12 * time.Hour

// NewVapid wraps an ES256 signing key and contact subject.
func NewVapid(key *ecdsa.PrivateKey, subject string) (*Vapid, error) {
	if key.Curve != elliptic.P256() {
		return nil, errors.New("VAPID key must be P-256")
	}
	eck, err := key.ECDH()
	if err != nil {
		return nil, err
	}
	return &Vapid{
		key:     key,
		subject: subject,
		public:  base64.RawURLEncoding.EncodeToString(eck.PublicKey().Bytes()),
	}, nil
}

// LoadVapid reads the private key from path: PEM ("EC PRIVATE KEY" or PKCS#8
// "PRIVATE KEY") or a bare base64url-encoded 32-byte P-256 scalar.
func LoadVapid(path, subject string) (*Vapid, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading VAPID key: %w", err)
	}
	key, err := parsePrivateKey(raw)
	if err != nil {
		return nil, fmt.Errorf("parsing VAPID key %s: %w", path, err)
	}
	return NewVapid(key, subject)
}

func parsePrivateKey(raw []byte) (*ecdsa.PrivateKey, error) {
	if block, _ := pem.Decode(raw); block != nil {
		switch block.Type {
		case "EC PRIVATE KEY":
			return x509.ParseECPrivateKey(block.Bytes)
		case "PRIVATE KEY":
			parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
			if err != nil {
				return nil, err
			}
			key, ok := parsed.(*ecdsa.PrivateKey)
			if !ok {
				return nil, errors.New("PKCS#8 key is not ECDSA")
			}
			return key, nil
		default:
			return nil, fmt.Errorf("unsupported PEM block %q", block.Type)
		}
	}
	scalar, err := b64Field(strings.TrimSpace(string(raw)))
	if err != nil || len(scalar) != 32 {
		return nil, errors.New("expected PEM or a base64url 32-byte P-256 scalar")
	}
	// Reconstruct the ECDSA key from the bare scalar (the common output of
	// VAPID key generators). ScalarBaseMult is the stdlib's only route from
	// scalar to public point.
	key := &ecdsa.PrivateKey{
		D:         new(big.Int).SetBytes(scalar),
		PublicKey: ecdsa.PublicKey{Curve: elliptic.P256()},
	}
	key.PublicKey.X, key.PublicKey.Y = key.Curve.ScalarBaseMult(scalar)
	if key.PublicKey.X == nil {
		return nil, errors.New("invalid P-256 scalar")
	}
	return key, nil
}

// Authorization returns the "vapid t=<jwt>, k=<pub>" header value for the
// push service hosting endpoint: JWT aud = the endpoint's origin,
// exp = now + TokenTTL, sub = the configured subject, signed ES256.
func (v *Vapid) Authorization(endpoint string, now time.Time) (string, error) {
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf("bad endpoint %q", endpoint)
	}
	claims, err := json.Marshal(map[string]any{
		"aud": u.Scheme + "://" + u.Host,
		"exp": now.Add(TokenTTL).Unix(),
		"sub": v.subject,
	})
	if err != nil {
		return "", err
	}
	b64 := base64.RawURLEncoding
	signingInput := b64.EncodeToString([]byte(`{"alg":"ES256","typ":"JWT"}`)) + "." + b64.EncodeToString(claims)
	digest := sha256.Sum256([]byte(signingInput))
	r, s, err := ecdsa.Sign(rand.Reader, v.key, digest[:])
	if err != nil {
		return "", err
	}
	sig := make([]byte, 64) // JWS ES256: raw R || S, 32 bytes each
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])
	return "vapid t=" + signingInput + "." + b64.EncodeToString(sig) + ", k=" + v.public, nil
}

// b64Field decodes base64url with or without padding (and tolerates
// standard-alphabet input, which some clients emit).
func b64Field(s string) ([]byte, error) {
	s = strings.TrimRight(s, "=")
	if strings.ContainsAny(s, "+/") {
		return base64.RawStdEncoding.DecodeString(s)
	}
	return base64.RawURLEncoding.DecodeString(s)
}
