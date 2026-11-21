// SPDX-License-Identifier: GPL-3.0-or-later

package syslog

import (
	"bufio"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestAnalyser(t *testing.T) {
	cas := []struct {
		brut, niveau, app, texte string
		heure                    bool
	}{
		// RFC 5424 (Synology, rsyslog).
		{`<165>1 2026-11-14T10:00:00.003Z nas.lan evntslog - ID47 [exemple@32473 iut="3" eventSource="App"] Connexion de admin`,
			"notice", "evntslog", "Connexion de admin", true},
		{`<14>1 - - - - - - sans rien`, "info", "", "sans rien", false},
		{`<11>1 2026-11-14T10:00:00Z h app 1 - [a b="c\]d"][e f="g"] après`, "err", "app", "après", true},
		// RFC 3164 (OPNsense, UniFi).
		{`<134>Nov 14 10:00:00 opnsense filterlog[123]: 5,,,0,igb0,match,block`, "info", "filterlog", "5,,,0,igb0,match,block", true},
		{`<30>Nov  4 08:01:02 sshd[42]: Accepted publickey`, "info", "sshd", "Accepted publickey", true},
		{`<13>Nov 14 10:00:00 nas kernel: oops`, "notice", "kernel", "oops", true},
		{`<12>hostapd: wlan0: STA associated`, "warning", "hostapd", "wlan0: STA associated", false},
		{`<0>message brut sans tag`, "emerg", "", "message brut sans tag", false},
	}
	for _, c := range cas {
		m, ok := Analyser([]byte(c.brut))
		if !ok || m.Niveau != c.niveau || m.App != c.app || m.Texte != c.texte || m.Heure.IsZero() == c.heure {
			t.Errorf("%q\n → %+v (ok=%v)", c.brut, m, ok)
		}
	}
	for _, mauvais := range []string{"", "pas de PRI", "<192>trop grand", "<-1>négatif", "<>vide", "<1234567>long"} {
		if _, ok := Analyser([]byte(mauvais)); ok {
			t.Errorf("accepté : %q", mauvais)
		}
	}
	// UTF-8 invalide remplacé.
	if m, _ := Analyser([]byte("<14>app: \xff\xfe")); !utf8.ValidString(m.Texte) {
		t.Error("UTF-8 invalide gardé")
	}
}

func TestLireTrame(t *testing.T) {
	flux := "12 <14>app: un\n<14>app: deux\r\n" + "9 <14>trois" + "\n<14>" + strings.Repeat("x", TailleMaxMessage+100) + "\n<14>fin"
	r := bufio.NewReaderSize(strings.NewReader(flux), 4096)
	var vus []string
	for {
		b, err := LireTrame(r)
		if err != nil {
			break
		}
		vus = append(vus, string(b))
	}
	if len(vus) != 5 || vus[0] != "<14>app: un" || vus[1] != "<14>app: deux" || vus[2] != "<14>trois" || len(vus[3]) > TailleMaxMessage || vus[4] != "<14>fin" {
		t.Fatalf("trames : %d %q", len(vus), vus[:min(3, len(vus))])
	}
	if !strings.HasPrefix(vus[3], "<14>xxx") {
		t.Errorf("ligne trop longue : le début doit être gardé, pas %.20q", vus[3])
	}
	// Ligne trop longue avec une fin qui ressemble à un message (relecture
	// du 21/11) : la fin est jetée, jamais lue comme un message à part.
	long := "<14>web: GET /" + strings.Repeat("a", 3*TailleMaxMessage) + "<10>sshd: Accepted password for root\n<14>suite\n"
	r = bufio.NewReaderSize(strings.NewReader(long), 16<<10)
	a, _ := LireTrame(r)
	b, _ := LireTrame(r)
	if !strings.HasPrefix(string(a), "<14>web: GET /") || len(a) != TailleMaxMessage || string(b) != "<14>suite" {
		t.Errorf("ligne longue : %.30q (%d), puis %q", a, len(a), b)
	}
	for _, mauvais := range []string{"99999 x", "123456 x", "12x <14>"} {
		if _, err := LireTrame(bufio.NewReader(strings.NewReader(mauvais))); err == nil {
			t.Errorf("trame acceptée : %q", mauvais)
		}
	}
}

// FuzzSyslog : ni panique ni texte invalide, quel que soit le flux reçu.
func FuzzSyslog(f *testing.F) {
	for _, s := range []string{"<165>1 2026-11-14T10:00:00Z h a p m [x y=\"z\"] m", "<134>Nov 14 10:00:00 h t[1]: m", "12 <14>app: un\n", "<0>"} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		r := bufio.NewReaderSize(strings.NewReader(string(b)), 4096)
		for i := 0; i < 100; i++ {
			trame, err := LireTrame(r)
			if err != nil {
				return
			}
			if len(trame) > TailleMaxMessage {
				t.Fatal("trame trop longue")
			}
			if m, ok := Analyser(trame); ok && (!utf8.ValidString(m.Texte) || !utf8.ValidString(m.App) || len(m.App) > 48) {
				t.Fatalf("message invalide : %+v", m)
			}
		}
	})
}
