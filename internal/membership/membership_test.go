package membership

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/netip"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/DB-CL/cercle/internal/store"
)

type tnode struct {
	t    *testing.T
	s    *Service
	st   *store.Store
	db   string
	id   Identity
	name string
}

func randID() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return base64.StdEncoding.EncodeToString(b)
}

func newNode(t *testing.T, name string) *tnode {
	t.Helper()
	db := filepath.Join(t.TempDir(), "n.db")
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	n := &tnode{t: t, db: db, id: Identity{NodeID: randID(), Sign: priv}, name: name}
	n.open()
	return n
}

func (n *tnode) open() {
	n.t.Helper()
	st, err := store.Open(context.Background(), n.db)
	if err != nil {
		n.t.Fatal(err)
	}
	n.t.Cleanup(func() { st.Close() })
	s, err := New(context.Background(), st, n.id, nil)
	if err != nil {
		n.t.Fatal(err)
	}
	n.s, n.st = s, st
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

var subnet = netip.MustParsePrefix("10.42.0.0/24")

// join fait entrer n dans le cercle par inviter, sans réseau.
func join(t *testing.T, inviter, n *tnode) Code {
	t.Helper()
	ctx := context.Background()
	code, _, err := inviter.s.NewInvite(ctx, time.Hour, []string{"127.0.0.1:1"}, randID(), randID())
	must(t, err)
	parsed, err := ParseCode(code.Encode())
	must(t, err)
	req, err := n.s.PrepareJoin(parsed, n.name, nil)
	must(t, err)
	resp, err := inviter.s.AcceptJoin(ctx, parsed.TempIP, req)
	must(t, err)
	must(t, n.s.CompleteJoin(ctx, parsed, req, resp))
	return parsed
}

func gossip(t *testing.T, a, b *tnode) {
	t.Helper()
	_, err := b.s.Merge(context.Background(), a.s.Table())
	must(t, err)
	_, err = a.s.Merge(context.Background(), b.s.Table())
	must(t, err)
}

func names(ms []Member) map[string]Member {
	out := map[string]Member{}
	for _, m := range ms {
		out[m.Name] = m
	}
	return out
}

func TestCreateJoinGossip(t *testing.T) {
	ctx := context.Background()
	alice, bob, carol := newNode(t, "alice"), newNode(t, "bob"), newNode(t, "carol")
	must(t, alice.s.Create(ctx, subnet, "alice", []string{"alice.example:51820"}))
	if self, _ := alice.s.Self(); self.TunnelIP.String() != "10.42.0.1" {
		t.Fatalf("fondateur en %v, attendu 10.42.0.1", self.TunnelIP)
	}
	if err := alice.s.Create(ctx, subnet, "alice", nil); !errors.Is(err, ErrAlreadyInCircle) {
		t.Fatalf("seconde création : %v", err)
	}

	join(t, alice, bob)
	join(t, bob, carol) // carol invitée par bob
	if len(alice.s.Members()) != 2 {
		t.Fatalf("alice ne doit pas encore connaître carol : %d membres", len(alice.s.Members()))
	}
	gossip(t, bob, alice)
	m := names(alice.s.Members())
	if len(m) != 3 || m["carol"].InvitedBy != bob.id.NodeID {
		t.Fatalf("alice après gossip : %+v", m)
	}
	ipSeen := map[netip.Addr]bool{}
	for _, mm := range m {
		if ipSeen[mm.TunnelIP] || !subnet.Contains(mm.TunnelIP) {
			t.Fatalf("adresses en double ou hors sous-réseau : %+v", m)
		}
		ipSeen[mm.TunnelIP] = true
	}

	// Mise à jour par le propriétaire : propagée, version plus haute.
	changed, err := carol.s.UpdateSelf(ctx, func(b *Body) { b.Name = "carole" })
	must(t, err)
	if !changed {
		t.Fatal("changement non vu")
	}
	gossip(t, carol, bob)
	gossip(t, bob, alice)
	if _, ok := names(alice.s.Members())["carole"]; !ok {
		t.Fatal("renommage non propagé")
	}

	// Persistance : rechargement depuis la base, signatures revérifiées.
	alice.open()
	if len(alice.s.Members()) != 3 {
		t.Fatalf("après rechargement : %d membres", len(alice.s.Members()))
	}
}

func TestForgeryRejected(t *testing.T) {
	ctx := context.Background()
	alice, bob, mallory := newNode(t, "alice"), newNode(t, "bob"), newNode(t, "mallory")
	must(t, alice.s.Create(ctx, subnet, "alice", nil))
	join(t, alice, bob)
	join(t, alice, mallory)

	// mallory réécrit le corps de bob (autre endpoint, version plus haute) avec sa propre clé.
	tab := mallory.s.Table()
	for i, f := range tab.Fiches {
		var b Body
		_ = json.Unmarshal(f.Body, &b)
		if b.NodeID == bob.id.NodeID {
			b.Announced = []string{"piege.example:51820"}
			b.Version += 1000
			tab.Fiches[i].Body, tab.Fiches[i].BodySig, _ = NewBody(mallory.id.Sign, b)
		}
	}
	// Et une fausse fiche admise par un inconnu.
	_, evil, _ := ed25519.GenerateKey(rand.Reader)
	adm, admSig, _ := NewAdmission(evil, Admission{Circle: tab.circleOf(t), NodeID: "intrus", SignPub: evil.Public().(ed25519.PublicKey), InvitedBy: "personne"})
	body, bodySig, _ := NewBody(evil, Body{Circle: tab.circleOf(t), NodeID: "intrus", Name: "intrus", TunnelIP: netip.MustParseAddr("10.42.0.200"), Version: 1})
	tab.Fiches = append(tab.Fiches, Fiche{Admission: adm, AdmissionSig: admSig, Body: body, BodySig: bodySig})

	_, err := alice.s.Merge(ctx, tab)
	must(t, err)
	m := names(alice.s.Members())
	if len(m["bob"].Announced) != 0 {
		t.Errorf("fiche de bob falsifiée acceptée : %+v", m["bob"])
	}
	if _, ok := m["intrus"]; ok {
		t.Error("membre sans chaîne d'admission accepté")
	}
}

func (tab Table) circleOf(t *testing.T) string {
	var a Admission
	if len(tab.Fiches) == 0 || json.Unmarshal(tab.Fiches[0].Admission, &a) != nil {
		t.Fatal("table vide")
	}
	return a.Circle
}

func TestRevocationIsFinal(t *testing.T) {
	ctx := context.Background()
	alice, bob, carol := newNode(t, "alice"), newNode(t, "bob"), newNode(t, "carol")
	must(t, alice.s.Create(ctx, subnet, "alice", nil))
	join(t, alice, bob)
	join(t, alice, carol)
	gossip(t, alice, bob)
	gossip(t, alice, carol)

	must(t, bob.s.Revoke(ctx, carol.id.NodeID))
	if err := bob.s.Revoke(ctx, bob.id.NodeID); err == nil {
		t.Error("auto-révocation acceptée")
	}
	gossip(t, bob, alice)
	if m := names(alice.s.Members()); !m["carol"].Revoked || m["carol"].RevokedBy != bob.id.NodeID {
		t.Fatalf("révocation non propagée : %+v", m["carol"])
	}
	if _, ok := alice.s.ByIP(names(alice.s.Members())["carol"].TunnelIP); ok {
		t.Error("un révoqué est encore reconnu par son adresse")
	}

	// carol, qui n'a rien vu, publie une fiche plus récente : elle reste révoquée.
	_, err := carol.s.UpdateSelf(ctx, func(b *Body) { b.Name = "carol-bis" })
	must(t, err)
	_, err = alice.s.Merge(ctx, carol.s.Table())
	must(t, err)
	if m := names(alice.s.Members()); !m["carol-bis"].Revoked {
		t.Fatalf("une version plus haute a annulé la révocation : %+v", m)
	}

	// Rechargement : toujours révoquée.
	alice.open()
	if m := names(alice.s.Members()); !m["carol-bis"].Revoked {
		t.Fatal("révocation perdue au redémarrage")
	}
}

func TestInviteRules(t *testing.T) {
	ctx := context.Background()
	alice, bob, carol := newNode(t, "alice"), newNode(t, "bob"), newNode(t, "carol")
	must(t, alice.s.Create(ctx, subnet, "alice", nil))
	if _, _, err := alice.s.NewInvite(ctx, time.Hour, nil, "k", "p"); err == nil {
		t.Error("invitation sans endpoint acceptée")
	}

	code, _, err := alice.s.NewInvite(ctx, time.Hour, []string{"h:1"}, randID(), randID())
	must(t, err)
	if code.Assigned == code.TempIP || code.Assigned == netip.MustParseAddr("10.42.0.1") {
		t.Fatalf("adresses de l'invitation : %v et %v", code.Assigned, code.TempIP)
	}
	req, _ := bob.s.PrepareJoin(code, "bob", nil)
	resp1, err := alice.s.AcceptJoin(ctx, code.TempIP, req)
	must(t, err)
	// Réponse perdue : bob réessaie, même réponse.
	resp2, err := alice.s.AcceptJoin(ctx, code.TempIP, req)
	if err != nil || string(resp1.Admission) != string(resp2.Admission) {
		t.Fatalf("nouvel essai de bob : %v", err)
	}
	// carol essaie le même code.
	reqC, _ := carol.s.PrepareJoin(code, "carol", nil)
	if _, err := alice.s.AcceptJoin(ctx, code.TempIP, reqC); !errors.Is(err, ErrInviteUsed) {
		t.Fatalf("code réutilisé : %v", err)
	}
	// Mauvais jeton.
	code2, _, _ := alice.s.NewInvite(ctx, time.Hour, []string{"h:1"}, randID(), randID())
	reqC, _ = carol.s.PrepareJoin(code2, "carol", nil)
	reqC.Token = "faux"
	if _, err := alice.s.AcceptJoin(ctx, code2.TempIP, reqC); !errors.Is(err, ErrInviteUnknown) {
		t.Fatalf("mauvais jeton : %v", err)
	}
	// Expiré.
	code3, _, _ := alice.s.NewInvite(ctx, time.Hour, []string{"h:1"}, randID(), randID())
	reqC, _ = carol.s.PrepareJoin(code3, "carol", nil)
	alice.s.now = func() time.Time { return time.Now().Add(2 * time.Hour) }
	if _, err := alice.s.AcceptJoin(ctx, code3.TempIP, reqC); !errors.Is(err, ErrInviteExpired) {
		t.Fatalf("code expiré : %v", err)
	}
	carol.s.now = alice.s.now
	if _, err := carol.s.PrepareJoin(code3, "carol", nil); !errors.Is(err, ErrInviteExpired) {
		t.Fatalf("code expiré côté invité : %v", err)
	}
	alice.s.now = time.Now
	// Annulation.
	code4, inv4, _ := alice.s.NewInvite(ctx, time.Hour, []string{"h:1"}, randID(), randID())
	must(t, alice.s.CancelInvite(ctx, inv4.ID))
	carol.s.now = time.Now
	reqC, _ = carol.s.PrepareJoin(code4, "carol", nil)
	if _, err := alice.s.AcceptJoin(ctx, code4.TempIP, reqC); !errors.Is(err, ErrInviteUnknown) {
		t.Fatalf("code annulé : %v", err)
	}
	if _, err := ParseCode("cercle1:abc"); !errors.Is(err, ErrBadCode) {
		t.Fatalf("code tronqué : %v", err)
	}
	if _, err := ParseCode("  " + code.Encode()[:20] + "\n" + code.Encode()[20:] + " "); err != nil {
		t.Fatalf("code coupé par une messagerie : %v", err)
	}
}

func TestAddressConflict(t *testing.T) {
	ctx := context.Background()
	alice, bob, carol, dave := newNode(t, "alice"), newNode(t, "bob"), newNode(t, "carol"), newNode(t, "dave")
	must(t, alice.s.Create(ctx, subnet, "alice", nil))
	join(t, alice, bob)
	gossip(t, alice, bob)
	join(t, alice, carol)
	join(t, bob, dave)
	// Forçage du conflit : dave prend l'adresse de carol (entrée avant lui).
	carolIP := names(alice.s.Members())["carol"].TunnelIP
	_, err := dave.s.UpdateSelf(ctx, func(b *Body) { b.TunnelIP = carolIP })
	must(t, err)
	gossip(t, dave, bob)
	gossip(t, bob, alice)
	m := names(alice.s.Members())
	if m["carol"].Conflict || !m["dave"].Conflict {
		t.Fatalf("arbitrage : carol %v, dave %v ; le plus ancien doit garder l'adresse", m["carol"].Conflict, m["dave"].Conflict)
	}
	if owner, ok := alice.s.ByIP(carolIP); !ok || owner.Name != "carol" {
		t.Fatalf("propriétaire de %v : %+v", carolIP, owner)
	}
	gossip(t, alice, dave)
	newIP, err := dave.s.ResolveConflict(ctx)
	must(t, err)
	if !newIP.IsValid() || newIP == carolIP {
		t.Fatalf("dave n'a pas changé d'adresse : %v", newIP)
	}
	gossip(t, dave, alice)
	if m := names(alice.s.Members()); m["dave"].Conflict || m["dave"].TunnelIP != newIP {
		t.Fatalf("après résolution : %+v", m["dave"])
	}
	if ip, _ := alice.s.ResolveConflict(ctx); ip.IsValid() {
		t.Fatal("alice n'avait pas de conflit")
	}
}

func TestDuplicateNames(t *testing.T) {
	ctx := context.Background()
	alice, bob1, bob2 := newNode(t, "alice"), newNode(t, "bob"), newNode(t, "bob")
	must(t, alice.s.Create(ctx, subnet, "alice", nil))
	join(t, alice, bob1)
	join(t, alice, bob2)
	seen := map[string]bool{}
	for _, m := range alice.s.Members() {
		if seen[m.DisplayName] {
			t.Fatalf("nom affiché en double : %q", m.DisplayName)
		}
		seen[m.DisplayName] = true
	}
}

func TestOversizedOrOutOfSubnetFicheRejected(t *testing.T) {
	ctx := context.Background()
	alice, mallory := newNode(t, "alice"), newNode(t, "mallory")
	must(t, alice.s.Create(ctx, subnet, "alice", nil))
	join(t, alice, mallory)
	gossip(t, alice, mallory)

	forge := func(mut func(*Body)) Table {
		tab := mallory.s.Table()
		for i, f := range tab.Fiches {
			var b Body
			_ = json.Unmarshal(f.Body, &b)
			if b.NodeID == mallory.id.NodeID {
				mut(&b)
				b.Version += 10
				tab.Fiches[i].Body, tab.Fiches[i].BodySig, _ = NewBody(mallory.id.Sign, b)
			}
		}
		return tab
	}
	for name, mut := range map[string]func(*Body){
		"nom démesuré":       func(b *Body) { b.Name = strings.Repeat("x", 5000) },
		"hors sous-réseau":   func(b *Body) { b.TunnelIP = netip.MustParseAddr("192.168.1.1") },
		"endpoints en masse": func(b *Body) { b.Announced = make([]string, 100) },
	} {
		_, err := alice.s.Merge(ctx, forge(mut))
		must(t, err)
		m, _ := alice.s.Member(mallory.id.NodeID)
		if m.Name != "mallory" || m.TunnelIP.String() == "192.168.1.1" || len(m.Announced) > 8 {
			t.Errorf("%s : fiche acceptée (%q, %v, %d endpoints)", name, m.Name, m.TunnelIP, len(m.Announced))
		}
	}
	if got := CleanName("  bob\x00\n" + strings.Repeat("é", 100)); utf8.RuneCountInString(got) != 64 || strings.ContainsAny(got, "\x00\n") {
		t.Errorf("CleanName = %q", got)
	}
}
