// SPDX-License-Identifier: GPL-3.0-or-later

package racine

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Journaux PBS d'exemple (forme des journaux de PBS 3, réduits).
const (
	journalGC = `2026-11-20T03:00:00+01:00: starting garbage collection on store store1
2026-11-20T03:00:00+01:00: Start GC phase1 (mark used chunks)
2026-11-20T03:05:12+01:00: Start GC phase2 (sweep unused chunks)
2026-11-20T03:06:40+01:00: Removed garbage: 12.345 GiB
2026-11-20T03:06:40+01:00: Removed chunks: 5123
2026-11-20T03:06:40+01:00: Pending removals: 1.2 GiB (in 512 chunks)
2026-11-20T03:06:40+01:00: Original data usage: 4.567 TiB
2026-11-20T03:06:40+01:00: On-Disk usage: 345.6 GiB (7.39%)
2026-11-20T03:06:40+01:00: On-Disk chunks: 123456
2026-11-20T03:06:40+01:00: Deduplication factor: 13.53
2026-11-20T03:06:40+01:00: TASK OK
`
	journalVerif = `2026-11-20T04:00:00+01:00: verify datastore store1
2026-11-20T04:00:01+01:00: verify group store1:vm/100 (2 snapshots)
2026-11-20T04:00:01+01:00: verify store1:vm/100/2026-11-19T01:00:00Z
2026-11-20T04:00:05+01:00:   verified 1023.00/2048.00 MiB in 4.00 seconds, speed 255.75/512.00 MiB/s (0 errors)
2026-11-20T04:00:05+01:00: verify store1:vm/101/2026-11-19T01:00:00Z
2026-11-20T04:00:09+01:00:   verified 10.00/20.00 MiB in 1.00 seconds, speed 10.00/20.00 MiB/s (2 errors)
2026-11-20T04:00:09+01:00: Failed to verify the following snapshots/groups:
2026-11-20T04:00:09+01:00: 	vm/101/2026-11-19T01:00:00Z
2026-11-20T04:00:09+01:00: TASK ERROR: verification failed - please check the log for details
`
	journalElague = `2026-11-20T05:00:00+01:00: Starting datastore prune on datastore 'store1', root namespace
2026-11-20T05:00:00+01:00: Pruning group :"vm/100"
2026-11-20T05:00:00+01:00: remove vm/100/2026-11-01T01:00:00Z
2026-11-20T05:00:00+01:00: remove vm/100/2026-11-02T01:00:00Z
2026-11-20T05:00:00+01:00: keep vm/100/2026-11-19T01:00:00Z
2026-11-20T05:00:00+01:00: TASK OK
`
)

func TestJournauxPBS(t *testing.T) {
	gc := analyserJournalPBS([]byte(journalGC), "garbage_collection")
	if len(gc.Resume) != 5 || gc.Resume[0] != (InfoTache{"Place libérée", "12.345 GiB"}) || gc.Resume[4].Valeur != "13.53" || len(gc.Erreurs) != 0 {
		t.Errorf("nettoyage : %+v", gc)
	}
	v := analyserJournalPBS([]byte(journalVerif), "verificationjob")
	if len(v.Resume) != 2 || v.Resume[0].Valeur != "2" || v.Resume[1].Valeur != "2" || len(v.Erreurs) != 2 ||
		!strings.Contains(v.Erreurs[0], "vm/101/2026-11-19") || !strings.HasPrefix(v.Erreurs[1], "verification failed") {
		t.Errorf("vérification : %+v", v)
	}
	e := analyserJournalPBS([]byte(journalElague), "prunejob")
	if len(e.Resume) != 2 || e.Resume[0].Valeur != "2" || e.Resume[1].Valeur != "1" {
		t.Errorf("élagage : %+v", e)
	}
	if d := analyserJournalPBS([]byte("n'importe quoi\n"), "syncjob"); len(d.Resume)+len(d.Erreurs) != 0 {
		t.Errorf("journal inconnu : %+v", d)
	}
}

