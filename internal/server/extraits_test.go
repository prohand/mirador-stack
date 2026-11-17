// SPDX-License-Identifier: GPL-3.0-or-later

package server

import (
	"context"
	"strings"
	"testing"
	"time"

	"mirador-stack/internal/server/notif"
	"mirador-stack/internal/server/stockage"
	"mirador-stack/internal/server/stockage/journaux"
)

// Recherche réelle dans le stockage des logs (SPEC § 41) : bonne machine,
// bonne unité, plus récentes d'abord, 5 lignes au plus ; ligne du noyau
// pour OomKiller ; conteneur.
func TestChercheurExtraits(t *testing.T) {
	ctx := context.Background()
	t0 := time.Now().Add(-time.Hour).Truncate(time.Second)
	jx, err := journaux.Ouvrir(journaux.Options{Dossier: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer jx.Fermer()
	var lignes []stockage.Ligne
	ajout := func(k int, msg string, champs ...string) {
		ch := map[string]string{}
		for i := 0; i+1 < len(champs); i += 2 {
			ch[champs[i]] = champs[i+1]
		}
		lignes = append(lignes, stockage.Ligne{T: t0.Add(time.Duration(k) * time.Second).UnixMilli(), Msg: msg, Champs: ch})
	}
	for k := 0; k < 8; k++ {
		ajout(k, "nginx ligne "+string(rune('0'+k)), "host", "web1", "job", "journal", "unit", "nginx.service")
	}
	ajout(9, "autre machine", "host", "web2", "job", "journal", "unit", "nginx.service")
	ajout(10, "sshd : connexion", "host", "web1", "job", "journal", "unit", "ssh.service")
	ajout(11, "Out of memory: Killed process 4242 (java)", "host", "web1", "job", "journal", "ident", "kernel")
	ajout(12, "panic: base injoignable", "host", "web1", "job", "docker", "container", "appli")
	if err := jx.Ecrire(ctx, lignes); err != nil {
		t.Fatal(err)
	}
	if err := jx.Vider(); err != nil {
		t.Fatal(err)
	}
	chercher := chercheurExtraits(jx)
	r := notif.RequeteExtrait{Champs: map[string]string{"host": "web1", "unit": "nginx.service"}, Debut: t0.Add(-time.Minute), Fin: t0.Add(time.Minute)}
	got, err := chercher(ctx, r, notif.ExtraitLignes)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != notif.ExtraitLignes || !strings.HasSuffix(got[0], "nginx ligne 7") || !strings.HasSuffix(got[4], "nginx ligne 3") {
		t.Fatalf("unité : %q", got)
	}
	if want := t0.Add(7 * time.Second).Format("15:04:05"); !strings.HasPrefix(got[0], want+" ") {
		t.Errorf("heure : %q (attendu %s)", got[0], want)
	}
	r = notif.RequeteExtrait{Champs: map[string]string{"host": "web1"}, Mots: "killed memory", Debut: t0.Add(-time.Minute), Fin: t0.Add(time.Minute)}
	if got, _ = chercher(ctx, r, notif.ExtraitLignes); len(got) != 1 || !strings.Contains(got[0], "Killed process 4242") {
		t.Errorf("noyau : %q", got)
	}
	r = notif.RequeteExtrait{Champs: map[string]string{"host": "web1", "container": "appli"}, Debut: t0.Add(-time.Minute), Fin: t0.Add(time.Minute)}
	if got, _ = chercher(ctx, r, notif.ExtraitLignes); len(got) != 1 || !strings.Contains(got[0], "panic") {
		t.Errorf("conteneur : %q", got)
	}
}

// Coût d'une recherche d'extrait : 24 h de logs d'un parc (~200 000 lignes,
// 10 machines, 20 unités), recherche d'une unité sur 1 h 10.
func BenchmarkChercheurExtraits(b *testing.B) {
	ctx := context.Background()
	jx, err := journaux.Ouvrir(journaux.Options{Dossier: b.TempDir()})
	if err != nil {
		b.Fatal(err)
	}
	defer jx.Fermer()
	fin := time.Now()
	debut := fin.Add(-24 * time.Hour)
	const n = 200_000
	lot := make([]stockage.Ligne, 0, 5000)
	for k := 0; k < n; k++ {
		lot = append(lot, stockage.Ligne{T: debut.Add(time.Duration(k) * (24 * time.Hour / n)).UnixMilli(),
			Msg:    "message de test numéro " + string(rune('a'+k%26)) + " avec un peu de texte pour la taille habituelle d'une ligne",
			Champs: map[string]string{"host": "m" + string(rune('0'+k%10)), "job": "journal", "unit": "u" + string(rune('a'+k%20)) + ".service"}})
		if len(lot) == cap(lot) {
			if err := jx.Ecrire(ctx, lot); err != nil {
				b.Fatal(err)
			}
			lot = lot[:0]
		}
	}
	_ = jx.Ecrire(ctx, lot)
	_ = jx.Vider()
	chercher := chercheurExtraits(jx)
	r := notif.RequeteExtrait{Champs: map[string]string{"host": "m3", "unit": "ud.service"}, Debut: fin.Add(-70 * time.Minute), Fin: fin}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if got, err := chercher(ctx, r, notif.ExtraitLignes); err != nil || len(got) != notif.ExtraitLignes {
			b.Fatalf("%v %d", err, len(got))
		}
	}
}
