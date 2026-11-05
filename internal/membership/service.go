package membership

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/netip"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/DB-CL/cercle/internal/store"
)

// Identity est l'identité du nœud local.
type Identity struct {
	NodeID string             // clé publique WireGuard, base64
	Sign   ed25519.PrivateKey // clé de signature
}

// Member est la vue d'un membre vérifié.
type Member struct {
	NodeID      string            `json:"node_id"`
	Name        string            `json:"name"`
	DisplayName string            `json:"display_name"` // nom rendu unique dans le cercle
	TunnelIP    netip.Addr        `json:"tunnel_ip"`
	Announced   []string          `json:"announced,omitempty"`
	Observed    map[string]string `json:"-"`
	Version     int64             `json:"version"`
	JoinedAt    time.Time         `json:"joined_at"`
	UpdatedAt   time.Time         `json:"updated_at"`
	InvitedBy   string            `json:"invited_by"`
	Revoked     bool              `json:"revoked"`
	RevokedBy   string            `json:"revoked_by,omitempty"`
	Self        bool              `json:"self"`
	Conflict    bool              `json:"conflict,omitempty"` // adresse perdue au profit d'un membre plus ancien
}

type revocation struct {
	data RevocationData
	rev  Revocation
}

// Service tient l'état du cercle, en mémoire et en base.
type Service struct {
	st  *store.Store
	id  Identity
	log *slog.Logger
	now func() time.Time

	joinMu  sync.Mutex // une adhésion à la fois : un code ne fait entrer qu'un membre
	mu      sync.Mutex
	circle  string
	subnet  netip.Prefix
	members map[string]decoded
	revoked map[string]revocation
}

// Erreurs remontées à l'interface.
var (
	ErrNotInCircle     = errors.New("ce nœud n'appartient à aucun cercle")
	ErrAlreadyInCircle = errors.New("ce nœud appartient déjà à un cercle")
)

// New charge l'état du cercle depuis la base, en revérifiant chaque signature.
func New(ctx context.Context, st *store.Store, id Identity, log *slog.Logger) (*Service, error) {
	if log == nil {
		log = slog.Default()
	}
	s := &Service{st: st, id: id, log: log.With("module", "membership"), now: time.Now,
		members: map[string]decoded{}, revoked: map[string]revocation{}}
	var subnet string
	err := st.R.QueryRowContext(ctx, "SELECT circle_id, subnet FROM circle WHERE id = 1").Scan(&s.circle, &subnet)
	if errors.Is(err, sql.ErrNoRows) {
		return s, nil
	}
	if err != nil {
		return nil, fmt.Errorf("lecture du cercle : %w", err)
	}
	if s.subnet, err = netip.ParsePrefix(subnet); err != nil {
		return nil, fmt.Errorf("sous-réseau du cercle illisible : %w", err)
	}
	var t Table
	rows, err := st.R.QueryContext(ctx, "SELECT fiche FROM members")
	if err != nil {
		return nil, fmt.Errorf("lecture des membres : %w", err)
	}
	for rows.Next() {
		var raw string
		var f Fiche
		if err := rows.Scan(&raw); err == nil && json.Unmarshal([]byte(raw), &f) == nil {
			t.Fiches = append(t.Fiches, f)
		}
	}
	rows.Close()
	rows, err = st.R.QueryContext(ctx, "SELECT data FROM revocations")
	if err != nil {
		return nil, fmt.Errorf("lecture des révocations : %w", err)
	}
	for rows.Next() {
		var raw string
		var r Revocation
		if err := rows.Scan(&raw); err == nil && json.Unmarshal([]byte(raw), &r) == nil {
			t.Revocations = append(t.Revocations, r)
		}
	}
	rows.Close()
	s.members, s.revoked = s.resolve(nil, t)
	if _, ok := s.members[id.NodeID]; !ok {
		return nil, errors.New("la fiche de ce nœud est absente ou invalide : base incohérente")
	}
	return s, nil
}

// InCircle dit si le nœud appartient à un cercle.
func (s *Service) InCircle() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.circle != ""
}

// Circle renvoie l'identifiant et le sous-réseau du cercle.
func (s *Service) Circle() (string, netip.Prefix) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.circle, s.subnet
}

