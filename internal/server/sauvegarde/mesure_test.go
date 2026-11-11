// SPDX-License-Identifier: GPL-3.0-or-later

package sauvegarde

import (
	"crypto/rand"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// TestMesureDebit : débit de sauvegarde et de restauration sur des données
// déjà compressées (comme la TSDB et les logs). MESURE=1 pour lancer.
func TestMesureDebit(t *testing.T) {
	if os.Getenv("MESURE") == "" {
		t.Skip("MESURE=1 pour lancer la mesure")
	}
	src := preparer(t)
	const taille = 200 << 20
	donnees := make([]byte, taille)
	_, _ = rand.Read(donnees)
	_ = os.WriteFile(filepath.Join(src.Dossier, "versions", "gros"), donnees, 0o600)
	donnees = nil
	cpu := func() float64 {
		var r syscall.Rusage
		_ = syscall.Getrusage(syscall.RUSAGE_SELF, &r)
		return float64(r.Utime.Nano()+r.Stime.Nano()) / 1e9
	}
	c0, d := cpu(), time.Now()
	chemin, _, err := Sauvegarder(ctx, src, Options{Cle: cle})
	if err != nil {
		t.Fatal(err)
	}
	duree, c1 := time.Since(d), cpu()
	info, _ := os.Stat(chemin)
	t.Logf("RÉSULTAT : sauvegarde de 200 Mo : %v, CPU %.1f s → %.0f Mo/s, archive %.0f Mo",
		duree.Round(time.Millisecond), c1-c0, 200/duree.Seconds(), float64(info.Size())/(1<<20))
	f, _ := os.Open(chemin)
	defer f.Close()
	c0, d = cpu(), time.Now()
	if _, err := Restaurer(f, cle, t.TempDir(), TailleMaxRestauration); err != nil {
		t.Fatal(err)
	}
	duree = time.Since(d)
	t.Logf("RÉSULTAT : restauration : %v, CPU %.1f s → %.0f Mo/s", duree.Round(time.Millisecond), cpu()-c0, 200/duree.Seconds())
}
