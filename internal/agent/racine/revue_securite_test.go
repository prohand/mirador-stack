// SPDX-License-Identifier: GPL-3.0-or-later

package racine

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// arbre : dossiers d0..d(n-1), chacun avec cert.pem, cle.key et un sous-dossier.
func arbre(t *testing.T, n int) string {
	t.Helper()
	racine := t.TempDir()
	for i := range n {
		d := filepath.Join(racine, fmt.Sprintf("d%02d", i))
		if err := os.MkdirAll(filepath.Join(d, "sous"), 0o755); err != nil {
			t.Fatal(err)
		}
		for _, f := range []string{"cert.pem", "cle.key", "sous/chain.pem"} {
			if err := os.WriteFile(filepath.Join(d, f), nil, 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	return racine
}

func parcoursLarge() *parcours {
	return &parcours{reste: maxEntreesParcours, fin: time.Now().Add(time.Hour), horloge: time.Now}
}

// Sans limite atteinte, même résultat que filepath.Glob.
func TestParcoursCommeGlob(t *testing.T) {
	r := arbre(t, 5)
	for _, m := range []string{"/*/cert.pem", "/d0?/*.pem", "/*/sous/*.pem", "/d01/cert.pem", "/absent/*.pem",
		"/d0[13]/c*", "/*/*/*", "/d00/inexistant.pem"} {
		attendu, _ := filepath.Glob(r + m)
		obtenu, tronque := parcoursLarge().glob(r + m)
		if tronque || !slices.Equal(obtenu, attendu) {
			t.Errorf("%s :\n  obtenu %v (tronqué %v)\n  attendu %v", m, obtenu, tronque, attendu)
		}
	}
}

// Budget d'entrées : le parcours s'arrête, résultat signalé incomplet.
func TestParcoursBudgetEntrees(t *testing.T) {
	r := arbre(t, 40)
	p := &parcours{reste: 50, fin: time.Now().Add(time.Hour), horloge: time.Now}
	obtenu, tronque := p.glob(r + "/*/sous/*.pem")
	if !tronque {
		t.Fatal("budget dépassé sans le signaler")
	}
	if len(obtenu) >= 40 {
		t.Errorf("%d fichiers : le parcours ne s'est pas arrêté", len(obtenu))
	}
	// Budget épuisé : les motifs suivants ne lisent plus aucun dossier.
	if f, tronque := p.glob(r + "/d00/*.pem"); len(f) != 0 || !tronque {
		t.Errorf("après épuisement : %v, tronqué %v", f, tronque)
	}
}

// Échéance : passée, plus aucun dossier n'est ouvert.
func TestParcoursDuree(t *testing.T) {
	r := arbre(t, 10)
	maintenant := time.Now()
	p := &parcours{reste: maxEntreesParcours, fin: maintenant.Add(30 * time.Second),
		horloge: func() time.Time { maintenant = maintenant.Add(10 * time.Second); return maintenant }}
	obtenu, tronque := p.glob(r + "/*/sous/*.pem")
	if !tronque || len(obtenu) >= 10 {
		t.Errorf("échéance ignorée : %d fichiers, tronqué %v", len(obtenu), tronque)
	}
}

// lireCertificats compte un parcours arrêté en erreur (liste incomplète).
func TestCertificatsParcoursArrete(t *testing.T) {
	r := arbre(t, 3)
	c := lireCertificats([]string{r + "/*/cert.pem"}, nil)
	if c.Erreurs != 3 { // fichiers vides : illisibles, mais parcours complet
		t.Errorf("erreurs : %d", c.Erreurs)
	}
}

// Sortie bornée pendant la lecture : refus au-delà de max, rien gardé en plus.
func TestEcrivainBorne(t *testing.T) {
	arrets := 0
	e := &ecrivainBorne{max: 10, depasse: func() { arrets++ }}
	if n, err := e.Write([]byte("123456")); n != 6 || err != nil {
		t.Fatal(n, err)
	}
	if _, err := e.Write([]byte("78901")); !errors.Is(err, errSortieTropLongue) {
		t.Fatalf("dépassement accepté : %v", err)
	}
	if _, err := e.Write([]byte("x")); err == nil {
		t.Fatal("écriture acceptée après dépassement")
	}
	if e.Len() != 6 || !e.trop || arrets != 1 {
		t.Errorf("longueur %d, trop %v, arrêts %d", e.Len(), e.trop, arrets)
	}
	// io.Copy (os/exec) passerait par ReadFrom, sans limite : interdit.
	if _, ok := any(e).(io.ReaderFrom); ok {
		t.Error("ecrivainBorne ne doit pas avoir de méthode ReadFrom")
	}
}

// Un programme qui écrit sans fin (yes) est arrêté vite, mémoire bornée.
func TestLancerSortieSansFin(t *testing.T) {
	const yes = "/usr/bin/yes"
	if verifierProgramme(yes) != nil {
		t.Skip(yes + " absent ou n'appartenant pas à root")
	}
	debut := time.Now()
	_, err := programmesSysteme{delai: time.Minute}.lancer(yes)
	if err == nil || !strings.Contains(err.Error(), "sortie trop longue") {
		t.Fatalf("erreur attendue « sortie trop longue », obtenu %v", err)
	}
	if d := time.Since(debut); d > 20*time.Second {
		t.Errorf("arrêt trop lent : %s", d)
	}
}
