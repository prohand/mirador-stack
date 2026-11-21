// SPDX-License-Identifier: GPL-3.0-or-later

package racine

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Relecture du 21/11 : lectures root sans lien, tube ni périphérique, et
// chemin réel contrôlé sur le fichier ouvert.
func TestOuvrirSur(t *testing.T) {
	d := t.TempDir()
	bon := filepath.Join(d, "taches")
	if err := os.WriteFile(bon, []byte("ok"), 0o600); err != nil {
		t.Fatal(err)
	}
	if f, err := ouvrirSur(bon, false, nil); err != nil {
		t.Fatalf("fichier ordinaire refusé : %v", err)
	} else {
		f.Close()
	}
	// Lien final : refusé sans « suivre ».
	lien := filepath.Join(d, "lien")
	_ = os.Symlink(bon, lien)
	if _, err := ouvrirSur(lien, false, nil); err == nil {
		t.Error("lien suivi alors qu'interdit")
	}
	// Tube : refusé, sans bloquer.
	tube := filepath.Join(d, "tube")
	if err := syscall.Mkfifo(tube, 0o600); err != nil {
		t.Fatal(err)
	}
	fini := make(chan error, 1)
	go func() { _, err := ouvrirSur(tube, true, nil); fini <- err }()
	select {
	case err := <-fini:
		if err == nil {
			t.Error("tube accepté")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ouverture d'un tube bloquée")
	}
	// Périphérique (via un lien) : refusé.
	dev := filepath.Join(d, "dev.pem")
	_ = os.Symlink("/dev/null", dev)
	if _, err := ouvrirSur(dev, true, nil); err == nil {
		t.Error("périphérique accepté")
	}
	// Chemin réel contrôlé sur le fichier ouvert : un lien vers un
	// dossier non autorisé est refusé.
	autre := t.TempDir()
	secret := filepath.Join(autre, "secret.pem")
	_ = os.WriteFile(secret, []byte("x"), 0o600)
	vers := filepath.Join(d, "x.pem")
	_ = os.Symlink(secret, vers)
	if _, err := ouvrirSur(vers, true, func(reel string) bool { return strings.HasPrefix(reel, d+"/") }); err == nil {
		t.Error("lien vers un dossier non autorisé lu")
	}
}
