//go:build !linux

package transfer

// diskFree n'est mesuré que sous Linux (cible des images) ; ailleurs, pas de garde.
func diskFree(string) (int64, bool) { return 0, false }
