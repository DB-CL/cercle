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

// CodePrefix precedes every invitation code.
const CodePrefix = "cercle1:"

// Invitation errors, with messages meant for the user.
var (
	ErrInviteUnknown = errors.New("unknown or cancelled invitation code")
	ErrInviteUsed    = errors.New("this invitation code has already been used")
	ErrInviteExpired = errors.New("this invitation code has expired")
	ErrBadCode       = errors.New("unreadable invitation code: check that it was pasted in full")
)

// Code is the content of an invitation code. Its JSON field names are part of
// the wire format.
type Code struct {
	Circle      string     `json:"c"`
	Subnet      string     `json:"s"`
	Assigned    netip.Addr `json:"a"`  // final address of the new member
	TempIP      netip.Addr `json:"ta"` // address of the temporary key
	TempKey     string     `json:"tk"` // temporary WireGuard private key, base64
	Token       string     `json:"t"`
	Inviter     string     `json:"i"`  // inviter's node_id (WireGuard public key)
	InviterIP   netip.Addr `json:"ia"` // inviter's tunnel address
	Endpoints   []string   `json:"e"`  // endpoints where the inviter can be reached
	ExpiresAt   int64      `json:"x"`
	InviterName string     `json:"n,omitempty"`
}

// Encode serializes the code: compact JSON, base64url, cercle1: prefix.
func (c Code) Encode() string {
	b, _ := json.Marshal(c)
	return CodePrefix + base64.RawURLEncoding.EncodeToString(b)
}

