// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"mirador-stack/internal/protocole"
	"mirador-stack/internal/server/flotte"
)

// Cause probable et pannes vues tout de suite (SPEC § 98).
func TestCauseProbableEtPannes(t *testing.T) {
	b := nouveauBanc(t)
	b.machinesApprouvees(t, "web1")
	m := b.h.maintenant()
	proc := func(rang, nom string, v float64) serieTest {
		return serieTest{etq: []string{"__name__", "mirador_processus_cpu_pourcent", "instance", "web1", "rang", rang, "nom", nom, "groupe", ""}, f: constante(v)}
	}
	b.metriquesSimulees(t, []serieTest{proc("1", "vzdump", 86), proc("2", "kvm", 41), proc("3", "zstd", 30), proc("4", "sshd", 1)})
	ctx := context.Background()
	debut := m.Add(-time.Hour)
	cause, quoi, _ := b.c.causeProbable(ctx, "web1", `avg by (instance) (rate(node_cpu_seconds_total{mode!="idle"}[5m])) > 0.9`, debut)
	if quoi != "CPU" || len(cause) != 3 || cause[0].Nom != "vzdump" || cause[0].Valeur != "86 %" || cause[2].Nom != "zstd" {
		t.Fatalf("cause : %q %+v", quoi, cause)
	}
	if c, _, _ := b.c.causeProbable(ctx, "web1", `node_filesystem_avail_bytes < 1`, debut); c != nil {
		t.Errorf("règle de disque : cause %+v", c)
	}
	// Panne vue par l'agent, montrée sur l'alerte et dans le journal.
	var id int64
	_ = b.c.db.QueryRow(`SELECT id FROM machines WHERE nom = 'web1'`).Scan(&id)
	if err := flotte.NoterPanne(ctx, b.c.db, id, debut.Add(-time.Second), protocole.EvenementAgent{Type: "service", Objet: "nginx.service"}); err != nil {
		t.Fatal(err)
	}
	r, err := b.c.db.Exec(`INSERT INTO alertes(regle, etiquettes, gravite, etat, debut, active_le) VALUES ('LinuxServiceEnEchec',
		'{"alertname":"LinuxServiceEnEchec","instance":"web1","name":"nginx.service"}', 'critical', 'active', ?, ?)`, debut.UnixMilli(), debut.UnixMilli())
	if err != nil {
		t.Fatal(err)
	}
	ida, _ := r.LastInsertId()
	c, _ := b.premierAdmin("admin", "une longue phrase de passe")
	if _, corps, _ := b.get(c, "/alertes/"+strconv.FormatInt(ida, 10)); !strings.Contains(corps, "Vu tout de suite") || !strings.Contains(corps, "nginx.service est passé en échec") {
		t.Error("« Vu tout de suite » absent de la page de l'alerte")
	}
	if _, corps, _ := b.get(c, "/evenements?periode=24h"); !strings.Contains(corps, "nginx.service est passé en échec") {
		t.Error("panne absente du journal des événements")
	}
}
