// SPDX-License-Identifier: GPL-3.0-or-later

package journaux

import (
	"fmt"
	"math/rand/v2"
	"os"
	"runtime"
	"testing"
	"time"

	"mirador-stack/internal/server/stockage"
)

// TestMesureJournee : une journée du parc réel (≈ 1,4 M lignes, profil du
// § 16.1). Lancée à la main : MESURE=1 go test -run TestMesureJournee -v
func TestMesureJournee(t *testing.T) {
	if os.Getenv("MESURE") == "" {
		t.Skip("MESURE=1 pour lancer la mesure")
	}
	dir := t.TempDir()
	h := nouvelleHorloge(t0)
	m, err := Ouvrir(Options{Dossier: dir, Maintenant: h.maintenant})
	if err != nil {
		t.Fatal(err)
	}
	r := rand.New(rand.NewPCG(1, 2)) // #nosec G404 -- données de test
	units := []string{"ssh.service", "cron.service", "systemd-logind.service", "nginx.service", "kernel"}
	conteneurs := []string{"nginx", "victoriametrics", "grafana", "crowdsec", "unbound"}
	const parSeconde = 16 // 1,4 M lignes / jour
	var cpu time.Duration
	lignes := 0
	for s := 0; s < 86400; s++ {
		h.regler(t0.Add(time.Duration(s) * time.Second))
		lot := make([]stockage.Ligne, 0, parSeconde)
		for i := 0; i < parSeconde; i++ {
			host := fmt.Sprintf("machine%02d", r.IntN(25))
			l := stockage.Ligne{T: t0.Add(time.Duration(s)*time.Second + time.Duration(i)*time.Millisecond).UnixMilli(), Champs: map[string]string{"host": host}}
			if r.IntN(10) < 6 {
				c := conteneurs[r.IntN(len(conteneurs))]
				l.Champs["job"], l.Champs["container"], l.Champs["stream"] = "docker", c, "stdout"
				l.Msg = fmt.Sprintf("%d.%d.%d.%d - - \"GET /api/v1/query_range?query=up&step=%d HTTP/1.1\" 200 %d \"-\" \"Grafana/11.%d\"", r.IntN(256), r.IntN(256), r.IntN(256), r.IntN(256), r.IntN(60), r.IntN(100000), r.IntN(9))
			} else {
				u := units[r.IntN(len(units))]
				lvl := "info"
				if r.IntN(100) == 0 {
					lvl = "err"
				}
				l.Champs["job"], l.Champs["unit"], l.Champs["level"] = "journal", u, lvl
				l.Msg = fmt.Sprintf("pam_unix(sshd:session): session opened for user u%d(uid=%d) by (uid=0) id %x", r.IntN(50), 1000+r.IntN(50), r.Uint64())
			}
			lot = append(lot, l)
		}
		d := time.Now()
		if err := m.Ecrire(ctx, lot); err != nil {
			t.Fatal(err)
		}
		cpu += time.Since(d)
		lignes += len(lot)
	}
	// Une ligne rare, cherchée plus bas.
	_ = m.Ecrire(ctx, []stockage.Ligne{{T: t0.Add(12 * time.Hour).UnixMilli(), Msg: "kernel: Out of memory: Killed process 4242 (zabbix)", Champs: map[string]string{"host": "machine07", "job": "journal", "level": "err"}}})
	_ = m.Vider()
	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	t.Logf("écriture : %d lignes, %.2f µs par ligne, %d blocs, disque %.1f Mo, tas %.1f Mo",
		lignes, float64(cpu.Microseconds())/float64(lignes), m.Blocs.Load(), float64(m.Taille())/1e6, float64(ms.HeapInuse)/1e6)

	debut, fin := t0.UnixMilli(), t0.Add(24*time.Hour).UnixMilli()
	cas := []struct {
		nom string
		r   Requete
	}{
		{"erreurs d'une machine, 1 h", Requete{Debut: t0.Add(11 * time.Hour).UnixMilli(), Fin: t0.Add(12 * time.Hour).UnixMilli(), Champs: map[string]string{"host": "machine07", "level": "err"}}},
		{"mot rare, 24 h, toutes machines", Requete{Debut: debut, Fin: fin, Mots: "killed process"}},
		{"mot rare, 24 h, une machine", Requete{Debut: debut, Fin: fin, Mots: "killed", Champs: map[string]string{"host": "machine07"}}},
		{"morceau de texte, 24 h", Requete{Debut: debut, Fin: fin, Contient: "out of mem"}},
		{"500 dernières lignes, 24 h", Requete{Debut: debut, Fin: fin}},
	}
	for _, c := range cas {
		d := time.Now()
		res, err := m.Chercher(ctx, c.r, LimitesRechercheDefaut())
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("%-34s : %4d lignes, %6.0f ms, %4d blocs lus, %4d sautés, tronqué %v",
			c.nom, len(res.Lignes), float64(time.Since(d).Microseconds())/1000, res.BlocsLus, res.BlocsSautes, res.Tronque)
	}
	_ = m.Fermer()
	// Redémarrage : relecture de l'index.
	d := time.Now()
	m, err = Ouvrir(Options{Dossier: dir, Maintenant: h.maintenant})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Fermer()
	t.Logf("redémarrage : index relu en %v", time.Since(d).Round(time.Millisecond))
}
