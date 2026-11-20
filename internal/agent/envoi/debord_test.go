// SPDX-License-Identifier: GPL-3.0-or-later

package envoi

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func lotDe(c byte, n int) []byte { return bytes.Repeat([]byte{c}, n) }

// Tampon disque (SPEC § 100) : la mémoire déborde sur disque ; le disque
// part en premier (ordre gardé) ; quota respecté ; lots repris après un
// redémarrage ; Sauver à l'arrêt.
func TestDebord(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "metriques")
	d, err := OuvrirDebord(dir, 250, 100)
	if err != nil {
		t.Fatal(err)
	}
	f := NewFile(200) // 2 lots de 100 en mémoire
	f.AvecDisque(d)
	for _, c := range []byte("abcde") { // a, b, c débordent sur disque
		f.Ajouter(lotDe(c, 100))
	}
	_, _, jetes := f.Etat()
	if f.OctetsDisque() != 200 || jetes != 1 { // quota 250 : a jeté quand c arrive
		t.Fatalf("disque %d octets, %d jetés", f.OctetsDisque(), jetes)
	}
	// Ordre d'envoi : b, c (disque), puis d, e (mémoire).
	var ordre []byte
	for l := f.Premier(); l != nil; l = f.Premier() {
		ordre = append(ordre, l[0])
		f.Retirer(l)
	}
	if string(ordre) != "bcde" || f.OctetsDisque() != 0 {
		t.Fatalf("ordre %q, disque %d", ordre, f.OctetsDisque())
	}
	// Arrêt sans réseau : la mémoire va sur disque, reprise au démarrage suivant.
	f.Ajouter(lotDe('x', 50))
	f.Ajouter(lotDe('y', 50))
	f.Sauver()
	_ = os.WriteFile(filepath.Join(dir, "intrus.txt"), []byte("?"), 0o600)
	d2, err := OuvrirDebord(dir, 250, 100)
	if err != nil {
		t.Fatal(err)
	}
	f2 := NewFile(200)
	f2.AvecDisque(d2)
	select {
	case <-f2.Signal():
	default:
		t.Error("lots repris sans réveiller l'expéditeur")
	}
	if l := f2.Premier(); l == nil || l[0] != 'x' {
		t.Fatalf("reprise : %q", l)
	}
	if st, _ := os.Stat(dir); st.Mode().Perm() != 0o700 {
		t.Errorf("droits du dossier : %v", st.Mode().Perm())
	}
}

// Sans tampon disque : comportement d'avant (les plus anciens sont jetés).
func TestFileSansDisque(t *testing.T) {
	f := NewFile(200)
	for _, c := range []byte("abc") {
		f.Ajouter(lotDe(c, 100))
	}
	if l := f.Premier(); l[0] != 'b' {
		t.Errorf("premier : %c", l[0])
	}
	f.Sauver() // sans effet
}
