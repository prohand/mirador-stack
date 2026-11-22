// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"context"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/tsdb"

	"mirador-stack/internal/server/alertes"
	"mirador-stack/internal/server/flotte"
)

// Retour du 22/11 : l'écoute syslog se règle en cochant une interface du
// collecteur (adresses privées qu'il envoie) et en donnant le port.
func TestEcouteSyslogInterface(t *testing.T) {
	b := nouveauBanc(t)
	c, secret := b.premierAdmin("admin", mdpAdmin)
	ctx := context.Background()
	_, _ = b.c.db.Exec(`INSERT INTO machines(nom, statut, type, cree_le) VALUES ('collecteur-lan', 'approuvee', 'collecteur', 1), ('nas', 'approuvee', 'agent', 1)`)
	idC, _ := flotte.IDMachine(ctx, b.c.db, "collecteur-lan")
	idNas, _ := flotte.IDMachine(ctx, b.c.db, "nas")
	_ = flotte.RattacherEquipement(ctx, b.c.db, idC, idNas, "admin", time.Now())
	db, err := tsdb.Open(t.TempDir(), nil, nil, tsdb.DefaultOptions(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	m := b.h.maintenant()
	app := db.Appender(ctx)
	for _, a := range [][3]string{{"ens18", "192.168.35.10", "1"}, {"docker0", "172.17.0.1", "0"}, {"ens19", "203.0.113.5", "0"}} {
		_, _ = app.Append(0, labels.FromStrings("__name__", "mirador_adresse_info", "instance", "collecteur-lan", "interface", a[0], "adresse", a[1], "principale", a[2]), m.Add(-time.Minute).UnixMilli(), 1)
	}
	_ = app.Commit()
	b.c.lecteur = alertes.NouveauLecteur(db, nil)
	_, page, _ := b.get(c, "/machines/collecteur-lan")
	if !strings.Contains(page, `value="192.168.35.10" checked`) || !strings.Contains(page, `value="172.17.0.1"`) || strings.Contains(page, "203.0.113.5") ||
		!strings.Contains(page, `name="ecoute_port" value="6514"`) {
		t.Fatal("choix des interfaces : adresse privée principale cochée, adresse publique absente, port 6514 proposé")
	}
	b.post(c, "/machines/collecteur-lan", url.Values{"csrf": {b.csrf(c, "/machines")}, "action": {"syslog"}, "code": {b.code(secret)},
		"ecoute_ip": {"192.168.35.10"}, "ecoute_port": {"6515"}, "nom_certificat": {"syslog.lan.exemple.org"}, "emetteurs": {"192.168.35.7 nas"}})
	cfg, err := flotte.ConfigEffective(ctx, b.c.db, idC)
	if err != nil || cfg.Collecteur == nil || cfg.Collecteur.Syslog == nil || cfg.Collecteur.Syslog.Ecoute != "192.168.35.10:6515" {
		t.Fatalf("écoute enregistrée : %+v %v", cfg.Collecteur, err)
	}
	_, page, _ = b.get(c, "/machines/collecteur-lan")
	if !strings.Contains(page, `value="192.168.35.10" checked`) || !strings.Contains(page, `name="ecoute_port" value="6515"`) {
		t.Error("écoute enregistrée non réaffichée")
	}
	// Case « filterlog » : seulement avec un pare-feu parmi les émetteurs.
	if strings.Contains(page, `name="garder_filterlog"`) {
		t.Error("case filterlog affichée sans pare-feu")
	}
	app = db.Appender(ctx)
	_, _ = app.Append(0, labels.FromStrings("__name__", "mirador_collecteur_syslog_refus_total", "instance", "collecteur-lan", "motif", "filterlog"), m.Add(-time.Minute).UnixMilli(), 120)
	_ = app.Commit()
	b.h.avancer(2 * time.Minute) // cache des requêtes de la page
	if _, page, _ := b.get(c, "/machines/collecteur-lan"); !strings.Contains(page, `name="garder_filterlog"`) {
		t.Error("case filterlog absente alors que des lignes filterlog sont arrivées")
	}
}
