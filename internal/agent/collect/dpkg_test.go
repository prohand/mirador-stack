// SPDX-License-Identifier: GPL-3.0-or-later

package collect

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestJournalDpkg(t *testing.T) {
	chemin := filepath.Join(t.TempDir(), "dpkg.log")
	maintenant := time.Date(2026, 11, 18, 12, 0, 0, 0, time.Local)
	ecrire := func(txt string) {
		f, err := os.OpenFile(chemin, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = f.WriteString(txt)
		_ = f.Close()
	}
	ecrire("2026-11-10 03:00:00 upgrade vieux:amd64 1 2\n" + // plus de 48 h : ignoré
		"2026-11-18 03:12:40 upgrade openssl:amd64 3.0.15-1 3.0.15-2\n" +
		"2026-11-18 03:12:41 status installed openssl:amd64 3.0.15-2\n" +
		"2026-11-18 03:12:42 install jq:amd64 <none> 1.7.1-3\n" +
		"ligne illisible\n")
	j := journalDpkg{chemin: chemin}
	if j.lire(maintenant) {
		t.Error("premier passage : rien de « nouveau » (état initial)")
	}
	if len(j.liste) != 2 || j.liste[0].nom != "openssl" || j.liste[0].avant != "3.0.15-1" || j.liste[0].apres != "3.0.15-2" ||
		j.liste[1].nom != "jq" || j.liste[1].avant != "" {
		t.Fatalf("liste : %+v", j.liste)
	}
	if j.lire(maintenant) {
		t.Error("rien de changé")
	}
	time.Sleep(10 * time.Millisecond)
	ecrire("2026-11-18 11:59:00 upgrade curl:amd64 8.5.0-1 8.5.0-2\n")
	if !j.lire(maintenant) || len(j.liste) != 3 || j.liste[2].nom != "curl" {
		t.Fatalf("ajout : %+v", j.liste)
	}
	// Rotation : fichier plus court, relu depuis le début.
	if err := os.WriteFile(chemin, []byte("2026-11-18 11:59:30 upgrade git:amd64 1 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !j.lire(maintenant) || j.liste[len(j.liste)-1].nom != "git" {
		t.Fatalf("rotation : %+v", j.liste)
	}
	// 48 h plus tard : tout est oublié.
	ecrire("x\n")
	j.lire(maintenant.Add(72 * time.Hour))
	if len(j.liste) != 0 {
		t.Fatalf("ménage : %+v", j.liste)
	}
}

// Relecture du 21/11 : sans nouvelle installation (fichier inchangé), les
// paquets de plus de 48 h ne sont plus envoyés.
func TestJournalDpkgMenageSansChangement(t *testing.T) {
	chemin := filepath.Join(t.TempDir(), "dpkg.log")
	maintenant := time.Date(2026, 11, 18, 12, 0, 0, 0, time.Local)
	if err := os.WriteFile(chemin, []byte("2026-11-18 03:12:40 upgrade openssl:amd64 1 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	j := journalDpkg{chemin: chemin}
	j.lire(maintenant)
	if len(j.liste) != 1 {
		t.Fatalf("liste : %+v", j.liste)
	}
	j.lire(maintenant.Add(72 * time.Hour))
	if len(j.liste) != 0 {
		t.Errorf("paquet de plus de 48 h encore envoyé : %+v", j.liste)
	}
}
