// SPDX-License-Identifier: GPL-3.0-or-later

package journaux

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"mirador-stack/internal/server/stockage"
)

var (
	t0  = time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	ctx = context.Background()
	lim = LimitesRechercheDefaut()
)

type horloge struct{ ns atomic.Int64 }

func nouvelleHorloge(t time.Time) *horloge {
	h := &horloge{}
	h.regler(t)
	return h
}
func (h *horloge) regler(t time.Time)    { h.ns.Store(t.UnixNano()) }
func (h *horloge) maintenant() time.Time { return time.Unix(0, h.ns.Load()).UTC() }

func ouvrir(t *testing.T, dossier string, h *horloge, taille int) *Moteur {
	t.Helper()
	m, err := Ouvrir(Options{Dossier: dossier, Maintenant: h.maintenant, TailleBloc: taille})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func ligne(d time.Duration, host, level, msg string) stockage.Ligne {
	return stockage.Ligne{T: t0.Add(d).UnixMilli(), Msg: msg,
		Champs: map[string]string{"host": host, "level": level, "job": "journal", "unit": "ssh.service"}}
}

// remplir : n lignes réparties sur 2 machines, une erreur toutes les 100.
func remplir(t *testing.T, m *Moteur, n int) {
	t.Helper()
	var lot []stockage.Ligne
	for i := 0; i < n; i++ {
		host, level, msg := "pc", "info", fmt.Sprintf("Accepted publickey for user%d from 192.0.2.%d port %d", i%7, i%250, 40000+i)
		if i%2 == 1 {
			host = "nas"
		}
		if i%100 == 0 {
			level, msg = "err", fmt.Sprintf("Failed password for root from 203.0.113.%d (essai %d)", i%250, i)
		}
		lot = append(lot, ligne(time.Duration(i)*time.Second, host, level, msg))
	}
	if err := m.Ecrire(ctx, lot); err != nil {
		t.Fatal(err)
	}
	if err := m.Vider(); err != nil {
		t.Fatal(err)
	}
}

func periode(d time.Duration) (int64, int64) { return t0.UnixMilli(), t0.Add(d).UnixMilli() }

func TestEcrireChercher(t *testing.T) {
	h := nouvelleHorloge(t0)
	m := ouvrir(t, t.TempDir(), h, 32<<10) // petits blocs : plusieurs blocs
	defer m.Fermer()
	remplir(t, m, 3000)
	if m.Blocs.Load() < 5 {
		t.Fatalf("%d blocs", m.Blocs.Load())
	}
	debut, fin := periode(time.Hour)

	// Tout, limité à 10 : les plus récentes d'abord, et arrêt sans tout lire.
	res, err := m.Chercher(ctx, Requete{Debut: debut, Fin: fin, Limite: 10}, lim)
	if err != nil || len(res.Lignes) != 10 || !res.Tronque {
		t.Fatalf("%d lignes, tronqué %v, %v", len(res.Lignes), res.Tronque, err)
	}
	if res.Lignes[0].T != t0.Add(2999*time.Second).UnixMilli() || res.Lignes[0].T < res.Lignes[9].T {
		t.Errorf("ordre : %d … %d", res.Lignes[0].T, res.Lignes[9].T)
	}
	if res.BlocsLus >= int(m.Blocs.Load()) {
		t.Errorf("tous les blocs lus (%d) pour 10 lignes", res.BlocsLus)
	}

	// Champs + mots : erreurs du NAS contenant « failed password ».
	res, err = m.Chercher(ctx, Requete{Debut: debut, Fin: fin, Champs: map[string]string{"level": "err"},
		Mots: "FAILED password", Limite: 1000}, lim)
	if err != nil || len(res.Lignes) != 30 || res.Tronque {
		t.Fatalf("erreurs : %d lignes, %v", len(res.Lignes), err)
	}
	for _, l := range res.Lignes {
		if l.Champs["level"] != "err" || l.Champs["host"] != "pc" { // i%100 == 0 → pair → pc
			t.Fatalf("ligne inattendue : %+v", l)
		}
	}
	// Mot entier : « pass » n'est pas « password ».
	res, _ = m.Chercher(ctx, Requete{Debut: debut, Fin: fin, Mots: "pass"}, lim)
	if len(res.Lignes) != 0 {
		t.Errorf("« pass » : %d lignes", len(res.Lignes))
	}
	// Morceau de texte : « pass » trouvé dans « password ».
	res, _ = m.Chercher(ctx, Requete{Debut: debut, Fin: fin, Contient: "PASS", Limite: 1000}, lim)
	if len(res.Lignes) != 30 {
		t.Errorf("contient « pass » : %d lignes", len(res.Lignes))
	}
	// Mot absent partout : le filtre de Bloom évite presque toute décompression.
	res, _ = m.Chercher(ctx, Requete{Debut: debut, Fin: fin, Mots: "introuvable"}, lim)
	if len(res.Lignes) != 0 || res.BlocsLus > 1 {
		t.Errorf("mot absent : %d lignes, %d blocs lus sur %d", len(res.Lignes), res.BlocsLus, res.BlocsLus+res.BlocsSautes)
	}
	// Machine absente de l'index : aucun bloc lu.
	res, _ = m.Chercher(ctx, Requete{Debut: debut, Fin: fin, Champs: map[string]string{"host": "autre"}}, lim)
	if res.BlocsLus != 0 {
		t.Errorf("machine inconnue : %d blocs lus", res.BlocsLus)
	}
	// Variantes (machine renommée : ancien nom) : « nouveau » + « pc ».
	res, _ = m.Chercher(ctx, Requete{Debut: debut, Fin: fin, Champs: map[string]string{"host": "nouveau"},
		Variantes: map[string][]string{"host": {"pc"}}, Limite: 5000}, lim)
	if len(res.Lignes) != 1500 {
		t.Errorf("variantes : %d lignes", len(res.Lignes))
	}
	// Période : 10 premières secondes seulement.
	res, _ = m.Chercher(ctx, Requete{Debut: debut, Fin: t0.Add(9 * time.Second).UnixMilli()}, lim)
	if len(res.Lignes) != 10 {
		t.Errorf("période : %d lignes", len(res.Lignes))
	}
}

func TestLignesEnAttenteTrouvees(t *testing.T) {
	m := ouvrir(t, t.TempDir(), nouvelleHorloge(t0), 0)
	defer m.Fermer()
	_ = m.Ecrire(ctx, []stockage.Ligne{ligne(0, "pc", "info", "pas encore écrite")})
	res, _ := m.Chercher(ctx, Requete{Debut: t0.UnixMilli(), Fin: t0.UnixMilli(), Mots: "écrite"}, lim)
	if len(res.Lignes) != 1 {
		t.Fatalf("%d lignes", len(res.Lignes))
	}
}

func TestRequetesRefusees(t *testing.T) {
	m := ouvrir(t, t.TempDir(), nouvelleHorloge(t0), 0)
	defer m.Fermer()
	if _, err := m.Chercher(ctx, Requete{Debut: 10, Fin: 5}, lim); err != ErrRequete {
		t.Error("bornes inversées acceptées")
	}
	if _, err := m.Chercher(ctx, Requete{Debut: 0, Fin: int64(32 * 24 * time.Hour / time.Millisecond)}, lim); err != ErrRequete {
		t.Error("période trop longue acceptée")
	}
}

func TestLimiteDeBlocs(t *testing.T) {
	m := ouvrir(t, t.TempDir(), nouvelleHorloge(t0), 8<<10)
	defer m.Fermer()
	remplir(t, m, 2000)
	l := lim
	l.MaxBlocs = 2
	debut, fin := periode(time.Hour)
	res, err := m.Chercher(ctx, Requete{Debut: debut, Fin: fin, Contient: "introuvable"}, l)
	if err != nil || !res.Tronque || res.BlocsLus > 2+2*l.Travailleurs {
		t.Fatalf("tronqué %v, %d blocs lus, %v", res.Tronque, res.BlocsLus, err)
	}
}

func TestRedemarrageEtBlocAbime(t *testing.T) {
	dir := t.TempDir()
	h := nouvelleHorloge(t0)
	m := ouvrir(t, dir, h, 16<<10)
	remplir(t, m, 1000)
	if err := m.Fermer(); err != nil {
		t.Fatal(err)
	}
	// Arrêt brutal simulé : un morceau de bloc à moitié écrit en fin de fichier.
	seg := filepath.Join(dir, "2026110912.seg")
	f, err := os.OpenFile(seg, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.Write([]byte("MLB1\x10\x00\x00\x00abc"))
	_ = f.Close()

	m = ouvrir(t, dir, h, 16<<10)
	defer m.Fermer()
	if m.BlocsAbimes.Load() != 1 {
		t.Errorf("bloc abîmé non détecté")
	}
	debut, fin := periode(time.Hour)
	res, err := m.Chercher(ctx, Requete{Debut: debut, Fin: fin, Limite: 5000}, lim)
	if err != nil || len(res.Lignes) != 1000 {
		t.Fatalf("après redémarrage : %d lignes, %v", len(res.Lignes), err)
	}
	// On peut continuer d'écrire dans le même fichier après la coupure.
	_ = m.Ecrire(ctx, []stockage.Ligne{ligne(2000*time.Second, "pc", "info", "après reprise")})
	_ = m.Vider()
	res, _ = m.Chercher(ctx, Requete{Debut: debut, Fin: fin, Mots: "reprise"}, lim)
	if len(res.Lignes) != 1 {
		t.Fatalf("écriture après reprise : %d", len(res.Lignes))
	}
}

func TestRetentionEtPlafond(t *testing.T) {
	dir := t.TempDir()
	h := nouvelleHorloge(t0)
	m := ouvrir(t, dir, h, 0)
	defer m.Fermer()
	// 5 heures de réception, une ligne par heure.
	for i := 0; i < 5; i++ {
		h.regler(t0.Add(time.Duration(i) * time.Hour))
		_ = m.Ecrire(ctx, []stockage.Ligne{ligne(time.Duration(i)*time.Hour, "pc", "info", "heure")})
		_ = m.Vider()
	}
	if n := len(m.fichiers); n != 5 {
		t.Fatalf("%d segments", n)
	}
	// Rétention ramenée à 2 h (à chaud) : les 2 plus anciens partent.
	if err := m.Regler(Reglages{Retention: 2 * time.Hour}); err != nil {
		t.Fatal(err)
	}
	if n := len(m.fichiers); n != 3 {
		t.Fatalf("après rétention : %d segments", n)
	}
	// Plafond de taille : on ne garde que ce qui tient (jamais le segment en cours).
	if err := m.Regler(Reglages{Retention: 30 * 24 * time.Hour, MaxOctets: 1}); err != nil {
		t.Fatal(err)
	}
	if n := len(m.fichiers); n != 1 {
		t.Fatalf("après plafond : %d segments", n)
	}
	entrees, _ := os.ReadDir(dir)
	if len(entrees) != 1 {
		t.Fatalf("%d fichiers sur disque", len(entrees))
	}
}

func TestVidageAutomatique(t *testing.T) {
	h := nouvelleHorloge(t0)
	m, err := Ouvrir(Options{Dossier: t.TempDir(), Maintenant: h.maintenant, Vidage: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Fermer()
	_ = m.Ecrire(ctx, []stockage.Ligne{ligne(0, "pc", "info", "x")})
	h.regler(t0.Add(2 * time.Second))
	limite := time.Now().Add(5 * time.Second)
	for m.Blocs.Load() == 0 && time.Now().Before(limite) {
		time.Sleep(50 * time.Millisecond)
	}
	if m.Blocs.Load() != 1 {
		t.Fatal("le bloc en cours n'a pas été écrit après le délai")
	}
}

func TestContientMot(t *testing.T) {
	cas := map[[2]string]bool{
		{"failed password for root", "password"}: true,
		{"failed password for root", "pass"}:     false,
		{"passwords", "password"}:                false,
		{"x-password:", "password"}:              true,
		{"échec de connexion", "échec"}:          true,
		{"réchec", "échec"}:                      false,
	}
	for c, attendu := range cas {
		if contientMot(c[0], c[1]) != attendu {
			t.Errorf("%q dans %q", c[1], c[0])
		}
	}
}

// FuzzLireEntete : un fichier abîmé ou modifié ne fait jamais paniquer.
func FuzzLireEntete(f *testing.F) {
	e := &entete{tmin: 1, tmax: 2, lignes: 1, index: map[string][]string{"host": {"pc"}}, bloomK: 5, bloom: []byte{1, 2}}
	f.Add(encoderEntete(e))
	f.Add([]byte("MLB1\xff\xff\xff\xff"))
	f.Fuzz(func(t *testing.T, b []byte) {
		e, err := lireEntete(bytesLecteur(b))
		if err == nil && e.tmax < e.tmin {
			t.Fatal("en-tête incohérent accepté")
		}
		if err == nil {
			_ = decoderLignes(b, e, func(*stockage.Ligne) bool { return true })
		}
	})
}

type lecteurOctets struct{ b []byte }

func bytesLecteur(b []byte) *lecteurOctets { return &lecteurOctets{b} }
func (l *lecteurOctets) Read(p []byte) (int, error) {
	if len(l.b) == 0 {
		return 0, os.ErrClosed
	}
	n := copy(p, l.b)
	l.b = l.b[n:]
	return n, nil
}

func BenchmarkEcrire(b *testing.B) {
	m, err := Ouvrir(Options{Dossier: b.TempDir()})
	if err != nil {
		b.Fatal(err)
	}
	defer m.Fermer()
	var lot []stockage.Ligne
	for i := 0; i < 1000; i++ {
		lot = append(lot, stockage.Ligne{T: t0.Add(time.Duration(i) * time.Millisecond).UnixMilli(),
			Msg:    fmt.Sprintf("GET /api/v1/query?x=%d 200 %dms user-agent=Mozilla/5.0 (X11; Linux x86_64)", i, i%50),
			Champs: map[string]string{"host": fmt.Sprintf("pc%d", i%25), "job": "docker", "container": "nginx", "stream": "stdout"}})
	}
	b.ReportAllocs()
	for b.Loop() {
		if err := m.Ecrire(ctx, lot); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N)/1000, "ns/ligne")
}
