// SPDX-License-Identifier: GPL-3.0-or-later

package alertes

import (
	"bytes"
	"strings"
	"testing"
)

// FuzzImporter : un fichier de règles quelconque (YAML mirador ou vmalert)
// ne doit jamais faire planter l'import, et toute règle acceptée doit être
// valide.
func FuzzImporter(f *testing.F) {
	f.Add([]byte("groups:\n  - name: g\n    rules:\n      - alert: X\n        expr: up == 0\n        for: 5m\n"))
	f.Add([]byte("version: 1\nregles:\n  - nom: X\n    expr: up == 0\n    gravite: warning\n"))
	f.Add([]byte("groups: [{rules: [{alert: A, expr: 'lag(x[1d]) > 60', annotations: {summary: '{{ $value | humanize }}'}}]}]"))
	f.Fuzz(func(t *testing.T, b []byte) {
		rap, err := Importer(bytes.NewReader(b), "f.yml")
		if err != nil {
			return
		}
		for i := range rap.Regles {
			if _, err := rap.Regles[i].Valider(); err != nil {
				t.Fatalf("règle acceptée mais invalide : %v", err)
			}
		}
	})
}

// FuzzExpr : l'analyse d'une expression ne plante jamais.
func FuzzExpr(f *testing.F) {
	for _, e := range []string{`up == 0`, `rate(x[5m]) > 1 and y`, `lag(a{b=~"c.*"}[1d]) > 60`, `x @ 1`, `{__name__=~"a|b"}`} {
		f.Add(e)
	}
	f.Fuzz(func(_ *testing.T, e string) {
		if x, err := AnalyserExpr(e); err == nil {
			_ = fenetreMax(x)
			_ = metriquesLues(x)
			_ = strings.TrimSpace(x.String())
		}
	})
}
