// SPDX-License-Identifier: GPL-3.0-or-later

package racine

import (
	"os"
	"path/filepath"
	"testing"
)

// Journaux de tâches Proxmox VE et Proxmox Backup Server (SPEC § 80).
func TestTachesProxmox(t *testing.T) {
	d := t.TempDir()
	pve := "UPID:pve1:0000A1B2:0012C3D4:6560F2A0:vzdump::root@pam: 6560F5B3 OK\n" +
		"UPID:pve1:0000A1B3:0012C3D5:6560F300:qmstart:101:root@pam: 6560F301 OK\n" + // pas une sauvegarde
		"UPID:pve1:0000A1B4:0012C3D6:65614420:vzdump::root@pam: 65614433 job errors\n" + // plus récente : en échec
		"UPID:pve1:0000A1B5:0012C3D7:65614500:vzdump:105:root@pam: 65614510 WARNINGS: 1\n" +
		"ligne illisible\n"
	pbs := "UPID:pbs:000003E8:00001234:00000005:6560F2A0:garbage_collection:store1:root@pam: 6560F5B3 OK\n" +
		"UPID:pbs:000003E9:00001235:00000006:6560F400:backup:store1\\x3avm/100:root@pam!pve: 6560F420 OK\n"
	indexPVE, archivePBS = filepath.Join(d, "index"), filepath.Join(d, "archive")
	if err := os.WriteFile(indexPVE, []byte(pve), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(archivePBS, []byte(pbs), 0o600); err != nil {
		t.Fatal(err)
	}
	par := map[string]TacheProxmox{}
	for _, x := range lireTachesProxmox() {
		par[x.Source+"|"+x.Nom] = x
	}
	if len(par) != 4 {
		t.Fatalf("tâches : %+v", par)
	}
	if v := par["pve|vzdump"]; v.OK || v.Message != "job errors" || v.Debut.Unix() != 0x65614420 || v.Fin.Unix() != 0x65614433 {
		t.Errorf("vzdump (dernière en échec) : %+v", v)
	}
	if v := par["pve|vzdump 105"]; !v.OK {
		t.Errorf("avertissements : réussie attendue : %+v", v)
	}
	if v := par["pbs|backup store1:vm/100"]; !v.OK || v.Fin.Unix() != 0x6560F420 {
		t.Errorf("sauvegarde reçue par PBS : %+v", v)
	}
	if _, ok := par["pbs|garbage_collection store1"]; !ok {
		t.Error("ramasse-miettes PBS absent")
	}
	// Hors Proxmox : fichiers absents, rien.
	indexPVE, archivePBS = filepath.Join(d, "absent1"), filepath.Join(d, "absent2")
	if n := len(lireTachesProxmox()); n != 0 {
		t.Fatalf("hors Proxmox : %d tâches", n)
	}
}