// Create fonde un cercle : le fondateur prend la première adresse du sous-réseau.
func (s *Service) Create(ctx context.Context, subnet netip.Prefix, name string, announced []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.circle != "" {
		return ErrAlreadyInCircle
	}
	circle := CircleIDFor(s.id.Sign.Public().(ed25519.PublicKey))
	ip := subnet.Masked().Addr().Next()
	now := s.now().UnixNano()
	adm, admSig, err := NewAdmission(s.id.Sign, Admission{Circle: circle, NodeID: s.id.NodeID,
		SignPub: s.id.Sign.Public().(ed25519.PublicKey), JoinedAt: now, InvitedBy: s.id.NodeID})
	if err != nil {
		return err
	}
	body, bodySig, err := NewBody(s.id.Sign, Body{Circle: circle, NodeID: s.id.NodeID, Name: CleanName(name),
		TunnelIP: ip, Announced: announced, Version: 1, UpdatedAt: now})
	if err != nil {
		return err
	}
	f := Fiche{Admission: adm, AdmissionSig: admSig, Body: body, BodySig: bodySig}
	return s.enterLocked(ctx, circle, subnet.Masked(), f, Table{})
}

// enterLocked enregistre le cercle et la fiche du nœud, puis fusionne t.
func (s *Service) enterLocked(ctx context.Context, circle string, subnet netip.Prefix, self Fiche, t Table) error {
	members, revoked := s.resolveFor(circle, subnet, Table{Fiches: append([]Fiche{self}, t.Fiches...), Revocations: t.Revocations})
	if _, ok := members[s.id.NodeID]; !ok {
		return errors.New("fiche d'adhésion invalide (chaîne d'invitations incomplète)")
	}
	tx, err := s.st.W.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("enregistrement du cercle : %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, "INSERT INTO circle (id, circle_id, subnet, created_at) VALUES (1, ?, ?, ?)",
		circle, subnet.String(), s.now().UnixNano()); err != nil {
		return fmt.Errorf("enregistrement du cercle : %w", err)
	}
	if err := saveTx(ctx, tx, members, revoked); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("enregistrement du cercle : %w", err)
	}
	s.circle, s.subnet, s.members, s.revoked = circle, subnet, members, revoked
	return nil
}

func saveTx(ctx context.Context, tx *sql.Tx, members map[string]decoded, revoked map[string]revocation) error {
	if _, err := tx.ExecContext(ctx, "DELETE FROM members"); err != nil {
		return fmt.Errorf("enregistrement des membres : %w", err)
	}
	for id, d := range members {
		raw, _ := json.Marshal(d.fiche)
		if _, err := tx.ExecContext(ctx, "INSERT INTO members (node_id, fiche, tunnel_ip, version, joined_at, updated_at) VALUES (?, ?, ?, ?, ?, ?)",
			id, string(raw), d.body.TunnelIP.String(), d.body.Version, d.adm.JoinedAt, d.body.UpdatedAt); err != nil {
			return fmt.Errorf("enregistrement des membres : %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM revocations"); err != nil {
		return fmt.Errorf("enregistrement des révocations : %w", err)
	}
	for target, r := range revoked {
		raw, _ := json.Marshal(r.rev)
		if _, err := tx.ExecContext(ctx, "INSERT INTO revocations (target, data) VALUES (?, ?)", target, string(raw)); err != nil {
			return fmt.Errorf("enregistrement des révocations : %w", err)
		}
	}
	return nil
}

func (s *Service) saveLocked(ctx context.Context, members map[string]decoded, revoked map[string]revocation) error {
	tx, err := s.st.W.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("enregistrement des membres : %w", err)
	}
	defer tx.Rollback()
	if err := saveTx(ctx, tx, members, revoked); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("enregistrement des membres : %w", err)
	}
	s.members, s.revoked = members, revoked
	return nil
}

// resolve valide la table actuelle complétée par t.
func (s *Service) resolve(cur map[string]decoded, t Table) (map[string]decoded, map[string]revocation) {
	all := Table{Fiches: slices.Clone(t.Fiches), Revocations: slices.Clone(t.Revocations)}
	for _, d := range cur {
		all.Fiches = append(all.Fiches, d.fiche)
	}
	for _, r := range s.revoked {
		all.Revocations = append(all.Revocations, r.rev)
	}
	return s.resolveFor(s.circle, s.subnet, all)
}

