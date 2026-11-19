// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"net/url"
	"strings"
	"testing"
	"time"

	"mirador-stack/internal/server/motifs"
	"mirador-stack/internal/server/stockage"
)

func TestLireRecherche(t *testing.T) {
	for entree, attendu := range map[string][2]string{
		`error timeout`:                  {"error timeout", ""},
		`"out of memory"`:                {"", "out of memory"},
		`nginx "address already" failed`: {"nginx failed", "address already"},
		`"a" "b c"`:                      {"b c", "a"},
		`guillemet "seul`:                {"guillemet seul", ""},
	} {
		if m, c := lireRecherche(entree); m != attendu[0] || c != attendu[1] {
			t.Errorf("%q → %q, %q ; attendu %q", entree, m, c, attendu)
		}
	}
}

func TestSurligner(t *testing.T) {
	texte := func(segs []segment) string {
		var b strings.Builder
		for _, s := range segs {
			if s.Marque {
				b.WriteString("[" + s.Texte + "]")
			} else {
				b.WriteString(s.Texte)
			}
		}
		return b.String()
	}
	for _, c := range []struct {
		msg, mots, contient, attendu string
	}{
		{"bind() failed (98)", "failed", "", "bind() [failed] (98)"},
		{"FAILED password, refailed", "failed", "", "[FAILED] password, refailed"}, // mot entier seulement
		{"connexion refusée", "refusée", "", "connexion [refusée]"},
		{"Out of memory: kill", "", "of mem", "Out [of mem]ory: kill"},
		{"rien à voir", "absent", "", "rien à voir"},
	} {
		if g := texte(surligner(c.msg, motsDe(c.mots), c.contient)); g != c.attendu {
			t.Errorf("%q : %q, attendu %q", c.msg, g, c.attendu)
		}
	}
}

func TestRegrouper(t *testing.T) {
	l := func(msg, level, host string, s int) stockage.Ligne {
		return stockage.Ligne{T: int64(s) * 1000, Msg: msg, Champs: map[string]string{"level": level, "host": host}}
	}
	ls := []stockage.Ligne{
		l("Accepted publickey for admin from 192.0.2.3 port 5061 ssh2", "info", "pve1", 10),
		l("Accepted publickey for admin from 192.0.2.6 port 5060 ssh2", "info", "srv-test", 9),
		l("Accepted publickey for admin from 198.51.100.7 port 40000 ssh2", "info", "vps-web", 8),
		l("nginx: [emerg] bind() to 0.0.0.0:443 failed (98: Address already in use)", "err", "vps-web", 7),
		l("request failed, retrying in 2 s", "warning", "vm-docker", 6),
	}
	g := regrouper(ls, "", 0, 11000, nil, time.Now())
	if len(g) != 3 || g[0].N != 3 || g[0].TexteMachines() != "3 machines" || g[0].Largeur != 100 || g[0].Derniere != 10000 {
		t.Fatalf("plus fréquents : %+v", g)
	}
	if g = regrouper(ls, "erreurs", 0, 11000, nil, time.Now()); g[0].Classe != "err" || g[1].Classe != "warn" {
		t.Errorf("erreurs d'abord : %v %v", g[0].Niveau, g[1].Niveau)
	}
	if g = regrouper(ls, "rares", 0, 11000, nil, time.Now()); g[0].N != 1 {
		t.Errorf("rares d'abord : %d", g[0].N)
	}
	if n := len(lignesDuMotif(ls, regrouper(ls, "", 0, 11000, nil, time.Now())[0].Cle)); n != 3 {
		t.Errorf("lignes du message type : %d", n)
	}
	if g[0].Evolution == "" {
		t.Error("petite courbe absente")
	}
}

