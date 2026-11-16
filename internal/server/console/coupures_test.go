// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/tsdb"

	"mirador-stack/internal/server/alertes"
	"mirador-stack/internal/server/notif"
)

func pannePour(id int64, machine string, debut, fin time.Time) alertes.Alerte {
	return alertes.Alerte{ID: id, Regle: "AgentMuet", Etat: alertes.Resolue, ActiveLe: debut, Debut: debut, Fin: fin,
		Etiquettes: labels.FromStrings("alertname", "AgentMuet", "instance", machine, "severity", "critical")}
}

// Coupures retrouvées dans l'historique : au moins 2 machines d'un même
// site à moins de 10 min d'écart ; une machine seule n'en est pas une.
func TestTrouverCoupures(t *testing.T) {
	t0 := time.Date(2026, 11, 16, 10, 30, 46, 0, time.UTC)
	sites := map[string]string{"pve": "maison", "dns": "maison", "nas": "maison", "vps": "ovh", "a": "", "b": ""}
	as := []alertes.Alerte{
		pannePour(1, "dns", t0, t0.Add(2*time.Hour)),
		pannePour(2, "pve", t0.Add(40*time.Second), time.Time{}), // encore muette
		pannePour(3, "nas", t0.Add(-24*time.Hour), t0.Add(-23*time.Hour)),
		pannePour(4, "vps", t0, t0.Add(time.Hour)),
		pannePour(5, "a", t0, t0.Add(time.Hour)),
		pannePour(6, "b", t0.Add(9*time.Minute), t0.Add(time.Hour)),
		pannePour(7, "supprimee", t0, t0.Add(time.Hour)),
		{ID: 8, Regle: "CpuEleve", ActiveLe: t0, Etiquettes: labels.FromStrings("instance", "nas")},
	}
	cs := trouverCoupures(as, []string{"AgentMuet"}, sites)
	if len(cs) != 2 {
		t.Fatalf("2 coupures attendues : %+v", cs)
	}
	for _, c := range cs {
		switch c.Site {
		case "maison":
			if !c.Fin.IsZero() || strings.Join(c.Machines, ",") != "dns,pve" || !c.Debut.Equal(t0) {
				t.Errorf("maison : %+v", c)
			}
		case sansSite:
			if !c.Fin.Equal(t0.Add(time.Hour)) || len(c.Machines) != 2 {
				t.Errorf("sans site : %+v", c)
			}
		default:
			t.Errorf("coupure inattendue : %+v", c)
		}
	}
}