// Bout en bout : index PBS, journal retrouvé dans le sous-dossier « pstart & 255 ».
func TestCompleterPBS(t *testing.T) {
	ancien := dossierTachesPBS
	t.Cleanup(func() { dossierTachesPBS = ancien })
	dossierTachesPBS = t.TempDir()
	upid := "UPID:pbs:000004D2:0001E2AB:00000003:673D3A42:garbage_collection:store1:root@pam:"
	_ = os.MkdirAll(filepath.Join(dossierTachesPBS, "AB"), 0o755)
	_ = os.WriteFile(filepath.Join(dossierTachesPBS, "AB", upid), []byte(journalGC), 0o644)
	taches := analyserTaches([]byte(upid+" 673D3B6E OK\n"), "pbs")
	if len(taches) != 1 || taches[0].Nom != "garbage_collection store1" {
		t.Fatalf("index : %+v", taches)
	}
	completerPBS(taches)
	if d := taches[0].Detail; d == nil || len(d.Resume) != 5 {
		t.Fatalf("détail : %+v", d)
	}
	// Autre sous-dossier : retrouvé quand même.
	_ = os.Rename(filepath.Join(dossierTachesPBS, "AB"), filepath.Join(dossierTachesPBS, "07"))
	if cheminJournalPBS(upid) == "" {
		t.Error("journal non retrouvé")
	}
	if cheminJournalPBS("UPID:../../etc/passwd") != "" {
		t.Error("chemin hors du dossier accepté")
	}
}

// VM oubliées : tout sauf exclues, liste, pool, travail d'un autre nœud,
// travail désactivé ; pool inconnu ou aucun travail : rien à dire.
func TestSansSauvegarde(t *testing.T) {
	cfg := `vzdump: nuit
	schedule 02:00
	all 1
	exclude 104,105
	storage pbs

vzdump: autre-noeud
	schedule 03:00
	vmid 104
	node pve2

vzdump: prod
	schedule sat 04:00
	pool prod

vzdump: arrete
	vmid 105
	enabled 0
`
	travaux := lireTravaux([]byte(cfg))
	if len(travaux) != 4 || !travaux[0].Tout || strings.Join(travaux[0].Exclues, ",") != "104,105" || travaux[1].Noeud != "pve2" || travaux[2].Pool != "prod" {
		t.Fatalf("travaux : %+v", travaux)
	}
	pools := lirePools([]byte("user:root@pam:1:0::::::\npool:prod:Production:105,106::\n"))
	if strings.Join(pools["prod"], ",") != "105,106" {
		t.Fatalf("pools : %+v", pools)
	}
	inv := []Invite{{ID: "100"}, {ID: "104"}, {ID: "105"}, {ID: "107"}}
	l, ok := sansSauvegarde(inv, "pve1", travaux, pools)
	if !ok || strings.Join(l, ",") != "104" {
		t.Errorf("pve1 : %v %v", l, ok)
	}
	// Sur pve2, le travail « autre-noeud » couvre la 104.
	if l, ok := sansSauvegarde(inv, "pve2", travaux, pools); !ok || len(l) != 0 {
		t.Errorf("pve2 : %v %v", l, ok)
	}
	if _, ok := sansSauvegarde(inv, "pve1", travaux, map[string][]string{}); ok {
		t.Error("pool inconnu : rien ne doit être signalé")
	}
	if _, ok := sansSauvegarde(inv, "pve1", travaux[3:], pools); ok {
		t.Error("aucun travail actif : rien ne doit être signalé")
	}
}

// Relecture du 21/11 : au-delà de 60 tâches, le nettoyage (et les autres
// tâches hors sauvegarde d'un groupe) reste visible.
func TestTachesAuDelaDuMax(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 70; i++ {
		fmt.Fprintf(&b, "UPID:pbs:000004D2:0001E2AB:%08X:673D3A42:backup:store1\\x3avm-%d:root@pam: 673D3B6E OK\n", i, 100+i)
	}
	b.WriteString("UPID:pbs:000004D2:0001E2AB:00000099:673D3A40:garbage_collection:store1:root@pam: 673D3B6E erreur\n")
	taches := analyserTaches([]byte(b.String()), "pbs")
	trouve := false
	for _, x := range taches {
		trouve = trouve || strings.HasPrefix(x.Nom, "garbage_collection")
	}
	if len(taches) != maxTaches || !trouve {
		t.Fatalf("%d tâches, nettoyage présent : %v", len(taches), trouve)
	}
}