// ParseCode reads a code pasted by the user.
func ParseCode(s string) (Code, error) {
	s = strings.Join(strings.Fields(s), "") // spaces and line breaks added by messaging apps
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

// Invite is an invitation as seen by its author.
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

// Pending reports whether the invitation can still be used.
func (i Invite) Pending(now time.Time) bool { return i.UsedAt.IsZero() && now.Before(i.ExpiresAt) }

func tokenHash(token string) []byte {
	h := sha256.Sum256([]byte("cercle1/jeton\x00" + token))
	return h[:]
}

// NewInvite creates an invitation. tempPriv and tempPub are a fresh WireGuard
// key pair (base64); endpoints are the addresses where the new member will
// reach this node.
func (s *Service) NewInvite(ctx context.Context, ttl time.Duration, endpoints []string, tempPriv, tempPub string) (Code, Invite, error) {
	if len(endpoints) == 0 {
		return Code{}, Invite{}, errors.New("this node announces no reachable address (WG_PUBLIC_ENDPOINTS): the new member could not contact it")
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
		return Code{}, Invite{}, fmt.Errorf("storing the invitation: %w", err)
	}
	code := Code{Circle: s.circle, Subnet: s.subnet.String(), Assigned: assigned, TempIP: tempIP, TempKey: tempPriv,
		Token: token, Inviter: s.id.NodeID, InviterIP: self.body.TunnelIP, Endpoints: endpoints,
		ExpiresAt: inv.ExpiresAt.UnixNano(), InviterName: self.body.Name}
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

// Invites returns the non-cancelled invitations, newest first.
func (s *Service) Invites(ctx context.Context) ([]Invite, error) {
	rows, err := s.st.R.QueryContext(ctx, "SELECT "+inviteCols+" FROM invites WHERE cancelled = 0 ORDER BY created_at DESC")
	if err != nil {
		return nil, fmt.Errorf("reading invitations: %w", err)
	}
	defer rows.Close()
	var out []Invite
	for rows.Next() {
		inv, err := scanInvite(rows)
		if err != nil {
			return nil, fmt.Errorf("reading invitations: %w", err)
		}
		out = append(out, inv)
	}
	return out, rows.Err()
}

// CancelInvite cancels an invitation not used yet.
func (s *Service) CancelInvite(ctx context.Context, id string) error {
	res, err := s.st.W.ExecContext(ctx, "UPDATE invites SET cancelled = 1 WHERE id = ? AND used_at = 0", id)
	if err != nil {
		return fmt.Errorf("cancelling the invitation: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrInviteUnknown
	}
	return nil
}

// TempPeers returns the temporary keys to declare in WireGuard: those of
// pending invitations, and those used less than 2 minutes ago (time for the
// /join response to arrive).
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

// InviteByTempIP reports whether an address belongs to a current temporary key.
func (s *Service) InviteByTempIP(ctx context.Context, ip netip.Addr) bool {
	for _, inv := range s.TempPeers(ctx) {
		if inv.TempIP == ip {
			return true
		}
	}
	return false
}

// JoinRequest is sent by the new member, with the temporary key.
type JoinRequest struct {
	Token   string          `json:"token"`
	NodeID  string          `json:"node_id"`  // real WireGuard public key
	SignPub []byte          `json:"sign_pub"` // Ed25519 public key
	Body    json.RawMessage `json:"body"`     // body of its record, already signed by itself
	BodySig []byte          `json:"body_sig"`
}

// JoinResponse is the inviter's response.
type JoinResponse struct {
	Admission    json.RawMessage `json:"admission"`
	AdmissionSig []byte          `json:"admission_sig"`
	Table        Table           `json:"table"`
}

// AcceptJoin handles a join request coming from the temporary address
// tempIP. A request repeated by the same node (lost response) gets the same
// response.
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
		return JoinResponse{}, fmt.Errorf("reading the invitation: %w", err)
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
		return JoinResponse{}, errors.New("this node is already a member of the circle")
	}
	s.mu.Unlock()
	var b Body
	if len(req.SignPub) != ed25519.PublicKeySize || !verify(req.SignPub, ctxBody, req.Body, req.BodySig) ||
		json.Unmarshal(req.Body, &b) != nil || b.NodeID != req.NodeID || b.Circle != circle || b.TunnelIP != assigned {
		return JoinResponse{}, errors.New("invalid join request")
	}
	adm, admSig, err := NewAdmission(s.id.Sign, Admission{Circle: circle, NodeID: req.NodeID, SignPub: req.SignPub,
		JoinedAt: s.now().UnixNano(), InvitedBy: s.id.NodeID})
	if err != nil {
		return JoinResponse{}, err
	}
	f := Record{Admission: adm, AdmissionSig: admSig, Body: req.Body, BodySig: req.BodySig}
	if _, err := s.Merge(ctx, Table{Records: []Record{f}}); err != nil {
		return JoinResponse{}, err
	}
	if _, ok := s.Member(req.NodeID); !ok {
		return JoinResponse{}, errors.New("new member's record refused")
	}
	resp := JoinResponse{Admission: adm, AdmissionSig: admSig, Table: s.Table()}
	raw, _ := json.Marshal(resp)
	res, err := s.st.W.ExecContext(ctx, `UPDATE invites SET used_at = ?, used_by = ?, response = ? WHERE id = ? AND used_at = 0`,
		s.now().UnixNano(), req.NodeID, string(raw), id)
	if err != nil {
		return JoinResponse{}, fmt.Errorf("storing the membership: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return JoinResponse{}, ErrInviteUsed // two concurrent requests: the first one won
	}
	s.log.Info("new member admitted", "node_id", req.NodeID, "name", b.Name, "tunnel_ip", assigned)
	return resp, nil
}

// PrepareJoin builds the local node's join request.
func (s *Service) PrepareJoin(code Code, name string, announced []string) (JoinRequest, error) {
	if s.InCircle() {
		return JoinRequest{}, ErrAlreadyInCircle
	}
	if s.now().UnixNano() >= code.ExpiresAt {
		return JoinRequest{}, ErrInviteExpired
	}
	body, sig, err := NewBody(s.id.Sign, Body{Circle: code.Circle, NodeID: s.id.NodeID, Name: CleanName(name),
		TunnelIP: code.Assigned, Announced: announced, Version: 1, UpdatedAt: s.now().UnixNano()})
	if err != nil {
		return JoinRequest{}, err
	}
	return JoinRequest{Token: code.Token, NodeID: s.id.NodeID, SignPub: s.id.Sign.Public().(ed25519.PublicKey), Body: body, BodySig: sig}, nil
}

// CompleteJoin stores the membership after the inviter's response.
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
	self := Record{Admission: resp.Admission, AdmissionSig: resp.AdmissionSig, Body: req.Body, BodySig: req.BodySig}
	return s.enterLocked(ctx, code.Circle, subnet, self, resp.Table)
}
