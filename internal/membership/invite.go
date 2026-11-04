package membership

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"time"
)

// CodePrefix précède chaque code d'invitation.
const CodePrefix = "cercle1:"

// Erreurs d'invitation, avec des messages destinés à l'utilisateur.
var (
	ErrInviteUnknown = errors.New("code d'invitation inconnu ou annulé")
	ErrInviteUsed    = errors.New("ce code d'invitation a déjà servi")
	ErrInviteExpired = errors.New("ce code d'invitation a expiré")
	ErrBadCode       = errors.New("code d'invitation illisible : vérifiez qu'il a été collé en entier")
)

// Code est le contenu d'un code d'invitation.
//
// Écart au PRD : pas de clé de groupe (remplacée par les signatures Ed25519),
// et une adresse temporaire pour la clé temporaire.
type Code struct {
	Circle     string     `json:"c"`
	Subnet     string     `json:"s"`
	Assigned   netip.Addr `json:"a"`  // adresse définitive du nouveau membre
	TempIP     netip.Addr `json:"ta"` // adresse de la clé temporaire
	TempKey    string     `json:"tk"` // clé privée WireGuard temporaire, base64
	Token      string     `json:"t"`
	Inviter    string     `json:"i"`  // node_id (clé publique WireGuard) de l'invitant
	InviterIP  netip.Addr `json:"ia"` // adresse de tunnel de l'invitant
	Endpoints  []string   `json:"e"`  // endpoints où joindre l'invitant
	ExpiresAt  int64      `json:"x"`
	InviterNom string     `json:"n,omitempty"`
}

// Encode sérialise le code : JSON compact, base64url, préfixe cercle1:.
func (c Code) Encode() string {
	b, _ := json.Marshal(c)
	return CodePrefix + base64.RawURLEncoding.EncodeToString(b)
}

// ParseCode lit un code collé par l'utilisateur.
func ParseCode(s string) (Code, error) {
	s = strings.Join(strings.Fields(s), "") // espaces et retours à la ligne des messageries
	rest, ok := strings.CutPrefix(s, CodePrefix)
	if !ok {
		return Code{}, ErrBadCode
	}
	b, err := base64.RawURLEncoding.DecodeString(rest)
	if err != nil {
		return Code{}, ErrBadCode
	}
	var c Code
	if err := json.Unmarshal(b, &c); err != nil || c.Circle == "" || c.Token == "" || !c.Assigned.Is4() || !c.TempIP.Is4() {
		return Code{}, ErrBadCode
	}
	return c, nil
}

// Invite est une invitation vue par son auteur.
type Invite struct {
	ID         string     `json:"id"`
	TempPubKey string     `json:"-"`
	TempIP     netip.Addr `json:"-"`
	AssignedIP netip.Addr `json:"assigned_ip"`
	CreatedAt  time.Time  `json:"created_at"`
	ExpiresAt  time.Time  `json:"expires_at"`
	UsedAt     time.Time  `json:"used_at,omitzero"`
	UsedBy     string     `json:"used_by,omitempty"`
}

// Pending dit si l'invitation peut encore servir.
func (i Invite) Pending(now time.Time) bool { return i.UsedAt.IsZero() && now.Before(i.ExpiresAt) }

func tokenHash(token string) []byte {
	h := sha256.Sum256([]byte("cercle1/jeton\x00" + token))
	return h[:]
}

