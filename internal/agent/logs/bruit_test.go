// SPDX-License-Identifier: GPL-3.0-or-later

package logs

import (
	"math/rand"
	"regexp"
	"strings"
	"testing"
)

// bruitDefaut : copie de bruitDefaut (import impossible : cycle).
// TestBruitDefautCopie (paquet config) vérifie qu'elles restent égales.
const bruitDefaut = `(?i)(session (opened|closed) for user|pam_unix\(cron|Started Session|Starting Session|Removed slice|Created slice)`

// La voie rapide donne toujours le même résultat que l'expression.
func TestBruitVoieRapideIdentique(t *testing.T) {
	motifs := []string{
		bruitDefaut,
		`(?i)[Ss]ession`, `[Ss]ession`, `Session`, `(?i)foo|bar\.baz`, `(?i)a+b`, `^(?i)cron`,
		`(?i)(x|y)(z|w)`, `(?i)ok|`,
	}
	morceaux := []string{"session opened for user", "SESSION Opened FOR user", "pam_unix(cron:session)", "Started Session 12",
		"starting session", "Removed slice", "CREATED SLICE user.slice", "ſession opened for user", "Kelvin K", "foo", "BAR.BAZ",
		"barXbaz", "aaab", "cron", "xz", "YW", "nginx: GET / 200", "é", " ", "ok", "Session", "session"}
	r := rand.New(rand.NewSource(1))
	for _, m := range motifs {
		f, re := nouveauFiltreBruit(m), regexp.MustCompile(m)
		for i := 0; i < 20000; i++ {
			var b strings.Builder
			for j := r.Intn(4); j >= 0; j-- {
				p := morceaux[r.Intn(len(morceaux))]
				if r.Intn(3) == 0 {
					p = strings.ToUpper(p)
				}
				b.WriteString(p)
				if r.Intn(2) == 0 {
					b.WriteByte(' ')
				}
			}
			l := b.String()
			if got, want := f.Correspond(l), re.MatchString(l); got != want {
				t.Fatalf("motif %q, ligne %q : %v, attendu %v", m, l, got, want)
			}
		}
	}
	if f := nouveauFiltreBruit(bruitDefaut); f.textes == nil {
		t.Error("motif par défaut : voie rapide attendue")
	}
	for _, m := range []string{`[Ss]ession`, `Session`, `(?i)a+b`, `^(?i)cron`, `(?i)ok|`} {
		if f := nouveauFiltreBruit(m); f.textes != nil {
			t.Errorf("motif %q : pas de voie rapide attendue (%q)", m, f.textes)
		}
	}
}

func BenchmarkBruitDefaut(b *testing.B) {
	l := `192.168.1.20 - - [27/Nov/2026:10:00:00 +0100] "GET /index.html HTTP/1.1" 200 512 "-" "Mozilla/5.0"`
	f, re := nouveauFiltreBruit(bruitDefaut), regexp.MustCompile(bruitDefaut)
	b.Run("expression", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			re.MatchString(l)
		}
	})
	b.Run("rapide", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			f.Correspond(l)
		}
	})
}
