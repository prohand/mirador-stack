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
	"mirador-stack/internal/server/certsyslog"
	"mirador-stack/internal/server/flotte"
)

// Avancement complet (SPEC § 32) : le dernier pas est confirmé par les
// mesures du collecteur (écoute ouverte, certificat chargé).
func TestAvancementEcouteOuverte(t *testing.T) {
	b := nouveauBanc(t)
	db, err := tsdb.Open(t.TempDir(), nil, nil, tsdb.DefaultOptions(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	m := b.h.maintenant()
	fin := m.Add(80 * 24 * time.Hour).Unix()
	app := db.Appender(context.Background())
	for ts := m.Add(-10 * time.Minute); !ts.After(m); ts = ts.Add(30 * time.Second) {
		_, _ = app.Append(0, labels.FromStrings("__name__", "mirador_collecteur_syslog_ecoute", "instance", "collecteur-lan"), ts.UnixMilli(), 1)
		_, _ = app.Append(0, labels.FromStrings("__name__", "mirador_collecteur_syslog_certificat_fin_timestamp_seconds", "instance", "collecteur-lan"), ts.UnixMilli(), float64(fin))
	}
	_ = app.Commit()
	b.c.lecteur = alertes.NouveauLecteur(db, nil)
	s := vueSyslog{Regle: true, Nom: "syslog.lan.exemple.org", Ecoute: "192.168.1.10:6515",
		Cert: certsyslog.Etat{Nom: "syslog.lan.exemple.org", CSR: true, CSRRecueLe: m.Unix(), Certificat: "PEM", Fin: fin}}
	r := reglagesCertificats{Domaine: "exemple.org", JetonEnregistre: true}
	a := b.c.avancementCertificat(context.Background(), "collecteur-lan", s, r, m)
	for _, e := range a.Etapes {
		if e.Etat != "fait" {
			t.Errorf("étape %q : %s (%s)", e.Titre, e.Etat, e.Detail)
		}
	}
	if a.EnCours || a.Resume != "Écoute ouverte" || a.Classe != "ok" || len(a.Etapes) != 6 {
		t.Errorf("résumé : %+v", a)
	}
	// Certificat obtenu mais pas encore chargé par le collecteur (autre fin).
	s.Cert.Fin = fin + 30*86400
	if a = b.c.avancementCertificat(context.Background(), "collecteur-lan", s, r, m); a.Etapes[5].Etat != "encours" || !a.EnCours {
		t.Errorf("certificat pas encore chargé : %+v", a.Etapes[5])
	}
}

// « Renouveler » depuis Réglages › Certificats : demande notée ; refusé
// pour une machine qui n'est pas un collecteur.
func TestRenouvelerDepuisReglages(t *testing.T) {
	b := nouveauBanc(t)
	c, secret := b.premierAdmin("admin", mdpAdmin)
	ctx := context.Background()
	csrf := b.csrf(c, "/reglages/certificats")
	if _, err := b.c.db.Exec(`INSERT INTO machines(nom, statut, type, cree_le) VALUES ('collecteur-lan', 'approuvee', 'collecteur', 1), ('web1', 'approuvee', 'agent', 1)`); err != nil {
		t.Fatal(err)
	}
	idC, _ := flotte.IDMachine(ctx, b.c.db, "collecteur-lan")
	_, _ = b.c.db.Exec(`INSERT INTO certificats_syslog(machine_id, nom, csr, csr_recue_le) VALUES (?, '', 'x', 1)`, idC)
	_, corps, _ := b.get(c, "/reglages/certificats")
	for _, attendu := range []string{`href="/machines/collecteur-lan#certificat">collecteur-lan</a>`, `<span class="pastille">syslog pas réglé</span>`, `href="/machines/collecteur-lan#syslog"`,
		`value="renouveler"`, "Compte DNS et Let's Encrypt", `<span class="pastille attention">À faire</span>`, `href="/reglages/certificats">Certificats</a>`} {
		if !strings.Contains(corps, attendu) {
			t.Errorf("page Certificats : %q absent", attendu)
		}
	}
	if strings.Contains(corps, ">web1<") {
		t.Error("une machine à agent listée parmi les collecteurs")
	}
	b.h.avancer(31 * time.Second)
	b.post(c, "/reglages/certificats", url.Values{"csrf": {csrf}, "action": {"renouveler"}, "collecteur": {"web1"}, "code": {b.code(secret)}})
	b.post(c, "/reglages/certificats", url.Values{"csrf": {csrf}, "action": {"renouveler"}, "collecteur": {"collecteur-lan"}})
	if e, _ := certsyslog.LireEtat(ctx, b.c.db, idC); e.DemandeLe == 0 {
		t.Error("renouvellement non demandé")
	}
}
