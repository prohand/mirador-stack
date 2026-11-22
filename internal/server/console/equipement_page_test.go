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
