// Package membership tient la table des membres du cercle : fiches signées,
// fusion des tables reçues par gossip, invitations, révocations, attribution
// des adresses de tunnel. Il ne fait aucun accès réseau.
//
// Intégrité (décision de relecture du 4 novembre 2026, à la place du HMAC à
// clé de groupe) : chaque nœud a une clé Ed25519. Une fiche a deux parties
// signées séparément :
//   - l'admission (qui est ce membre, qui l'a invité), signée par l'invitant ;
//     celle du fondateur est signée par lui-même, et l'identifiant du cercle
//     dérive de sa clé, ce qui la rend vérifiable par tous ;
//   - le corps (nom, adresse, endpoints, version), signé par le membre lui-même.
//
// Personne ne peut donc modifier la fiche d'un autre. Une révocation est un
// acte séparé, signé par le membre qui la prononce, et définitif.
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

// Préfixes de signature : une signature d'admission ne peut pas passer pour
// une signature de corps ou de révocation.
const (
	ctxAdmission  = "cercle1/admission\x00"
	ctxBody       = "cercle1/corps\x00"
	ctxRevocation = "cercle1/revocation\x00"
)

// Admission dit qui est un membre et qui l'a fait entrer. Immuable.
type Admission struct {
	Circle    string `json:"circle"`
	NodeID    string `json:"node_id"`  // clé publique WireGuard, base64
	SignPub   []byte `json:"sign_pub"` // clé publique Ed25519
	JoinedAt  int64  `json:"joined_at"`
	InvitedBy string `json:"invited_by"` // node_id de l'invitant ; lui-même pour le fondateur
}

// Body est la partie de la fiche que seul le membre modifie.
type Body struct {
	Circle    string            `json:"circle"`
	NodeID    string            `json:"node_id"`
	Name      string            `json:"name"`
	TunnelIP  netip.Addr        `json:"tunnel_ip"`
	Announced []string          `json:"announced,omitempty"` // hôte:port déclarés (WG_PUBLIC_ENDPOINTS)
	Observed  map[string]string `json:"observed,omitempty"`  // node_id → endpoint vu chez ce pair
	Version   int64             `json:"version"`
	UpdatedAt int64             `json:"updated_at"`
}

// Fiche est une fiche telle qu'elle circule : octets signés et signatures.
type Fiche struct {
	Admission    json.RawMessage `json:"admission"`
	AdmissionSig []byte          `json:"admission_sig"`
	Body         json.RawMessage `json:"body"`
	BodySig      []byte          `json:"body_sig"`
}

// RevocationData est le contenu d'une révocation.
type RevocationData struct {
	Circle string `json:"circle"`
	Target string `json:"target"`
	By     string `json:"by"`
	At     int64  `json:"at"`
}

// Revocation est une révocation signée par son auteur.
type Revocation struct {
	Data json.RawMessage `json:"data"`
	Sig  []byte          `json:"sig"`
}

// Table est ce qu'échangent deux nœuds par gossip.
type Table struct {
	Fiches      []Fiche      `json:"fiches"`
	Revocations []Revocation `json:"revocations,omitempty"`
}

// CircleIDFor dérive l'identifiant d'un cercle de la clé de son fondateur.
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

// signedJSON encode v et le signe.
func signedJSON(key ed25519.PrivateKey, ctx string, v any) (json.RawMessage, []byte, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, nil, err
	}
	return b, sign(key, ctx, b), nil
}

// NewAdmission signe l'admission d'un membre par l'invitant (ou le fondateur).
func NewAdmission(key ed25519.PrivateKey, a Admission) (json.RawMessage, []byte, error) {
	return signedJSON(key, ctxAdmission, a)
}

// NewBody signe le corps d'une fiche par son propriétaire.
func NewBody(key ed25519.PrivateKey, b Body) (json.RawMessage, []byte, error) {
	return signedJSON(key, ctxBody, b)
}

// decoded est une fiche dont les deux signatures ont été vérifiées.
type decoded struct {
	adm   Admission
	body  Body
	fiche Fiche
}