// Motifs (SPEC § 52) : mots qui changent fusionnés, « nouveau » d'après le
// registre, tri « nouveaux d'abord ».
func TestRegrouperMotifs(t *testing.T) {
	ctx := t.Context()
	b := nouveauBanc(t)
	t0 := time.Now().Add(-20 * 24 * time.Hour)
	reg, err := motifs.OuvrirRegistre(ctx, b.c.db, nil, t0)
	if err != nil {
		t.Fatal(err)
	}
	maintenant := time.Now()
	reg.Ajouter("Invalid user admin from 203.0.113.5 port 51234", t0.Add(time.Hour).Unix())
	reg.Ajouter("kernel: ata3.00: exception Emask 0x0 SAct 0x1 frozen", maintenant.Add(-time.Hour).Unix())
	l := func(msg string, s int) stockage.Ligne {
		return stockage.Ligne{T: maintenant.Add(time.Duration(-s) * time.Minute).UnixMilli(), Msg: msg, Champs: map[string]string{"host": "vps"}}
	}
	ls := []stockage.Ligne{
		l("Invalid user oracle from 198.51.100.7 port 4022", 1),
		l("Invalid user git from 192.0.2.1 port 60000", 2),
		l("kernel: ata3.00: exception Emask 0x0 SAct 0x1 frozen", 3),
	}
	g := regrouper(ls, "nouveaux", maintenant.Add(-time.Hour).UnixMilli(), maintenant.UnixMilli(), reg, maintenant)
	if len(g) != 2 || !g[0].Nouveau || g[1].Nouveau || g[1].N != 2 || g[1].PremierVu == "" {
		t.Fatalf("groupes : %+v", g)
	}
	var texte strings.Builder
	for _, s := range g[1].Motif {
		if s.Var {
			texte.WriteString("‹" + s.Texte + "›")
		} else {
			texte.WriteString(s.Texte)
		}
	}
	if texte.String() != "Invalid user ‹MOT› from ‹IP› port ‹N›" {
		t.Errorf("motif : %q", texte.String())
	}
}

func TestHistogrammeLogs(t *testing.T) {
	ls := []stockage.Ligne{
		{T: 30_000, Champs: map[string]string{"level": "err"}},
		{T: 31_000, Champs: map[string]string{"level": "info"}},
		{T: 59_000, Champs: map[string]string{"level": "warning"}},
		{T: 999_999_999, Champs: map[string]string{}}, // hors période : ignorée
	}
	h := calculerHistogramme(ls, 0, 60_000, time.UTC)
	if len(h.Barres) != histoBarres || h.Barres[30].Erreurs != 1 || h.Barres[31].Autres != 1 || h.Barres[59].Avertissements != 1 ||
		h.Barres[30].HErreur <= 0 || len(h.Graduations) != 5 {
		t.Errorf("histogramme : %+v", h.Barres[30])
	}
}

func TestPastillesEtLiens(t *testing.T) {
	v := url.Values{"host": {"pve1"}, "level": {"err+"}, "sans": {"unit:cron.service"}, "q": {`nginx "already in"`}, "periode": {"24h"}}
	f, err := lireFiltresLogs(v, time.Now())
	if err != nil || f.Champs["level"] != "err" || len(f.Variantes["level"]) == 0 || f.Exclus["unit"][0] != "cron.service" || f.Mots != "nginx" || f.Contient != "already in" {
		t.Fatalf("filtres : %+v %v", f, err)
	}
	ps := pastilles(v, f)
	trouve := map[string]pastilleFiltre{}
	for _, p := range ps {
		trouve[p.Titre] = p
	}
	if trouve["niveau"].Valeur != "err et plus grave" || !trouve["sans unité"].Exclu || trouve["mots"].Valeur != "nginx" || trouve["texte"].Valeur != "already in" {
		t.Fatalf("pastilles : %+v", ps)
	}
	if l := trouve["sans unité"].Lien; strings.Contains(l, "sans=") || !strings.Contains(l, "host=pve1") {
		t.Errorf("retirer l'exclusion : %s", l)
	}
	if l := trouve["mots"].Lien; !strings.Contains(l, "q=%22already+in%22") {
		t.Errorf("retirer les mots garde le texte exact : %s", l)
	}
	for _, mauvais := range []url.Values{{"sans": {"inconnu:x"}}, {"sans": {"host"}}, {"host": {`a"}`}}, {"q": {strings.Repeat("x", 201)}}} {
		if _, err := lireFiltresLogs(mauvais, time.Now()); err == nil {
			t.Errorf("accepté : %v", mauvais)
		}
	}
	// Période par zoom (de, a) prioritaire.
	if f, _ = lireFiltresLogs(url.Values{"de": {"1000"}, "a": {"2000"}, "periode": {"24h"}}, time.Now()); !f.Perso || f.Debut != 1000 || f.Fin != 2000 {
		t.Errorf("zoom : %+v", f)
	}
}

func TestNettoyerRequeteEtCSV(t *testing.T) {
	if s, err := nettoyerRequete("?host=pve1&inconnu=1&q=error&autour=5"); err != nil || s != "host=pve1&q=error" {
		t.Errorf("nettoyage : %q %v", s, err)
	}
	if _, err := nettoyerRequete("host=%22%3E"); err == nil {
		t.Error("valeur piégée acceptée")
	}
	for entree, attendu := range map[string]string{"=1+2": "'=1+2", "@SOMME": "'@SOMME", "-x": "'-x", "normal": "normal", "": ""} {
		if g := celluleCSV(entree); g != attendu {
			t.Errorf("%q → %q", entree, g)
		}
	}
}