// resolveFor ne garde que les fiches dont la chaîne d'admissions remonte au
// fondateur et dont les signatures sont valides ; pour chaque membre, la plus
// haute version gagne. Les révocations valides sont absorbantes : une fois
// révoqué, un membre le reste, quelle que soit la version de sa fiche.
func (s *Service) resolveFor(circle string, subnet netip.Prefix, t Table) (map[string]decoded, map[string]revocation) {
	cands := map[string][]Fiche{}
	for _, f := range t.Fiches {
		var a Admission
		if json.Unmarshal(f.Admission, &a) == nil && a.NodeID != "" {
			cands[a.NodeID] = append(cands[a.NodeID], f)
		}
	}
	valid := map[string]decoded{}
	signer := func(id string) (ed25519.PublicKey, bool) {
		d, ok := valid[id]
		return d.adm.SignPub, ok
	}
	for progress := true; progress; {
		progress = false
		for id, fs := range cands {
			if _, ok := valid[id]; ok {
				continue
			}
			var best *decoded
			for _, f := range fs {
				d, err := check(f, circle, signer)
				if err != nil {
					continue
				}
				if subnet.IsValid() && !subnet.Contains(d.body.TunnelIP) {
					continue // adresse hors du sous-réseau du cercle
				}
				if best == nil || d.body.Version > best.body.Version {
					best = &d
				}
			}
			if best != nil {
				valid[id] = *best
				progress = true
			}
		}
	}
	revoked := map[string]revocation{}
	for _, r := range t.Revocations {
		var d RevocationData
		if json.Unmarshal(r.Data, &d) != nil || d.Circle != circle {
			continue
		}
		by, ok1 := valid[d.By]
		_, ok2 := valid[d.Target]
		if !ok1 || !ok2 || !verify(by.adm.SignPub, ctxRevocation, r.Data, r.Sig) {
			continue
		}
		if old, ok := revoked[d.Target]; !ok || d.At < old.data.At {
			revoked[d.Target] = revocation{data: d, rev: r}
		}
	}
	return valid, revoked
}

func sameState(a map[string]decoded, b map[string]decoded, ra, rb map[string]revocation) bool {
	if len(a) != len(b) || len(ra) != len(rb) {
		return false
	}
	for id, d := range a {
		o, ok := b[id]
		if !ok || !bytes.Equal(d.fiche.Body, o.fiche.Body) || !bytes.Equal(d.fiche.Admission, o.fiche.Admission) {
			return false
		}
	}
	for id := range ra {
		if _, ok := rb[id]; !ok {
			return false
		}
	}
	return true
}

// Merge fusionne une table reçue. changed dit si l'état local a changé.
func (s *Service) Merge(ctx context.Context, t Table) (changed bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.circle == "" {
		return false, ErrNotInCircle
	}
	mine := s.members[s.id.NodeID]
	members, revoked := s.resolve(s.members, t)
	// Ma fiche : c'est moi qui fais foi. Si une version plus haute circule
	// (base restaurée), je repars au-dessus avec mon contenu actuel.
	if got, ok := members[s.id.NodeID]; ok && !bytes.Equal(got.fiche.Body, mine.fiche.Body) {
		if got.body.Version >= mine.body.Version {
			b := mine.body
			b.Version = got.body.Version + 1
			raw, sig, err := NewBody(s.id.Sign, b)
			if err != nil {
				return false, err
			}
			mine.body, mine.fiche.Body, mine.fiche.BodySig = b, raw, sig
		}
		members[s.id.NodeID] = mine
	}
	if sameState(s.members, members, s.revoked, revoked) {
		return false, nil
	}
	if err := s.saveLocked(ctx, members, revoked); err != nil {
		return false, err
	}
	return true, nil
}

// Table renvoie la table complète à envoyer par gossip.
func (s *Service) Table() Table {
	s.mu.Lock()
	defer s.mu.Unlock()
	var t Table
	for _, d := range s.members {
		t.Fiches = append(t.Fiches, d.fiche)
	}
	for _, r := range s.revoked {
		t.Revocations = append(t.Revocations, r.rev)
	}
	return t
}

