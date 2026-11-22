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
