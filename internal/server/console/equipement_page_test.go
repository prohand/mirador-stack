// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/tsdb"

	"mirador-stack/internal/server/alertes"
	"mirador-stack/internal/server/flotte"
)

// Retour du 22/11 : la page d'un NAS relevé en SNMP par le collecteur
// montre les courbes du tableau Synology (et non les courbes Linux vides).
func TestPageEquipementSNMP(t *testing.T) {
	b := nouveauBanc(t)
	c, _ := b.premierAdmin("admin", mdpAdmin)
	ctx := context.Background()
	if _, err := b.c.db.Exec(`INSERT INTO machines(nom, statut, type, cree_le) VALUES ('collecteur-lan', 'approuvee', 'collecteur', 1), ('nas', 'approuvee', 'agent', 1), ('pc1', 'approuvee', 'agent', 1)`); err != nil {
		t.Fatal(err)
	}
	idC, _ := flotte.IDMachine(ctx, b.c.db, "collecteur-lan")
	idNas, _ := flotte.IDMachine(ctx, b.c.db, "nas")
	if err := flotte.RattacherEquipement(ctx, b.c.db, idC, idNas, "admin", time.Now()); err != nil {
		t.Fatal(err)
	}
	db, err := tsdb.Open(t.TempDir(), nil, nil, tsdb.DefaultOptions(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	m := b.h.maintenant()
	app := db.Appender(ctx)
	for ts := m.Add(-10 * time.Minute); !ts.After(m); ts = ts.Add(30 * time.Second) {
		for nom, v := range map[string]float64{"systemStatus": 1, "up": 1, "temperature": 40} {
			_, _ = app.Append(0, labels.FromStrings("__name__", nom, "instance", "nas", "job", "snmp"), ts.UnixMilli(), v)
		}
	}
	_ = app.Commit()
	b.c.lecteur = alertes.NouveauLecteur(db, nil)
	_, corps, _ := b.get(c, "/machines/nas")
	if !strings.Contains(corps, "Stockage") || !strings.Contains(corps, "Onduleur") {
		t.Error("page du NAS : rangées du tableau Synology absentes")
	}
	// Une machine avec agent garde ses courbes Linux.
	if _, corps, _ := b.get(c, "/machines/pc1"); strings.Contains(corps, "Onduleur") {
		t.Error("page d'une machine avec agent : tableau Synology affiché")
	}
}

// Retour du 22/11 : OPNsense (Telegraf, token propre) rattaché au
// collecteur pour son syslog et son certificat : sa page montre le tableau
// OPNsense, filtré sur l'étiquette « host » de Telegraf.
func TestPageEquipementOPNsense(t *testing.T) {
	b := nouveauBanc(t)
	c, _ := b.premierAdmin("admin", mdpAdmin)
	ctx := context.Background()
	if _, err := b.c.db.Exec(`INSERT INTO machines(nom, statut, type, cree_le) VALUES ('collecteur-lan', 'approuvee', 'collecteur', 1), ('fw', 'approuvee', 'agent', 1)`); err != nil {
		t.Fatal(err)
	}
	idC, _ := flotte.IDMachine(ctx, b.c.db, "collecteur-lan")
	idFw, _ := flotte.IDMachine(ctx, b.c.db, "fw")
	if err := flotte.RattacherEquipement(ctx, b.c.db, idC, idFw, "admin", time.Now()); err != nil {
		t.Fatal(err)
	}
	db, err := tsdb.Open(t.TempDir(), nil, nil, tsdb.DefaultOptions(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	m := b.h.maintenant()
	app := db.Appender(ctx)
	for ts := m.Add(-10 * time.Minute); !ts.After(m); ts = ts.Add(30 * time.Second) {
		_, _ = app.Append(0, labels.FromStrings("__name__", "cpu_usage_idle", "cpu", "cpu-total", "instance", "fw", "host", "OPNsense.localdomain"), ts.UnixMilli(), 90)
		_, _ = app.Append(0, labels.FromStrings("__name__", "pf_entries", "instance", "fw", "host", "OPNsense.localdomain"), ts.UnixMilli(), 1200)
	}
	_ = app.Commit()
	b.c.lecteur = alertes.NouveauLecteur(db, nil)
	if tab, variable, valeur := b.c.tableauEquipement(ctx, "fw", m); tab != "opnsense" || variable != "host" || valeur != "OPNsense.localdomain" {
		t.Fatalf("tableau de fw : %s %s=%s", tab, variable, valeur)
	}
	if _, corps, _ := b.get(c, "/machines/fw"); !strings.Contains(corps, "Pare-feu (pf)") {
		t.Error("page de fw : rangées du tableau OPNsense absentes")
	}
	// État du token visible (retour du 22/11 : « on ne voit pas s'il y en a
	// un de configuré »).
	if _, corps, _ := b.get(c, "/machines/fw"); !strings.Contains(corps, "Aucun token") || !strings.Contains(corps, "passent par le collecteur collecteur-lan") {
		t.Error("page de fw sans token : état absent")
	}
	if _, err := flotte.TokenManuel(ctx, b.c.db, "fw", "admin", m); err != nil {
		t.Fatal(err)
	}
	if _, corps, _ := b.get(c, "/machines/fw"); !strings.Contains(corps, "Token actif") || !strings.Contains(corps, "Aucun envoi reçu avec ce token") {
		t.Error("page de fw avec token : état absent")
	}
}

// Retour du 22/11 : « collecteur-lan, fw et nas n'ont pas les mêmes infos »
// que les machines à agent. Les tuiles des équipements ont leurs jauges,
// tirées de Telegraf (OPNsense) et du SNMP (NAS).
func TestJaugesEquipements(t *testing.T) {
	b := nouveauBanc(t)
	c, _ := b.premierAdmin("admin", mdpAdmin)
	if _, err := b.c.db.Exec(`INSERT INTO machines(nom, statut, type, cree_le, vu_le) VALUES ('fw', 'approuvee', 'agent', 1, ?), ('nas', 'approuvee', 'agent', 1, ?)`,
		b.h.maintenant().Unix(), b.h.maintenant().Unix()); err != nil {
		t.Fatal(err)
	}
	db, err := tsdb.Open(t.TempDir(), nil, nil, tsdb.DefaultOptions(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	m := b.h.maintenant()
	app := db.Appender(context.Background())
	for ts := m.Add(-10 * time.Minute); !ts.After(m); ts = ts.Add(30 * time.Second) {
		for _, s := range []struct {
			l []string
			v float64
		}{
			{[]string{"__name__", "cpu_usage_idle", "cpu", "cpu-total", "instance", "fw"}, 90},
			{[]string{"__name__", "mem_used_percent", "instance", "fw"}, 40},
			{[]string{"__name__", "disk_used_percent", "fstype", "zfs", "instance", "fw"}, 25},
			{[]string{"__name__", "disk_used_percent", "fstype", "devfs", "instance", "fw"}, 100}, // ignoré
			{[]string{"__name__", "ssCpuIdle", "job", "snmp", "instance", "nas"}, 95},
			{[]string{"__name__", "memTotalReal", "job", "snmp", "instance", "nas"}, 1000},
			{[]string{"__name__", "memAvailReal", "job", "snmp", "instance", "nas"}, 200},
			{[]string{"__name__", "memBuffer", "job", "snmp", "instance", "nas"}, 100},
			{[]string{"__name__", "memCached", "job", "snmp", "instance", "nas"}, 200},
			{[]string{"__name__", "raidFreeSize", "job", "snmp", "raidName", "Volume 1", "instance", "nas"}, 300},
			{[]string{"__name__", "raidTotalSize", "job", "snmp", "raidName", "Volume 1", "instance", "nas"}, 1000},
		} {
			_, _ = app.Append(0, labels.FromStrings(s.l...), ts.UnixMilli(), s.v)
		}
	}
	_ = app.Commit()
	b.c.lecteur = alertes.NouveauLecteur(db, nil)
	_, corps, _ := b.get(c, "/machines")
	tuile := func(nom string) string {
		i := strings.Index(corps, `href="/machines/`+nom+`">`+nom+`</a>`)
		if i < 0 {
			t.Fatalf("tuile %s absente", nom)
		}
		fin := strings.Index(corps[i:], `class="tuile-machine`)
		if fin < 0 {
			fin = len(corps) - i
		}
		return corps[i : i+fin]
	}
	for nom, attendus := range map[string][]string{
		"fw":  {"largeur-10", "largeur-40", "largeur-25"},
		"nas": {"largeur-5", "largeur-50", "largeur-70"},
	} {
		x := tuile(nom)
		for _, a := range attendus {
			if !strings.Contains(x, a) {
				t.Errorf("tuile %s : %s absent", nom, a)
			}
		}
	}
}