// UpdateSelf modifie le corps de la fiche locale et la re-signe, si fn y
// change quelque chose. Renvoie vrai si la fiche a changé.
func (s *Service) UpdateSelf(ctx context.Context, fn func(b *Body)) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.circle == "" {
		return false, ErrNotInCircle
	}
	mine := s.members[s.id.NodeID]
	b := mine.body
	b.Announced = slices.Clone(b.Announced)
	b.Observed = maps.Clone(b.Observed)
	fn(&b)
	b.Name = CleanName(b.Name)
	before, _ := json.Marshal(mine.body)
	after, _ := json.Marshal(b)
	if bytes.Equal(before, after) {
		return false, nil
	}
	b.Version = max(mine.body.Version+1, s.now().UnixMilli())
	b.UpdatedAt = s.now().UnixNano()
	raw, sig, err := NewBody(s.id.Sign, b)
	if err != nil {
		return false, err
	}
	mine.body, mine.fiche.Body, mine.fiche.BodySig = b, raw, sig
	members := maps.Clone(s.members)
	members[s.id.NodeID] = mine
	if err := s.saveLocked(ctx, members, s.revoked); err != nil {
		return false, err
	}
	return true, nil
}

// Revoke révoque définitivement un membre.
func (s *Service) Revoke(ctx context.Context, target string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.circle == "" {
		return ErrNotInCircle
	}
	if target == s.id.NodeID {
		return errors.New("un membre ne peut pas se révoquer lui-même")
	}
	if _, ok := s.members[target]; !ok {
		return fmt.Errorf("membre inconnu : %s", target)
	}
	if _, ok := s.revoked[target]; ok {
		return nil
	}
	raw, sig, err := signedJSON(s.id.Sign, ctxRevocation, RevocationData{Circle: s.circle, Target: target, By: s.id.NodeID, At: s.now().UnixNano()})
	if err != nil {
		return err
	}
	members, revoked := s.resolve(s.members, Table{Revocations: []Revocation{{Data: raw, Sig: sig}}})
	if _, ok := revoked[target]; !ok {
		return errors.New("révocation refusée")
	}
	return s.saveLocked(ctx, members, revoked)
}

// owners attribue chaque adresse de tunnel : en cas de conflit, le membre
// entré le plus tôt la garde (puis le plus petit node_id). Les révoqués
// gardent la leur : une adresse n'est jamais réattribuée.
func ownersLocked(members map[string]decoded) map[netip.Addr]string {
	ids := slices.Collect(maps.Keys(members))
	sort.Slice(ids, func(i, j int) bool {
		a, b := members[ids[i]].adm, members[ids[j]].adm
		if a.JoinedAt != b.JoinedAt {
			return a.JoinedAt < b.JoinedAt
		}
		return ids[i] < ids[j]
	})
	owners := map[netip.Addr]string{}
	for _, id := range ids {
		ip := members[id].body.TunnelIP
		if _, taken := owners[ip]; !taken {
			owners[ip] = id
		}
	}
	return owners
}

// Members renvoie tous les membres connus, par date d'entrée.
func (s *Service) Members() []Member {
	s.mu.Lock()
	defer s.mu.Unlock()
	owners := ownersLocked(s.members)
	names := map[string]int{}
	for id, d := range s.members {
		if _, rev := s.revoked[id]; !rev {
			names[d.body.Name]++
		}
	}
	var out []Member
	for id, d := range s.members {
		m := Member{
			NodeID: id, Name: d.body.Name, DisplayName: d.body.Name, TunnelIP: d.body.TunnelIP,
			Announced: d.body.Announced, Observed: d.body.Observed, Version: d.body.Version,
			JoinedAt: time.Unix(0, d.adm.JoinedAt), UpdatedAt: time.Unix(0, d.body.UpdatedAt),
			InvitedBy: d.adm.InvitedBy, Self: id == s.id.NodeID, Conflict: owners[d.body.TunnelIP] != id,
		}
		if r, ok := s.revoked[id]; ok {
			m.Revoked, m.RevokedBy = true, r.data.By
		}
		if names[m.Name] > 1 && len(id) >= 4 {
			m.DisplayName = m.Name + " (" + id[:4] + ")"
		}
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].JoinedAt.Equal(out[j].JoinedAt) {
			return out[i].JoinedAt.Before(out[j].JoinedAt)
		}
		return out[i].NodeID < out[j].NodeID
	})
	return out
}

