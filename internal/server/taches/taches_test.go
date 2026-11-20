// SPDX-License-Identifier: GPL-3.0-or-later

package taches

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/prometheus/prometheus/model/labels"

	"mirador-stack/internal/server/alertes"
	"mirador-stack/internal/server/base"
)

// Les règles des tâches sont acceptées par le langage restreint.
func TestReglesTaches(t *testing.T) {
	n := 0
	for _, r := range alertes.ReglesMirador() {
		if r.Groupe != "taches" {
			continue
		}
		n++
		if _, err := alertes.AnalyserExpr(r.Expr); err != nil {
			t.Errorf("%s : %v", r.Nom, err)
		}
	}
	if n != 6 {
		t.Fatalf("%d règles de tâches", n)
	}
}

// Échéance et état : période réglée, timer (prochaine exécution), échec.
func TestCalculer(t *testing.T) {
	m := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	jour := Tache{Periode: 24 * time.Hour, Marge: time.Hour, CreeLe: m.Add(-72 * time.Hour)}
	if e := Calculer(jour, Releve{Fin: m.Add(-10 * time.Hour)}, m); e.Probleme || e.Texte != "OK" {
		t.Errorf("à l'heure : %+v", e)
	}
	if e := Calculer(jour, Releve{Fin: m.Add(-3 * 24 * time.Hour)}, m); !e.Probleme || e.Texte != "En retard de 47 h" {
		t.Errorf("en retard : %+v", e)
	}
	if e := Calculer(jour, Releve{Fin: m.Add(-time.Hour), Code: 3}, m); !e.Probleme || e.Texte != "Échec · code 3" {
		t.Errorf("échec : %+v", e)
	}
	if e := Calculer(Tache{Marge: time.Hour, CreeLe: m}, Releve{Fin: m.Add(-time.Hour), Prochaine: m.Add(-2 * time.Hour)}, m); !e.Probleme {
		t.Errorf("timer qui n'a pas tourné à l'heure prévue : %+v", e)
	}
	if e := Calculer(jour, Releve{}, m); !e.Probleme || e.Texte != "Jamais reçue" {
		t.Errorf("jamais reçue après le délai : %+v", e)
	}
	if e := Calculer(Tache{Periode: time.Hour, CreeLe: m}, Releve{}, m); e.Probleme || e.Classe != "inconnu" {
		t.Errorf("tout juste créée : %+v", e)
	}
}

