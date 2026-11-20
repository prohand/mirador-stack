// SPDX-License-Identifier: GPL-3.0-or-later

package racine

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Journal d'une sauvegarde vzdump (forme réelle, raccourcie) : deux VM vers
// PBS, une en échec, un conteneur vers un stockage local.
const journalVzdump = `INFO: starting new backup job: vzdump --all 1 --storage pbs-maison --mode snapshot --notes-template '{{guestname}}'
INFO: Starting Backup of VM 101 (qemu)
INFO: Backup started at 2026-11-20 02:00:02
INFO: status = running
INFO: VM Name: web
INFO: include disk 'scsi0' 'local-lvm:vm-101-disk-0' 32G
INFO: backup was done incrementally, reused 28.10 GiB (87%)
INFO: transferred 4.10 GiB in 180 seconds (23.3 MiB/s)
INFO: Finished Backup of VM 101 (00:03:01)
INFO: Backup finished at 2026-11-20 02:03:03
INFO: Starting Backup of VM 104 (qemu)
INFO: VM Name: nextcloud
ERROR: Backup of VM 104 failed - unable to create temporary directory '/var/tmp/vzdumptmp1234' - Permission denied
INFO: Failed at 2026-11-20 02:03:05
INFO: Starting Backup of VM 200 (lxc)
INFO: CT Name: ha
INFO: archive file size: 2.80GB
INFO: Finished Backup of VM 200 (00:02:10)
INFO: Backup job finished with errors
TASK ERROR: job errors
`

const travauxCfg = `vzdump: backup-1a2b3c4d-5e6f
	comment Sauvegardes de nuit
	schedule 02:00
	all 1
	enabled 1
	mode snapshot
	storage pbs-maison

vzdump: backup-ancien
	schedule sat 03:00
	enabled 0
	storage local

realm-sync: sync-ad
	schedule daily
`

func TestJournalSauvegarde(t *testing.T) {
	d := analyserJournalSauvegarde([]byte(journalVzdump))
	if d.Stockage != "pbs-maison" || len(d.VMs) != 3 {
		t.Fatalf("détail : %+v", d)
	}
	v := d.VMs
	if v[0].ID != "101" || v[0].Nom != "web" || !v[0].OK || v[0].Duree != 181 || v[0].Octets < 4.09*(1<<30) || v[0].Octets > 4.11*(1<<30) {
		t.Errorf("VM 101 : %+v", v[0])
	}
	if v[1].ID != "104" || v[1].OK || v[1].Nom != "nextcloud" || v[1].Message == "" {
		t.Errorf("VM 104 : %+v", v[1])
	}
	if v[2].ID != "200" || v[2].Type != "lxc" || v[2].Nom != "ha" || !v[2].OK || v[2].Octets < 2.79*(1<<30) {
		t.Errorf("CT 200 : %+v", v[2])
	}
	if len(d.Erreurs) != 2 || d.Erreurs[0][:37] != "Backup of VM 104 failed - unable to c" || d.Erreurs[1] != "job errors" {
		t.Errorf("erreurs : %q", d.Erreurs)
	}
	tr := lireTravaux([]byte(travauxCfg))
	if len(tr) != 2 || tr[0].Planning != "02:00" || tr[0].Commentaire != "Sauvegardes de nuit" || !tr[0].Actif || tr[1].Actif {
		t.Fatalf("travaux : %+v", tr)
	}
}

func TestProchainePVE(t *testing.T) {
	paris, _ := time.LoadLocation("Europe/Paris")
	maintenant := time.Date(2026, 11, 20, 10, 0, 0, 0, paris) // vendredi
	for planning, attendu := range map[string]string{
		"02:00":          "2026-11-21 02:00",
		"daily":          "2026-11-21 00:00",
		"sat 03:00":      "2026-11-21 03:00",
		"mon..fri 21:0":  "2026-11-20 21:00",
		"mon,wed 01:30":  "2026-11-23 01:30",
		"sat..mon 23:00": "2026-11-21 23:00",
		"*/2:00":         "",
		"lundi 02:00":    "",
	} {
		got := prochainePVE(planning, maintenant)
		texte := ""
		if !got.IsZero() {
			texte = got.Format("2006-01-02 15:04")
		}
		if texte != attendu {
			t.Errorf("%q : %q, attendu %q", planning, texte, attendu)
		}
	}
}

// Bout en bout : index des tâches, journal retrouvé par l'UPID, travail planifié.
func TestCompleterSauvegardes(t *testing.T) {
	dir := t.TempDir()
	ancienD, ancienT := dossierTachesPVE, travauxPVE
	t.Cleanup(func() { dossierTachesPVE, travauxPVE = ancienD, ancienT })
	dossierTachesPVE, travauxPVE = filepath.Join(dir, "tasks"), filepath.Join(dir, "jobs.cfg")
	upid := "UPID:pve1:0000AB12:0123ABCD:673D3A42:vzdump::root@pam:"
	_ = os.MkdirAll(filepath.Join(dossierTachesPVE, "2"), 0o755)
	_ = os.WriteFile(filepath.Join(dossierTachesPVE, "2", upid), []byte(journalVzdump), 0o644)
	_ = os.WriteFile(travauxPVE, []byte(travauxCfg), 0o644)
	taches := analyserTaches([]byte(upid+" 673D3B6E job errors\n"), "pve")
	if len(taches) != 1 || taches[0].Nom != "vzdump" {
		t.Fatalf("index : %+v", taches)
	}
	completerSauvegardes(taches, time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC))
	d := taches[0].Detail
	if d == nil || len(d.VMs) != 3 || d.Planning != "02:00" || d.Commentaire != "Sauvegardes de nuit" || d.Stockage != "pbs-maison" ||
		!d.Prochaine.Equal(time.Date(2026, 11, 21, 2, 0, 0, 0, time.UTC)) {
		t.Fatalf("détail : %+v", d)
	}
	// UPID piégé : pas de chemin.
	if cheminJournal("UPID:../../etc/passwd") != "" {
		t.Error("chemin hors du dossier accepté")
	}
}