// NewInvite crée une invitation. tempPriv et tempPub sont une paire de clés
// WireGuard neuve (base64) ; endpoints, les adresses où le nouveau membre
// pourra joindre ce nœud.
func (s *Service) NewInvite(ctx context.Context, ttl time.Duration, endpoints []string, tempPriv, tempPub string) (Code, Invite, error) {
	if len(endpoints) == 0 {
		return Code{}, Invite{}, errors.New("ce nœud n'annonce aucune adresse joignable (WG_PUBLIC_ENDPOINTS) : le nouveau membre ne pourrait pas le contacter")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.circle == "" {
		return Code{}, Invite{}, ErrNotInCircle
	}
	used := s.usedLocked(ctx)
	assigned, err := randomFree(s.subnet, used)
	if err != nil {
		return Code{}, Invite{}, err
	}
	used[assigned] = true
	tempIP, err := randomFree(s.subnet, used)
	if err != nil {
		return Code{}, Invite{}, err
	}
	tok := make([]byte, 32)
	idb := make([]byte, 6)
	_, _ = rand.Read(tok)
	_, _ = rand.Read(idb)
	token := base64.RawURLEncoding.EncodeToString(tok)
	now := s.now()
	inv := Invite{ID: hex.EncodeToString(idb), TempPubKey: tempPub, TempIP: tempIP, AssignedIP: assigned,
		CreatedAt: now, ExpiresAt: now.Add(ttl)}
	self := s.members[s.id.NodeID]
	if _, err := s.st.W.ExecContext(ctx, `INSERT INTO invites (id, token_hash, temp_pubkey, temp_ip, assigned_ip, created_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`, inv.ID, tokenHash(token), tempPub, tempIP.String(), assigned.String(),
		now.UnixNano(), inv.ExpiresAt.UnixNano()); err != nil {
		return Code{}, Invite{}, fmt.Errorf("enregistrement de l'invitation : %w", err)
	}
	code := Code{Circle: s.circle, Subnet: s.subnet.String(), Assigned: assigned, TempIP: tempIP, TempKey: tempPriv,
		Token: token, Inviter: s.id.NodeID, InviterIP: self.body.TunnelIP, Endpoints: endpoints,
		ExpiresAt: inv.ExpiresAt.UnixNano(), InviterNom: self.body.Name}
	return code, inv, nil
}

func scanInvite(row interface{ Scan(...any) error }) (Invite, error) {
	var inv Invite
	var temp, assigned string
	var created, expires, used int64
	if err := row.Scan(&inv.ID, &inv.TempPubKey, &temp, &assigned, &created, &expires, &used, &inv.UsedBy); err != nil {
		return inv, err
	}
	inv.TempIP, _ = netip.ParseAddr(temp)
	inv.AssignedIP, _ = netip.ParseAddr(assigned)
	inv.CreatedAt, inv.ExpiresAt = time.Unix(0, created), time.Unix(0, expires)
	if used > 0 {
		inv.UsedAt = time.Unix(0, used)
	}
	return inv, nil
}

const inviteCols = "id, temp_pubkey, temp_ip, assigned_ip, created_at, expires_at, used_at, used_by"

// Invites renvoie les invitations non annulées, les plus récentes d'abord.
func (s *Service) Invites(ctx context.Context) ([]Invite, error) {
	rows, err := s.st.R.QueryContext(ctx, "SELECT "+inviteCols+" FROM invites WHERE cancelled = 0 ORDER BY created_at DESC")
	if err != nil {
		return nil, fmt.Errorf("lecture des invitations : %w", err)
	}
	defer rows.Close()
	var out []Invite
	for rows.Next() {
		inv, err := scanInvite(rows)
		if err != nil {
			return nil, fmt.Errorf("lecture des invitations : %w", err)
		}
		out = append(out, inv)
	}
	return out, rows.Err()
}

// CancelInvite annule une invitation encore inutilisée.
func (s *Service) CancelInvite(ctx context.Context, id string) error {
	res, err := s.st.W.ExecContext(ctx, "UPDATE invites SET cancelled = 1 WHERE id = ? AND used_at = 0", id)
	if err != nil {
		return fmt.Errorf("annulation de l'invitation : %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrInviteUnknown
	}
	return nil
}

// TempPeers renvoie les clés temporaires à déclarer dans WireGuard : celles
// des invitations en attente, et celles utilisées depuis moins de 2 minutes
// (le temps que la réponse de /join arrive).
func (s *Service) TempPeers(ctx context.Context) []Invite {
	now := s.now()
	rows, err := s.st.R.QueryContext(ctx, "SELECT "+inviteCols+` FROM invites
		WHERE cancelled = 0 AND ((used_at = 0 AND expires_at > ?) OR used_at > ?)`, now.UnixNano(), now.Add(-2*time.Minute).UnixNano())
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []Invite
	for rows.Next() {
		if inv, err := scanInvite(rows); err == nil {
			out = append(out, inv)
		}
	}
	return out
}

// InviteByTempIP dit si une adresse est celle d'une clé temporaire en cours.
func (s *Service) InviteByTempIP(ctx context.Context, ip netip.Addr) bool {
	for _, inv := range s.TempPeers(ctx) {
		if inv.TempIP == ip {
			return true
		}
	}
	return false
}

// JoinRequest est envoyée par le nouveau membre, avec la clé temporaire.
type JoinRequest struct {
	Token   string          `json:"token"`
	NodeID  string          `json:"node_id"`  // vraie clé publique WireGuard
	SignPub []byte          `json:"sign_pub"` // clé publique Ed25519
	Body    json.RawMessage `json:"body"`     // corps de sa fiche, déjà signé par lui
	BodySig []byte          `json:"body_sig"`
}

// JoinResponse est la réponse de l'invitant.
type JoinResponse struct {
	Admission    json.RawMessage `json:"admission"`
	AdmissionSig []byte          `json:"admission_sig"`
	Table        Table           `json:"table"`
}

// AcceptJoin traite une demande d'adhésion arrivée depuis l'adresse temporaire
// tempIP. Une demande répétée par le même nœud (réponse perdue) reçoit la
// même réponse.
func (s *Service) AcceptJoin(ctx context.Context, tempIP netip.Addr, req JoinRequest) (JoinResponse, error) {
	s.joinMu.Lock()
	defer s.joinMu.Unlock()
	var (
		id, usedBy, response string
		hash                 []byte
		assignedS            string
		expires, used        int64
	)
	err := s.st.R.QueryRowContext(ctx, `SELECT id, token_hash, assigned_ip, expires_at, used_at, used_by, response
		FROM invites WHERE temp_ip = ? AND cancelled = 0 ORDER BY created_at DESC LIMIT 1`, tempIP.String()).
		Scan(&id, &hash, &assignedS, &expires, &used, &usedBy, &response)
	if errors.Is(err, sql.ErrNoRows) {
		return JoinResponse{}, ErrInviteUnknown
	}
	if err != nil {
		return JoinResponse{}, fmt.Errorf("lecture de l'invitation : %w", err)
	}
	if subtle.ConstantTimeCompare(hash, tokenHash(req.Token)) != 1 {
		return JoinResponse{}, ErrInviteUnknown
	}
	if used > 0 {
		if usedBy == req.NodeID && response != "" {
			var resp JoinResponse
			if err := json.Unmarshal([]byte(response), &resp); err == nil {
				return resp, nil
			}
		}
		return JoinResponse{}, ErrInviteUsed
	}
	if s.now().UnixNano() >= expires {
		return JoinResponse{}, ErrInviteExpired
	}
	assigned, _ := netip.ParseAddr(assignedS)

	s.mu.Lock()
	circle := s.circle
	if _, known := s.members[req.NodeID]; known {
		s.mu.Unlock()
		return JoinResponse{}, errors.New("ce nœud est déjà membre du cercle")
	}
	s.mu.Unlock()
	var b Body
	if len(req.SignPub) != ed25519.PublicKeySize || !verify(req.SignPub, ctxBody, req.Body, req.BodySig) ||
		json.Unmarshal(req.Body, &b) != nil || b.NodeID != req.NodeID || b.Circle != circle || b.TunnelIP != assigned {
		return JoinResponse{}, errors.New("demande d'adhésion invalide")
	}
	adm, admSig, err := NewAdmission(s.id.Sign, Admission{Circle: circle, NodeID: req.NodeID, SignPub: req.SignPub,
		JoinedAt: s.now().UnixNano(), InvitedBy: s.id.NodeID})
	if err != nil {
		return JoinResponse{}, err
	}
	f := Fiche{Admission: adm, AdmissionSig: admSig, Body: req.Body, BodySig: req.BodySig}
	if _, err := s.Merge(ctx, Table{Fiches: []Fiche{f}}); err != nil {
		return JoinResponse{}, err
	}
	if _, ok := s.Member(req.NodeID); !ok {
		return JoinResponse{}, errors.New("fiche du nouveau membre refusée")
	}
	resp := JoinResponse{Admission: adm, AdmissionSig: admSig, Table: s.Table()}
	raw, _ := json.Marshal(resp)
	res, err := s.st.W.ExecContext(ctx, `UPDATE invites SET used_at = ?, used_by = ?, response = ? WHERE id = ? AND used_at = 0`,
		s.now().UnixNano(), req.NodeID, string(raw), id)
	if err != nil {
		return JoinResponse{}, fmt.Errorf("enregistrement de l'adhésion : %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return JoinResponse{}, ErrInviteUsed // deux demandes concurrentes : la première a gagné
	}
	s.log.Info("nouveau membre admis", "node_id", req.NodeID, "name", b.Name, "tunnel_ip", assigned)
	return resp, nil
}

// PrepareJoin construit la demande d'adhésion du nœud local.
func (s *Service) PrepareJoin(code Code, name string, announced []string) (JoinRequest, error) {
	if s.InCircle() {
		return JoinRequest{}, ErrAlreadyInCircle
	}
	if s.now().UnixNano() >= code.ExpiresAt {
		return JoinRequest{}, ErrInviteExpired
	}
	body, sig, err := NewBody(s.id.Sign, Body{Circle: code.Circle, NodeID: s.id.NodeID, Name: name,
		TunnelIP: code.Assigned, Announced: announced, Version: 1, UpdatedAt: s.now().UnixNano()})
	if err != nil {
		return JoinRequest{}, err
	}
	return JoinRequest{Token: code.Token, NodeID: s.id.NodeID, SignPub: s.id.Sign.Public().(ed25519.PublicKey), Body: body, BodySig: sig}, nil
}

// CompleteJoin enregistre l'adhésion après la réponse de l'invitant.
func (s *Service) CompleteJoin(ctx context.Context, code Code, req JoinRequest, resp JoinResponse) error {
	subnet, err := netip.ParsePrefix(code.Subnet)
	if err != nil {
		return ErrBadCode
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.circle != "" {
		return ErrAlreadyInCircle
	}
	self := Fiche{Admission: resp.Admission, AdmissionSig: resp.AdmissionSig, Body: req.Body, BodySig: req.BodySig}
	return s.enterLocked(ctx, code.Circle, subnet, self, resp.Table)
}