// Page : liste et bilan, avec des métriques simulées. pve a tout rattrapé ;
// nas a un trou (lots jetés, relevés manquants).
func TestPageCoupures(t *testing.T) {
	b := nouveauBanc(t)
	m := b.h.maintenant()
	debut, fin := m.Add(-3*time.Hour), m.Add(-time.Hour)
	db, err := tsdb.Open(t.TempDir(), nil, nil, tsdb.DefaultOptions(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	app := db.Appender(context.Background())
	envoye, jetes := 0.0, 0.0
	for ts := m.Add(-5 * time.Hour); !ts.After(m); ts = ts.Add(30 * time.Second) {
		dedans := !ts.Before(debut) && ts.Before(fin)
		ajout := func(inst, nom string, v float64) {
			_, _ = app.Append(0, labels.FromStrings("__name__", nom, "instance", inst), ts.UnixMilli(), v)
		}
		envoye += 120 * 30 // 120 octets/s : ~19 h d'autonomie avec 8 Mio
		file := 0.0
		if dedans {
			file = float64(ts.Sub(debut)) / float64(fin.Sub(debut)) * float64(3<<20)
		}
		for _, inst := range []string{"pve", "nas"} {
			ajout(inst, "mirador_agent_sent_bytes_total", envoye)
			ajout(inst, "mirador_agent_queue_bytes", file)
			ajout(inst, "mirador_logs_queue_dropped_total", 0)
		}
		ajout("pve", "node_uname_info", 1)
		ajout("pve", "mirador_agent_queue_dropped_total", 0)
		// nas : la première heure de la coupure est perdue (3 lots jetés).
		if !dedans || ts.Sub(debut) >= time.Hour {
			ajout("nas", "node_uname_info", 1)
		}
		if dedans && ts.Sub(debut) == time.Hour {
			jetes = 3
		}
		ajout("nas", "mirador_agent_queue_dropped_total", jetes)
	}
	_ = app.Commit()
	b.c.lecteur = alertes.NouveauLecteur(db, nil)
	for _, nom := range []string{"pve", "nas"} {
		if _, err := b.c.db.Exec(`INSERT INTO machines(nom, site, statut, type, cree_le, vu_le) VALUES (?, 'maison', 'approuvee', 'agent', 1, ?)`, nom, m.Unix()); err != nil {
			t.Fatal(err)
		}
	}
	for i, nom := range []string{"pve", "nas"} {
		if _, err := b.c.db.Exec(`INSERT INTO alertes(regle, etiquettes, gravite, etat, debut, active_le, fin) VALUES ('AgentMuet', ?, 'critical', 'resolue', ?, ?, ?)`,
			`{"alertname":"AgentMuet","instance":"`+nom+`","severity":"critical"}`, debut.UnixMilli(), debut.Add(time.Duration(i)*20*time.Second).UnixMilli(),
			fin.Add(time.Duration(i)*20*time.Second).UnixMilli()); err != nil {
			t.Fatal(err)
		}
	}
	c, _ := b.premierAdmin("admin", mdpAdmin)
	avant := time.Now()
	code, corps, _ := b.get(c, "/coupures?site=maison")
	t.Logf("page des coupures (2 machines, 5 h de relevés à 30 s) : %v, %d octets", time.Since(avant), len(corps))
	if code != 200 {
		t.Fatalf("code %d", code)
	}
	for _, attendu := range []string{"Coupures de site", "Bilan : site maison", "Coupures des 30 derniers jours (1)",
		`<span class="pastille ok">Rattrapé</span>`, "Trou : 3 lot(s) jeté(s)", "3,0 Mo", "(37 % de 8 Mo)", "19 h", `class="actif" href="/coupures"`} {
		if !strings.Contains(corps, attendu) {
			t.Errorf("page des coupures : %q absent", attendu)
		}
	}
	if _, corps, _ = b.get(c, "/coupures?site=inconnu"); !strings.Contains(corps, "Aucune coupure trouvée pour le site inconnu") {
		t.Error("site sans coupure : message absent")
	}
}

// État d'une ligne pendant la coupure : tampon encore suffisant ou dépassé.
func TestEtatBilanPendantLaCoupure(t *testing.T) {
	m := time.Now()
	if e, c := etatBilan(ligneBilan{Limite: m.Add(time.Hour)}, m); c != "attention" || !strings.Contains(e, "gardées dans le tampon") {
		t.Errorf("tampon suffisant : %s %s", e, c)
	}
	if e, c := etatBilan(ligneBilan{Limite: m.Add(-time.Hour)}, m); c != "erreur" || !strings.Contains(e, "jetés") {
		t.Errorf("tampon dépassé : %s %s", e, c)
	}
	if e, _ := etatBilan(ligneBilan{Revenue: m.Add(-time.Minute), Couverture: 40, Jetes: 0}, m); e != "Rattrapage en cours" {
		t.Errorf("juste revenue : %s", e)
	}
}

// Bandeau « Site injoignable » sur la page Alertes (et l'accueil).
func TestBandeauSiteInjoignable(t *testing.T) {
	b := nouveauBanc(t)
	w := httptest.NewRecorder()
	depuis := time.Date(2026, 11, 16, 9, 30, 46, 0, time.UTC)
	b.c.pages.executer(w, http.StatusOK, "alertes-fragment", Page{D: donneesEnCours{Vue: "machine",
		Sites: []notif.SiteInjoignable{{Site: "maison", Machines: []string{"dhcpdns02", "proxmoxbackup"}, Depuis: depuis}}}})
	corps := w.Body.String()
	for _, attendu := range []string{"Site maison injoignable", "ses 2 machines (dhcpdns02, proxmoxbackup)", `href="/coupures?site=maison"`} {
		if !strings.Contains(corps, attendu) {
			t.Errorf("bandeau : %q absent de %s", attendu, corps)
		}
	}
}
