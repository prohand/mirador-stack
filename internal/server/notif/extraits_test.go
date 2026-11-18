// SPDX-License-Identifier: GPL-3.0-or-later

package notif

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"mirador-stack/internal/server/alertes"
)

// Quelle recherche pour quelle alerte (SPEC § 41).
func TestRequeteExtrait(t *testing.T) {
	maintenant := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	debut := maintenant.Add(-3 * time.Minute)
	cas := []struct {
		a      alertes.Alerte
		ok     bool
		champs string // host,unit,container
		mots   string
	}{
		{alerte(1, "ServiceSystemdEnEchec", alertes.Critique, "web1", debut, "name", "nginx.service"), true, "web1,nginx.service,", ""},
		{alerte(2, "ConteneurNonSain", alertes.Avertissement, "web1", debut, "name", "postgres"), true, "web1,,postgres", ""},
		{alerte(3, "OomKiller", alertes.Critique, "web1", debut), true, "web1,,", "killed memory"},
		{alerte(4, "CpuEleve", alertes.Critique, "web1", debut), false, "", ""},
		{alerte(5, "ServiceSystemdEnEchec", alertes.Critique, "", debut, "name", "x.service"), false, "", ""},
	}
	for _, c := range cas {
		r, ok := requeteExtrait(&c.a, maintenant)
		if ok != c.ok {
			t.Errorf("%s : ok = %v", c.a.Regle, ok)
			continue
		}
		if !ok {
			continue
		}
		if got := r.Champs["host"] + "," + r.Champs["unit"] + "," + r.Champs["container"]; got != c.champs || r.Mots != c.mots {
			t.Errorf("%s : champs %q mots %q", c.a.Regle, got, r.Mots)
		}
		if !r.Debut.Equal(debut.Add(-extraitAvant)) || !r.Fin.Equal(maintenant) {
			t.Errorf("%s : période %v → %v", c.a.Regle, r.Debut, r.Fin)
		}
	}
	// Alerte vieille de 3 jours : seulement les dernières 24 h.
	vieille := alerte(6, "ConteneurNonSain", alertes.Critique, "web1", maintenant.Add(-72*time.Hour), "name", "app")
	if r, _ := requeteExtrait(&vieille, maintenant); !r.Debut.Equal(maintenant.Add(-extraitPeriodeMax)) {
		t.Errorf("alerte ancienne : début %v", r.Debut)
	}
}

func TestNettoyerLigne(t *testing.T) {
	if got := nettoyerLigne("  a\tb\r\nc\x1b[31m  "); got != "a b  c [31m" {
		t.Errorf("contrôles : %q", got)
	}
	long := strings.Repeat("é", 300)
	if got := []rune(nettoyerLigne(long)); len(got) != extraitLongueur || got[len(got)-1] != '…' {
		t.Errorf("longueur : %d", len(got))
	}
}

// fauxLogs : enregistre les recherches, renvoie des lignes (plus récentes d'abord).
type fauxLogs struct {
	requetes []RequeteExtrait
	lignes   []string
	err      error
}

func (f *fauxLogs) chercher(_ context.Context, r RequeteExtrait, n int) ([]string, error) {
	f.requetes = append(f.requetes, r)
	if n != ExtraitLignes {
		return nil, errors.New("nombre de lignes inattendu")
	}
	return f.lignes, f.err
}

// Bout en bout dans l'aiguillage : extrait joint à la première annonce,
// pas aux rappels ni aux fins ; HTML échappé ; ordre chronologique.
func TestExtraitsDansLeMessage(t *testing.T) {
	b := nouveauBanc(t)
	f := &fauxLogs{lignes: []string{"12:00:03 nginx: [emerg] bind() to 0.0.0.0:80 failed <a>", "12:00:02 Starting nginx…"}}
	b.g.opt.Logs = f.chercher
	a := alerte(1, "ServiceSystemdEnEchec", alertes.Critique, "web1", b.horloge, "name", "nginx.service")
	b.debut(a)
	m := b.avancer(15 * time.Second)
	if len(m) != 1 || len(f.requetes) != 1 || f.requetes[0].Champs["unit"] != "nginx.service" {
		t.Fatalf("messages %d, recherches %+v", len(m), f.requetes)
	}
	texte := Texte(m[0], Mise{HTML: true})
	attendu := "📄 Derniers logs :\n<pre>12:00:02 Starting nginx…\n12:00:03 nginx: [emerg] bind() to 0.0.0.0:80 failed &lt;a&gt;</pre>"
	if !strings.Contains(texte, attendu) {
		t.Errorf("extrait absent ou mal formé :\n%s", texte)
	}
	if brut := Texte(m[0], Mise{}); !strings.Contains(brut, "  12:00:02 Starting nginx…\n  12:00:03") {
		t.Errorf("texte brut :\n%s", brut)
	}
	// Rappel (2 h) et fin : aucune nouvelle recherche.
	b.avancer(2*time.Hour + time.Minute)
	b.fin(a)
	b.avancer(6 * time.Minute)
	if len(f.requetes) != 1 {
		t.Errorf("recherches pour un rappel ou une fin : %d", len(f.requetes))
	}
}

