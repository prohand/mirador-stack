// SPDX-License-Identifier: GPL-3.0-or-later

package sauvegarde

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mirador-stack/internal/server/base"
)

var (
	cle   = bytes.Repeat([]byte{7}, 32)
	autre = bytes.Repeat([]byte{8}, 32)
	ctx   = context.Background()
	t0    = time.Date(2026, 11, 10, 3, 17, 0, 0, time.UTC)
)

func chiffrer(t *testing.T, clair []byte) []byte {
	t.Helper()
	var b bytes.Buffer
	w, err := NouvelEcrivain(&b, cle)
	if err != nil {
		t.Fatal(err)
	}
	// Écritures de tailles variées.
	for len(clair) > 0 {
		n := min(len(clair), 1000+len(clair)%7000)
		if _, err := w.Write(clair[:n]); err != nil {
			t.Fatal(err)
		}
		clair = clair[n:]
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func dechiffrer(c, k []byte) ([]byte, error) {
	r, err := NouveauLecteur(bytes.NewReader(c), k)
	if err != nil {
		return nil, err
	}
	return io.ReadAll(r)
}

func TestChiffrementAllerRetour(t *testing.T) {
	for _, n := range []int{0, 1, tailleSegment - 1, tailleSegment, tailleSegment + 1, 3*tailleSegment + 17} {
		clair := make([]byte, n)
		_, _ = rand.Read(clair)
		got, err := dechiffrer(chiffrer(t, clair), cle)
		if err != nil || !bytes.Equal(got, clair) {
			t.Fatalf("%d octets : %v", n, err)
		}
	}
}

func TestChiffrementAttaques(t *testing.T) {
	clair := make([]byte, 3*tailleSegment+100)
	_, _ = rand.Read(clair)
	c := chiffrer(t, clair)
	entete := len(magie) + tailleSel
	seg := tailleSegment + 16
	cas := map[string][]byte{
		"octet modifié":       func() []byte { x := bytes.Clone(c); x[entete+10] ^= 1; return x }(),
		"dernier segment ôté": c[:entete+3*seg],
		"fin coupée":          c[:len(c)-5],
		"segments inversés":   append(append(append(bytes.Clone(c[:entete]), c[entete+seg:entete+2*seg]...), c[entete:entete+seg]...), c[entete+2*seg:]...),
		"sel modifié":         func() []byte { x := bytes.Clone(c); x[len(magie)] ^= 1; return x }(),
	}
	for nom, x := range cas {
		if _, err := dechiffrer(x, cle); !errors.Is(err, ErrArchive) {
			t.Errorf("%s : %v", nom, err)
		}
	}
	if _, err := dechiffrer(c, autre); !errors.Is(err, ErrArchive) {
		t.Errorf("mauvaise clé : %v", err)
	}
	if _, err := dechiffrer([]byte("pas une sauvegarde"), cle); err == nil {
		t.Error("fichier quelconque accepté")
	}
	// Deux archives du même contenu n'ont pas le même chiffré (sel aléatoire).
	if bytes.Equal(chiffrer(t, clair), c) {
		t.Error("même chiffré deux fois")
	}
}

func TestCheminSur(t *testing.T) {
	bons := []string{"base/mirador.db", "logs/2026111003.seg", "metriques/brut/01ABC/chunks/000001", "MANIFESTE.json"}
	mauvais := []string{"../etc/passwd", "/etc/passwd", "base/../../x", "autre/x", "base/", "logs\\x", "base//x", "versions/./x"}
	for _, c := range bons {
		if !cheminSur(c) {
			t.Errorf("%q refusé", c)
		}
	}
	for _, c := range mauvais {
		if cheminSur(c) {
			t.Errorf("%q accepté", c)
		}
	}
}

// sources de test : une vraie base SQLite, de faux moteurs.
type fauxMetriques struct{}

func (fauxMetriques) Instantane(d string) error {
	if err := os.MkdirAll(filepath.Join(d, "brut", "01BLOC"), 0o750); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(d, "brut", "01BLOC", "index"), []byte("index de test"), 0o600)
}

type fauxLogs struct{ chemin string }

func (f fauxLogs) Segments() ([]Segment, error) {
	// Taille figée plus petite que le fichier : seule cette partie est copiée.
	return []Segment{{Nom: "2026111003.seg", Chemin: f.chemin, Taille: 5}}, nil
}

func preparer(t *testing.T) Sources {
	t.Helper()
	dir := t.TempDir()
	db, err := base.Ouvrir(filepath.Join(dir, "mirador.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := base.Migrer(ctx, db, filepath.Join(dir, "sauvegardes")); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO machines(nom, statut, cree_le) VALUES ('srv-test-mirador', 'approuvee', 1)`); err != nil {
		t.Fatal(err)
	}
	_ = os.MkdirAll(filepath.Join(dir, "versions"), 0o700)
	_ = os.WriteFile(filepath.Join(dir, "versions", "mirador-agent-1.0.0-linux-amd64"), []byte("binaire"), 0o600)
	seg := filepath.Join(dir, "logs", "2026111003.seg")
	_ = os.MkdirAll(filepath.Dir(seg), 0o750)
	_ = os.WriteFile(seg, []byte("12345 encore en cours d'écriture"), 0o600)
	conf := filepath.Join(dir, "server.conf")
	_ = os.WriteFile(conf, []byte("MIRADOR_DONNEES=/var/lib/mirador\n"), 0o600)
	return Sources{DB: db, Dossier: dir, FichierDemarrage: conf, Metriques: fauxMetriques{}, Logs: fauxLogs{seg}}
}

func TestSauvegarderRestaurer(t *testing.T) {
	src := preparer(t)
	chemin, man, err := Sauvegarder(ctx, src, Options{Cle: cle, Metriques: true, Logs: true, Version: "1.0.0", Maintenant: func() time.Time { return t0 }})
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(chemin) != "mirador-20261110-031700.sauvegarde" || man.Schema != base.VersionAttendue() || len(man.Fichiers) != 5 {
		t.Fatalf("%s %+v", chemin, man)
	}
	if info, _ := os.Stat(chemin); info.Mode().Perm() != 0o600 {
		t.Errorf("droits de l'archive : %v", info.Mode().Perm())
	}
	// Rien de lisible en clair dans l'archive.
	brut, _ := os.ReadFile(chemin)
	if bytes.Contains(brut, []byte("srv-test-mirador")) || bytes.Contains(brut, []byte("SQLite")) {
		t.Fatal("contenu en clair dans l'archive")
	}

	cible := t.TempDir()
	f, _ := os.Open(chemin)
	defer f.Close()
	if _, err := Restaurer(f, cle, cible, TailleMaxRestauration); err != nil {
		t.Fatal(err)
	}
	db, err := base.Ouvrir(filepath.Join(cible, "mirador.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var n int
	_ = db.QueryRow(`SELECT count(*) FROM machines WHERE nom = 'srv-test-mirador'`).Scan(&n)
	if n != 1 {
		t.Fatal("machine absente après restauration")
	}
	for fichier, attendu := range map[string]string{
		"versions/mirador-agent-1.0.0-linux-amd64": "binaire",
		"logs/2026111003.seg":                      "12345",
		"metriques/brut/01BLOC/index":              "index de test",
		"restauration/server.conf":                 "MIRADOR_DONNEES=/var/lib/mirador\n",
	} {
		if b, err := os.ReadFile(filepath.Join(cible, fichier)); err != nil || string(b) != attendu {
			t.Errorf("%s : %q %v", fichier, b, err)
		}
	}
	// Restauration refusée sur des données existantes, ou avec une mauvaise clé.
	_, _ = f.Seek(0, io.SeekStart)
	if _, err := Restaurer(f, cle, cible, TailleMaxRestauration); err == nil || !strings.Contains(err.Error(), "pas vide") {
		t.Errorf("dossier non vide : %v", err)
	}
	_, _ = f.Seek(0, io.SeekStart)
	vide := t.TempDir()
	if _, err := Restaurer(f, autre, vide, TailleMaxRestauration); !errors.Is(err, ErrArchive) {
		t.Errorf("mauvaise clé : %v", err)
	}
	if entrees, _ := os.ReadDir(vide); len(entrees) != 0 {
		t.Error("restauration ratée : des fichiers sont restés")
	}
	// Taille maximale.
	_, _ = f.Seek(0, io.SeekStart)
	if _, err := Restaurer(f, cle, t.TempDir(), 10); err == nil {
		t.Error("limite de taille ignorée")
	}
}

func TestDeuxSauvegardesMemeSeconde(t *testing.T) {
	src := preparer(t)
	var noms []string
	for i := 0; i < 3; i++ {
		c, _, err := Sauvegarder(ctx, src, Options{Cle: cle, Maintenant: func() time.Time { return t0 }})
		if err != nil {
			t.Fatal(err)
		}
		noms = append(noms, filepath.Base(c))
	}
	if noms[1] != "mirador-20261110-031700-2.sauvegarde" || noms[2] != "mirador-20261110-031700-3.sauvegarde" {
		t.Fatalf("%v", noms)
	}
	liste, _ := Lister(DossierArchives(src.Dossier))
	if len(liste) != 3 || liste[0].Nom != noms[2] {
		t.Fatalf("ordre : %+v", liste)
	}
}

func TestConfigurationSeule(t *testing.T) {
	src := preparer(t)
	_, man, err := Sauvegarder(ctx, src, Options{Cle: cle, Maintenant: func() time.Time { return t0 }})
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range man.Fichiers {
		if strings.HasPrefix(e.Chemin, "metriques/") || strings.HasPrefix(e.Chemin, "logs/") {
			t.Errorf("%s inclus sans option", e.Chemin)
		}
	}
}

func TestRetention(t *testing.T) {
	src := preparer(t)
	for i := 0; i < 5; i++ {
		d := t0.Add(time.Duration(i) * 24 * time.Hour)
		if _, _, err := Sauvegarder(ctx, src, Options{Cle: cle, Maintenant: func() time.Time { return d }}); err != nil {
			t.Fatal(err)
		}
	}
	dossier := DossierArchives(src.Dossier)
	_ = os.WriteFile(filepath.Join(dossier, "a-garder.txt"), []byte("x"), 0o600)
	sup, err := Retention(dossier, 2)
	if err != nil || len(sup) != 3 {
		t.Fatalf("%v %v", sup, err)
	}
	liste, _ := Lister(dossier)
	if len(liste) != 2 || liste[0].Date != t0.Add(4*24*time.Hour) {
		t.Fatalf("%+v", liste)
	}
	if _, err := os.Stat(filepath.Join(dossier, "a-garder.txt")); err != nil {
		t.Error("fichier étranger supprimé")
	}
	if _, err := Retention(dossier, 0); err == nil {
		t.Error("rétention 0 acceptée")
	}
}

// FuzzLecture : une archive piégée ne fait jamais paniquer ni écrire hors du dossier.
func FuzzLecture(f *testing.F) {
	var b bytes.Buffer
	w, _ := NouvelEcrivain(&b, cle)
	_, _ = w.Write([]byte("contenu"))
	_ = w.Close()
	f.Add(b.Bytes())
	f.Fuzz(func(t *testing.T, x []byte) {
		dir := t.TempDir()
		_, _ = lireArchive(bytes.NewReader(x), cle, dir, 1<<20)
	})
}
