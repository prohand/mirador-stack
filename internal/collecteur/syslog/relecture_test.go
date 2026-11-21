// SPDX-License-Identifier: GPL-3.0-or-later

package syslog

import (
	"bufio"
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// Relecture du 21/11 : horodatage ISO dans un message « BSD » (rsyslog
// RSYSLOG_ForwardFormat, syslog-ng ts-format(iso)…) : programme, heure et
// texte bien lus.
func TestISO3164(t *testing.T) {
	for _, c := range []struct{ ligne, app, texte string }{
		{"<13>2026-11-21T10:00:00.123456+01:00 nas sshd[42]: Accepted password for x", "sshd", "Accepted password for x"},
		{"<13>2026-11-21T10:00:00+01:00 nas kernel: oops", "kernel", "oops"},
	} {
		m, ok := Analyser([]byte(c.ligne))
		if !ok || m.App != c.app || m.Texte != c.texte || m.Heure.IsZero() || m.Heure.Hour() != 9 && m.Heure.UTC().Hour() != 9 {
			t.Errorf("%q : ok=%v App=%q Texte=%q Heure=%v", c.ligne, ok, m.App, m.Texte, m.Heure)
		}
	}
}

// Relecture du 21/11 : messages terminés par NUL (RFC 6587) lus un par un.
func TestFinNUL(t *testing.T) {
	var flux []byte
	for i := 0; i < 1000; i++ {
		flux = append(flux, "<13>app: message\x00"...)
	}
	flux = append(flux, "<13>app: dernier\n<13>app: sans fin"...)
	r := bufio.NewReaderSize(bytes.NewReader(flux), tailleTamponLecture)
	n := 0
	var dernier []byte
	for {
		tr, err := LireTrame(r)
		if err != nil {
			break
		}
		n++
		dernier = tr
	}
	if n != 1002 || string(dernier) != "<13>app: sans fin" {
		t.Errorf("%d message(s) lus, dernier %q", n, dernier)
	}
}

// Relecture du 21/11 : fichier de clé vide (coupure de courant pendant la
// première écriture) : une clé neuve est créée au lieu de couper le syslog
// pour toujours.
func TestCleVideRecreee(t *testing.T) {
	chemin := filepath.Join(t.TempDir(), "syslog.key")
	if err := os.WriteFile(chemin, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	cle, err := ChargerOuCreerCle(chemin)
	if err != nil || cle == nil {
		t.Fatalf("clé vide : %v", err)
	}
	if relue, err := ChargerOuCreerCle(chemin); err != nil || !relue.Equal(cle) {
		t.Errorf("clé non gardée : %v", err)
	}
}
