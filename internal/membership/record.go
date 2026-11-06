// Package membership keeps the circle's member table: signed records, merging
// of tables received through gossip, invitations, revocations and tunnel
// address allocation. It does no network access.
//
// Integrity: every node has an Ed25519 key. A record has two separately
// signed parts:
//   - the admission (who this member is, who invited them), signed by the
//     inviter; the founder's is self-signed, and the circle ID is derived from
//     the founder's key, which makes it verifiable by everyone;
//   - the body (name, address, endpoints, version), signed by the member.
//
// Nobody can therefore change someone else's record. A revocation is a
// separate act, signed by the member who issues it, and final.
package membership

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Signing contexts: an admission signature cannot pass for a body or
// revocation signature. These values are part of the wire format: do not
// change them.
const (
	ctxAdmission  = "cercle1/admission\x00"
	ctxBody       = "cercle1/corps\x00"
	ctxRevocation = "cercle1/revocation\x00"
)

// Admission says who a member is and who let them in. Immutable.
type Admission struct {
	Circle    string `json:"circle"`
	NodeID    string `json:"node_id"`  // WireGuard public key, base64
	SignPub   []byte `json:"sign_pub"` // Ed25519 public key
	JoinedAt  int64  `json:"joined_at"`
	InvitedBy string `json:"invited_by"` // inviter's node_id; the founder itself for the founder
}

// Body is the part of the record only the member changes.
type Body struct {
	Circle    string            `json:"circle"`
	NodeID    string            `json:"node_id"`
	Name      string            `json:"name"`
	TunnelIP  netip.Addr        `json:"tunnel_ip"`
	Announced []string          `json:"announced,omitempty"` // announced host:port (WG_PUBLIC_ENDPOINTS)
	Observed  map[string]string `json:"observed,omitempty"`  // node_id → endpoint seen for that peer
	Version   int64             `json:"version"`
	UpdatedAt int64             `json:"updated_at"`
}

// Record is a member record as it travels: signed bytes and signatures.
type Record struct {
	Admission    json.RawMessage `json:"admission"`
	AdmissionSig []byte          `json:"admission_sig"`
	Body         json.RawMessage `json:"body"`
	BodySig      []byte          `json:"body_sig"`
}

// RevocationData is the content of a revocation.
type RevocationData struct {
	Circle string `json:"circle"`
	Target string `json:"target"`
	By     string `json:"by"`
	At     int64  `json:"at"`
}

// Revocation is a revocation signed by its author.
type Revocation struct {
	Data json.RawMessage `json:"data"`
	Sig  []byte          `json:"sig"`
}

// Table is what two nodes exchange through gossip.
type Table struct {
	Records     []Record     `json:"fiches"` // wire name kept for compatibility
	Revocations []Revocation `json:"revocations,omitempty"`
}

// CircleIDFor derives a circle ID from its founder's key.
func CircleIDFor(founder ed25519.PublicKey) string {
	h := sha256.Sum256(append([]byte("cercle1/circle\x00"), founder...))
	return hex.EncodeToString(h[:16])
}

func sign(key ed25519.PrivateKey, ctx string, data []byte) []byte {
	return ed25519.Sign(key, append([]byte(ctx), data...))
}

func verify(pub []byte, ctx string, data, sig []byte) bool {
	return len(pub) == ed25519.PublicKeySize && ed25519.Verify(pub, append([]byte(ctx), data...), sig)
}

// signedJSON encodes v and signs it.
func signedJSON(key ed25519.PrivateKey, ctx string, v any) (json.RawMessage, []byte, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, nil, err
	}
	return b, sign(key, ctx, b), nil
}

// NewAdmission signs a member's admission by the inviter (or the founder).
func NewAdmission(key ed25519.PrivateKey, a Admission) (json.RawMessage, []byte, error) {
	return signedJSON(key, ctxAdmission, a)
}

// NewBody signs a record body by its owner.
func NewBody(key ed25519.PrivateKey, b Body) (json.RawMessage, []byte, error) {
	return signedJSON(key, ctxBody, b)
}

// decoded is a record whose two signatures have been verified.
type decoded struct {
	adm    Admission
	body   Body
	record Record
}

