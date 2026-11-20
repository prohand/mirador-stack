// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Commandes : aide, version, réglages manquants ou invalides refusés.
func TestLancer(t *testing.T) {
	var s, e bytes.Buffer
	if lancer(nil, &s, &e) != 0 || !strings.Contains(s.String(), "appliquer") {
		t.Error("aide")
	}
	if lancer([]string{"-version"}, &s, &e) != 0 {
		t.Error("version")
	}
	if lancer([]string{"appliquer", "-config", filepath.Join(t.TempDir(), "absent")}, &s, &e) == 0 {
		t.Error("réglages absents acceptés")
	}
	conf := filepath.Join(t.TempDir(), "conf")
	_ = os.WriteFile(conf, []byte("DOSSIER_COMPOSE=relatif\n"), 0o600)
	if lancer([]string{"verifier", "-config", conf}, &s, &e) == 0 {
		t.Error("réglages invalides acceptés")
	}
}