// Self renvoie la vue du nœud local.
func (s *Service) Self() (Member, bool) {
	for _, m := range s.Members() {
		if m.Self {
			return m, true
		}
	}
	return Member{}, false
}

// Member renvoie un membre par son identifiant.
func (s *Service) Member(id string) (Member, bool) {
	for _, m := range s.Members() {
		if m.NodeID == id {
			return m, true
		}
	}
	return Member{}, false
}

// ByIP renvoie le membre non révoqué qui possède une adresse de tunnel.
func (s *Service) ByIP(ip netip.Addr) (Member, bool) {
	for _, m := range s.Members() {
		if m.TunnelIP == ip && !m.Revoked && !m.Conflict {
			return m, true
		}
	}
	return Member{}, false
}

// usedLocked renvoie toutes les adresses prises : membres (même révoqués) et
// invitations en attente.
func (s *Service) usedLocked(ctx context.Context) map[netip.Addr]bool {
	used := map[netip.Addr]bool{}
	for _, d := range s.members {
		used[d.body.TunnelIP] = true
	}
	rows, err := s.st.R.QueryContext(ctx, `SELECT temp_ip, assigned_ip FROM invites WHERE cancelled = 0 AND (used_at = 0 AND expires_at > ?)`, s.now().UnixNano())
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var a, b string
			if rows.Scan(&a, &b) == nil {
				if ip, err := netip.ParseAddr(a); err == nil {
					used[ip] = true
				}
				if ip, err := netip.ParseAddr(b); err == nil {
					used[ip] = true
				}
			}
		}
	}
	return used
}

// randomFree tire une adresse libre au hasard dans le sous-réseau : deux
// invitations simultanées chez deux membres différents ne tombent presque
// jamais sur la même.
func randomFree(subnet netip.Prefix, used map[netip.Addr]bool) (netip.Addr, error) {
	base := subnet.Masked().Addr().As4()
	hostBits := 32 - subnet.Bits()
	n := uint32(1)<<hostBits - 2 // sans l'adresse de réseau ni de diffusion
	start := binary.BigEndian.Uint32(base[:])
	var free []netip.Addr
	for i := uint32(1); i <= n && len(free) < 4096; i++ {
		var b [4]byte
		binary.BigEndian.PutUint32(b[:], start+i)
		ip := netip.AddrFrom4(b)
		if !used[ip] {
			free = append(free, ip)
		}
	}
	if len(free) == 0 {
		return netip.Addr{}, errors.New("plus aucune adresse libre dans le sous-réseau du cercle")
	}
	var r [4]byte
	_, _ = rand.Read(r[:])
	return free[binary.BigEndian.Uint32(r[:])%uint32(len(free))], nil
}

// ResolveConflict vérifie si le nœud local a perdu son adresse au profit d'un
// membre plus ancien ; si oui, il en prend une libre et republie sa fiche.
// Renvoie la nouvelle adresse, ou une adresse invalide si rien n'a changé.
func (s *Service) ResolveConflict(ctx context.Context) (netip.Addr, error) {
	s.mu.Lock()
	mine, ok := s.members[s.id.NodeID]
	if !ok || ownersLocked(s.members)[mine.body.TunnelIP] == s.id.NodeID {
		s.mu.Unlock()
		return netip.Addr{}, nil
	}
	ip, err := randomFree(s.subnet, s.usedLocked(ctx))
	s.mu.Unlock()
	if err != nil {
		return netip.Addr{}, err
	}
	s.log.Warn("conflit d'adresse : un membre plus ancien garde l'adresse, nouvelle adresse choisie", "old", mine.body.TunnelIP, "new", ip)
	if _, err := s.UpdateSelf(ctx, func(b *Body) { b.TunnelIP = ip }); err != nil {
		return netip.Addr{}, err
	}
	return ip, nil
}
