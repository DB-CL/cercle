package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"os"
)

// Static décrit un cercle figé à la main, sans adhésion ni gossip.
// Temporaire : sert au jalon J1 et disparaît au jalon J4.
type Static struct {
	CircleID string       `json:"circle_id"`
	TunnelIP netip.Addr   `json:"tunnel_ip"`
	Peers    []StaticPeer `json:"peers"`
}

// StaticPeer est un pair déclaré à la main.
type StaticPeer struct {
	Name      string     `json:"name"`
	PublicKey string     `json:"public_key"` // base64
	TunnelIP  netip.Addr `json:"tunnel_ip"`
	Endpoint  string     `json:"endpoint,omitempty"` // hôte:port, facultatif
}

// LoadStatic lit et vérifie un fichier de configuration statique.
func LoadStatic(path string) (Static, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Static{}, fmt.Errorf("lecture de %s : %w", path, err)
	}
	var s Static
	if err := json.Unmarshal(b, &s); err != nil {
		return Static{}, fmt.Errorf("JSON invalide dans %s : %w", path, err)
	}
	if s.CircleID == "" {
		return Static{}, errors.New("circle_id manquant")
	}
	if !s.TunnelIP.Is4() {
		return Static{}, errors.New("tunnel_ip doit être une adresse IPv4")
	}
	seen := map[netip.Addr]bool{s.TunnelIP: true}
	for _, p := range s.Peers {
		if !p.TunnelIP.Is4() || seen[p.TunnelIP] {
			return Static{}, fmt.Errorf("pair %q : tunnel_ip absente ou en double", p.Name)
		}
		seen[p.TunnelIP] = true
		if p.PublicKey == "" {
			return Static{}, fmt.Errorf("pair %q : public_key manquante", p.Name)
		}
	}
	return s, nil
}