// Ping : création, signal réussi, échec par code, jeton inconnu ; séries.
func TestPingEtSeries(t *testing.T) {
	db, err := base.Ouvrir(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	if _, err := base.Migrer(ctx, db, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	m := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	jeton, err := CreerPing(ctx, db, Tache{Nom: "Sauvegarde NAS", Machine: "nas", Periode: 24 * time.Hour, Marge: time.Hour, Gravite: "critical"}, m)
	if err != nil || len(jeton) < 20 {
		t.Fatalf("création : %q %v", jeton, err)
	}
	var e string
	_ = db.QueryRow(`SELECT jeton FROM taches`).Scan(&e)
	if e == jeton {
		t.Fatal("le jeton doit être gardé haché")
	}
	if err := Signaler(ctx, db, jeton, false, 0, 3*time.Minute, "", m.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := Signaler(ctx, db, "inconnu-inconnu-inconnu", false, 0, 0, "", m); err != ErrJetonInconnu {
		t.Fatalf("jeton inconnu : %v", err)
	}
	ts, _ := Lister(ctx, db)
	if len(ts) != 1 || ts[0].Releve.Fin.Unix() != m.Add(time.Hour).Unix() || ts[0].Releve.Debut.Unix() != m.Add(57*time.Minute).Unix() {
		t.Fatalf("relevé : %+v", ts)
	}
	_ = Signaler(ctx, db, jeton, false, 2, 0, "disque plein", m.Add(2*time.Hour))
	ts, _ = Lister(ctx, db)
	series := map[string]float64{}
	for _, s := range Series(ts, nil) {
		series[s.Nom+"|"+s.Etiquettes[1]+"|"+s.Etiquettes[len(s.Etiquettes)-1]] = s.Valeur
	}
	if series["mirador_tache_code|nas|Sauvegarde NAS"] != 2 || series["mirador_tache_suivie|nas|critical"] != 1 ||
		series["mirador_tache_echeance_timestamp_seconds|nas|critical"] != float64(m.Add(2*time.Hour+25*time.Hour).Unix()) {
		t.Fatalf("séries : %v", series)
	}
	// Tâche d'agent : relevé lu dans les métriques.
	if err := Suivre(ctx, db, Tache{Nom: "certbot", Source: "systemd", Machine: "web1", Marge: time.Hour, Gravite: "warning"}, m); err != nil {
		t.Fatal(err)
	}
	if err := Suivre(ctx, db, Tache{Nom: "certbot", Source: "systemd", Machine: "web1", Marge: time.Hour, Gravite: "warning"}, m); err == nil {
		t.Fatal("suivie deux fois")
	}
	det := Detectees(func(expr string) ([]alertes.Echantillon, error) {
		l := labels.FromStrings("instance", "web1", "source", "systemd", "tache", "certbot")
		switch expr {
		case "mirador_tache_fin_timestamp_seconds":
			return []alertes.Echantillon{{Etiquettes: l, Valeur: float64(m.Unix())}}, nil
		case "mirador_tache_prochaine_timestamp_seconds":
			return []alertes.Echantillon{{Etiquettes: l, Valeur: float64(m.Add(12 * time.Hour).Unix())}}, nil
		}
		return nil, nil
	})
	ts, _ = Lister(ctx, db)
	for _, x := range AvecReleves(ts, det) {
		if x.Source == "systemd" && Echeance(x, x.Releve).Unix() != m.Add(13*time.Hour).Unix() {
			t.Fatalf("échéance d'un timer : %v", Echeance(x, x.Releve))
		}
	}
}

// Sauvegarde Proxmox VE (SPEC § 89) : détail lu dans les métriques, échec
// partiel, planning Proxmox comme fréquence, nom affiché gardant la clé.
func TestSauvegardeProxmox(t *testing.T) {
	db, err := base.Ouvrir(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	if _, err := base.Migrer(ctx, db, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	m := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	base0 := []string{"instance", "proxmoxbackup", "source", "pve", "tache", "vzdump"}
	et := func(kv ...string) labels.Labels {
		return labels.FromStrings(append(append([]string{}, base0...), kv...)...)
	}
	det := Detectees(func(expr string) ([]alertes.Echantillon, error) {
		switch expr {
		case "mirador_tache_fin_timestamp_seconds":
			return []alertes.Echantillon{{Etiquettes: et(), Valeur: float64(m.Add(-10 * time.Hour).Unix())}}, nil
		case "mirador_tache_code":
			return []alertes.Echantillon{{Etiquettes: et(), Valeur: 1}}, nil
		case "mirador_tache_prochaine_timestamp_seconds":
			return []alertes.Echantillon{{Etiquettes: et(), Valeur: float64(m.Add(14 * time.Hour).Unix())}}, nil
		case "mirador_tache_info":
			return []alertes.Echantillon{{Etiquettes: et("stockage", "pbs-maison", "planning", "02:00"), Valeur: 1}}, nil
		case "mirador_tache_vm":
			return []alertes.Echantillon{{Etiquettes: et("vm", "101", "type", "qemu", "nom", "web"), Valeur: 1},
				{Etiquettes: et("vm", "104", "type", "qemu", "nom", "nextcloud"), Valeur: 0},
				{Etiquettes: et("vm", "200", "type", "lxc", "nom", "ha"), Valeur: 1}}, nil
		case "mirador_tache_vm_octets":
			return []alertes.Echantillon{{Etiquettes: et("vm", "101"), Valeur: 4 << 30}}, nil
		case "mirador_tache_vm_message":
			return []alertes.Echantillon{{Etiquettes: et("vm", "104", "message", "unable to create temporary directory"), Valeur: 1}}, nil
		}
		return nil, nil
	})
	d := det[CleAgent("proxmoxbackup", "pve", "vzdump")]
	if d == nil || d.Releve.Stockage != "pbs-maison" || d.Releve.Planning != "02:00" || len(d.Releve.VMs) != 3 ||
		d.Releve.VMs[0].ID != "104" || d.Releve.VMs[0].Message == "" || d.Releve.VMs[1].Octets != 4<<30 {
		t.Fatalf("détail : %+v", d)
	}
	// Suivie « selon le planning Proxmox », avec un nom affiché.
	if err := Suivre(ctx, db, Tache{Nom: "vzdump", Source: "pve", Machine: "proxmoxbackup", Marge: 2 * time.Hour, Gravite: "critical",
		Libelle: "Sauvegardes des VM (nuit)"}, m); err != nil {
		t.Fatal(err)
	}
	ts, _ := Lister(ctx, db)
	ts = AvecReleves(ts, det)
	if ts[0].Affiche() != "Sauvegardes des VM (nuit)" || ts[0].Cle != CleAgent("proxmoxbackup", "pve", "vzdump") {
		t.Fatalf("nom affiché : %+v", ts[0])
	}
	if e := Calculer(ts[0], ts[0].Releve, m); e.Texte != "Échec partiel · 1/3" || !e.Probleme || !e.Echeance.Equal(m.Add(16*time.Hour)) {
		t.Fatalf("état : %+v", e)
	}
	// Nom affiché retiré : retour au nom d'origine.
	ts[0].Libelle = ""
	if err := Modifier(ctx, db, ts[0]); err != nil {
		t.Fatal(err)
	}
	if ts, _ = Lister(ctx, db); ts[0].Affiche() != "vzdump" {
		t.Errorf("nom affiché vide : %+v", ts[0])
	}
	ts[0].Libelle = "a\nb"
	if err := Modifier(ctx, db, ts[0]); err == nil {
		t.Error("nom affiché sur deux lignes accepté")
	}
}