// check vérifie une fiche : admission signée par signer (clé de l'invitant),
// corps signé par le membre, cohérence des champs.
func check(f Fiche, circle string, signer func(nodeID string) (ed25519.PublicKey, bool)) (decoded, error) {
	var d decoded
	if err := json.Unmarshal(f.Admission, &d.adm); err != nil {
		return d, fmt.Errorf("admission illisible : %w", err)
	}
	if err := json.Unmarshal(f.Body, &d.body); err != nil {
		return d, fmt.Errorf("corps illisible : %w", err)
	}
	a, b := d.adm, d.body
	if a.Circle != circle || b.Circle != circle {
		return d, errors.New("autre cercle")
	}
	if a.NodeID == "" || b.NodeID != a.NodeID {
		return d, errors.New("identifiants incohérents")
	}
	var inviterKey ed25519.PublicKey
	if a.InvitedBy == a.NodeID {
		// Fondateur : auto-signé, et le cercle dérive de sa clé.
		if CircleIDFor(a.SignPub) != circle {
			return d, errors.New("faux fondateur")
		}
		inviterKey = a.SignPub
	} else {
		k, ok := signer(a.InvitedBy)
		if !ok {
			return d, errors.New("invitant inconnu")
		}
		inviterKey = k
	}
	if !verify(inviterKey, ctxAdmission, f.Admission, f.AdmissionSig) {
		return d, errors.New("signature d'admission invalide")
	}
	if !verify(a.SignPub, ctxBody, f.Body, f.BodySig) {
		return d, errors.New("signature du corps invalide")
	}
	if !b.TunnelIP.Is4() {
		return d, errors.New("adresse de tunnel invalide")
	}
	if err := validBody(b); err != nil {
		return d, err
	}
	d.fiche = f
	return d, nil
}

// Plafonds des champs d'une fiche : un membre ne doit pas pouvoir publier une
// fiche démesurée que tous les autres stockeraient et afficheraient.
const (
	maxNameLen      = 64
	maxAnnounced    = 8
	maxEndpointLen  = 255
	maxObserved     = 256
	maxObservedAddr = 64
)

// CleanName rend un nom acceptable dans une fiche : sans caractère de
// contrôle, 64 caractères au plus, « membre » s'il ne reste rien.
func CleanName(n string) string {
	var out []rune
	for _, r := range strings.TrimSpace(n) {
		if !unicode.IsControl(r) && len(out) < maxNameLen {
			out = append(out, r)
		}
	}
	if len(out) == 0 {
		return "membre"
	}
	return string(out)
}

func validBody(b Body) error {
	if n := utf8.RuneCountInString(b.Name); n == 0 || n > maxNameLen {
		return errors.New("nom vide ou trop long")
	}
	for _, r := range b.Name {
		if unicode.IsControl(r) {
			return errors.New("caractère de contrôle dans le nom")
		}
	}
	if len(b.Announced) > maxAnnounced {
		return errors.New("trop d'endpoints annoncés")
	}
	for _, e := range b.Announced {
		if len(e) > maxEndpointLen {
			return errors.New("endpoint annoncé trop long")
		}
	}
	if len(b.Observed) > maxObserved {
		return errors.New("trop d'endpoints observés")
	}
	for k, v := range b.Observed {
		if len(k) > 64 || len(v) > maxObservedAddr {
			return errors.New("endpoint observé invalide")
		}
	}
	return nil
}

// LoadOrCreateSignKey lit la clé Ed25519 du nœud, ou la crée (0600).
func LoadOrCreateSignKey(path string) (ed25519.PrivateKey, error) {
	b, err := os.ReadFile(path)
	if err == nil {
		seed, derr := base64.StdEncoding.DecodeString(strings.TrimSpace(string(b)))
		if derr != nil || len(seed) != ed25519.SeedSize {
			return nil, fmt.Errorf("%s : clé de signature illisible", path)
		}
		return ed25519.NewKeyFromSeed(seed), nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("lecture de %s : %w", path, err)
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
		return nil, fmt.Errorf("écriture de la clé de signature : %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return nil, fmt.Errorf("écriture de la clé de signature : %w", err)
	}
	return ed25519.NewKeyFromSeed(seed), nil
}
