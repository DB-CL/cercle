// Package buildinfo expose la version injectée à la compilation par -ldflags.
package buildinfo

// Valeurs remplacées par -ldflags "-X github.com/DB-CL/cercle/internal/buildinfo.Version=…".
var (
	Version = "dev"
	Commit  = "inconnu"
	Date    = "inconnue"
)

// String renvoie la version lisible, par exemple « 1.2.0 (abc1234, 2026-11-04) ».
func String() string {
	return Version + " (" + Commit + ", " + Date + ")"
}
