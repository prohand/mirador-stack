// SPDX-License-Identifier: GPL-3.0-or-later

package majagent

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Un reste de téléchargement de plus d'une heure est retiré (revue du 27/11).
func TestOublierTelechargements(t *testing.T) {
	d := t.TempDir()
	vieux, recent := filepath.Join(d, ".telechargement-1"), filepath.Join(d, ".telechargement-2")
	_ = os.WriteFile(vieux, []byte("x"), 0o600)
	_ = os.WriteFile(recent, []byte("x"), 0o600)
	_ = os.Chtimes(vieux, time.Now().Add(-2*time.Hour), time.Now().Add(-2*time.Hour))
	(&Installateur{Dossier: d}).oublierTelechargements()
	if _, err := os.Stat(vieux); err == nil {
		t.Error("vieux reste de téléchargement gardé")
	}
	if _, err := os.Stat(recent); err != nil {
		t.Error("téléchargement récent (peut-être en cours) retiré")
	}
}