// check verifies a record: admission signed by signer (the inviter's key),
// body signed by the member, consistent fields.
func check(f Record, circle string, signer func(nodeID string) (ed25519.PublicKey, bool)) (decoded, error) {
	var d decoded
	if err := json.Unmarshal(f.Admission, &d.adm); err != nil {
		return d, fmt.Errorf("unreadable admission: %w", err)
	}
	if err := json.Unmarshal(f.Body, &d.body); err != nil {
		return d, fmt.Errorf("unreadable body: %w", err)
	}
	a, b := d.adm, d.body
	if a.Circle != circle || b.Circle != circle {
		return d, errors.New("other circle")
	}
	if a.NodeID == "" || b.NodeID != a.NodeID {
		return d, errors.New("inconsistent IDs")
	}
	var inviterKey ed25519.PublicKey
	if a.InvitedBy == a.NodeID {
		// Founder: self-signed, and the circle derives from its key.
		if CircleIDFor(a.SignPub) != circle {
			return d, errors.New("fake founder")
		}
		inviterKey = a.SignPub
	} else {
		k, ok := signer(a.InvitedBy)
		if !ok {
			return d, errors.New("unknown inviter")
		}
		inviterKey = k
	}
	if !verify(inviterKey, ctxAdmission, f.Admission, f.AdmissionSig) {
		return d, errors.New("invalid admission signature")
	}
	if !verify(a.SignPub, ctxBody, f.Body, f.BodySig) {
		return d, errors.New("invalid body signature")
	}
	if !b.TunnelIP.Is4() {
		return d, errors.New("invalid tunnel address")
	}
	if err := validBody(b); err != nil {
		return d, err
	}
	d.record = f
	return d, nil
}

// Caps on record fields: a member must not be able to publish an oversized
// record that everyone else would store and display.
const (
	maxNameLen      = 64
	maxAnnounced    = 8
	maxEndpointLen  = 255
	maxObserved     = 256
	maxObservedAddr = 64
)

// CleanName makes a name acceptable in a record: no control character, 64
// characters at most, "member" if nothing is left.
func CleanName(n string) string {
	var out []rune
	for _, r := range strings.TrimSpace(n) {
		if !unicode.IsControl(r) && len(out) < maxNameLen {
			out = append(out, r)
		}
	}
	if len(out) == 0 {
		return "member"
	}
	return string(out)
}

func validBody(b Body) error {
	if n := utf8.RuneCountInString(b.Name); n == 0 || n > maxNameLen {
		return errors.New("empty or too long name")
	}
	for _, r := range b.Name {
		if unicode.IsControl(r) {
			return errors.New("control character in the name")
		}
	}
	if len(b.Announced) > maxAnnounced {
		return errors.New("too many announced endpoints")
	}
	for _, e := range b.Announced {
		if len(e) > maxEndpointLen {
			return errors.New("announced endpoint too long")
		}
	}
	if len(b.Observed) > maxObserved {
		return errors.New("too many observed endpoints")
	}
	for k, v := range b.Observed {
		if len(k) > 64 || len(v) > maxObservedAddr {
			return errors.New("invalid observed endpoint")
		}
	}
	return nil
}

// LoadOrCreateSignKey reads the node's Ed25519 key, or creates it (0600).
func LoadOrCreateSignKey(path string) (ed25519.PrivateKey, error) {
	b, err := os.ReadFile(path)
	if err == nil {
		seed, derr := base64.StdEncoding.DecodeString(strings.TrimSpace(string(b)))
		if derr != nil || len(seed) != ed25519.SeedSize {
			return nil, fmt.Errorf("%s: unreadable signing key", path)
		}
		return ed25519.NewKeyFromSeed(seed), nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	seed := make([]byte, ed25519.SeedSize)
	if _, err := rand.Read(seed); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(base64.StdEncoding.EncodeToString(seed)+"\n"), 0o600); err != nil {
		return nil, fmt.Errorf("writing the signing key: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return nil, fmt.Errorf("writing the signing key: %w", err)
	}
	return ed25519.NewKeyFromSeed(seed), nil
}