// Réglage coupé, erreur de recherche, alerte sans correspondance, plafond
// de 3 alertes par message : le message part toujours.
func TestExtraitsLimites(t *testing.T) {
	b := nouveauBanc(t)
	f := &fauxLogs{err: errors.New("stockage indisponible")}
	b.g.opt.Logs = f.chercher
	b.debut(alerte(1, "ServiceSystemdEnEchec", alertes.Critique, "web1", b.horloge, "name", "a.service"))
	if m := b.avancer(15 * time.Second); len(m) != 1 || len(m[0].Extraits) != 0 {
		t.Fatalf("erreur de recherche : %+v", m)
	}
	// 5 conteneurs dans un même groupe (même règle, même machine) : 3 recherches.
	f.err, f.requetes, f.lignes = nil, nil, []string{"12:00:00 erreur"}
	for i, nom := range []string{"c1", "c2", "c3", "c4", "c5"} {
		b.debut(alerte(int64(10+i), "ConteneurNonSain", alertes.Avertissement, "web2", b.horloge, "name", nom))
	}
	if m := b.avancer(35 * time.Second); len(m) != 1 || len(f.requetes) != extraitAlertesMax || len(m[0].Extraits) != extraitAlertesMax {
		t.Fatalf("plafond : %d message(s), %d recherches", len(m), len(f.requetes))
	}
	// Réglage « sans extraits ».
	r, err := LireReglages(context.Background(), b.db)
	if err != nil {
		t.Fatal(err)
	}
	r.SansExtraitsLogs = true
	if err := EcrireReglages(context.Background(), b.db, r, cleTest, "test", b.horloge); err != nil {
		t.Fatal(err)
	}
	f.requetes = nil
	b.horloge = b.horloge.Add(2 * time.Minute) // relecture des réglages
	b.debut(alerte(20, "OomKiller", alertes.Critique, "web3", b.horloge))
	if m := b.avancer(15 * time.Second); len(m) != 1 || len(f.requetes) != 0 {
		t.Fatalf("réglage coupé : %d message(s), %d recherches", len(m), len(f.requetes))
	}
}

// Dernier changement de la machine dans le message (SPEC § 47) et données
// absentes (SPEC § 46).
func TestChangementEtAbsenceDansLeMessage(t *testing.T) {
	b := nouveauBanc(t)
	var demandes []string
	b.g.opt.Changement = func(_ context.Context, machine string, avant time.Time) (time.Time, string, bool) {
		demandes = append(demandes, machine)
		return avant.Add(-12 * time.Minute), "14 paquets mis à jour", true
	}
	a := alerte(1, "DisqueQuasiPlein", alertes.Critique, "nas", b.horloge)
	a.AbsenteDepuis = b.horloge.Add(-26 * time.Hour)
	b.debut(a)
	m := b.avancer(15 * time.Second)
	if len(m) != 1 || len(demandes) != 1 || demandes[0] != "nas" {
		t.Fatalf("messages %d, demandes %v", len(m), demandes)
	}
	texte := Texte(m[0], Mise{})
	for _, attendu := range []string{"🔧 12m avant : 14 paquets mis à jour", "📡 Données absentes depuis", "(1j 2h) : la machine n'envoie plus rien"} {
		if !strings.Contains(texte, attendu) {
			t.Errorf("%q absent :\n%s", attendu, texte)
		}
	}
}
