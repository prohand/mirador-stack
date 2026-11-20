// SPDX-License-Identifier: GPL-3.0-or-later

package notif

import (
	"strings"
	"testing"
	"time"
)

// Modèles des messages (SPEC § 82) : rendu, canal avant « tous », titre à
// part ou en première ligne, échappement Telegram, repli si erreur.
func TestModeles(t *testing.T) {
	t0 := time.Date(2026, 11, 20, 8, 26, 0, 0, time.UTC)
	m := MessageExemple(ModeleAlerte, t0)
	mods := map[string]Modele{
		ModeleAlerte: {Titre: "{{.Icone}} mirador : {{.Nb}} alerte(s)",
			Corps: "{{range .Alertes}}{{.Icone}} {{.Titre}} · {{.Machine}} <{{.Valeur}}>{{if .Lien}} {{.Lien}}{{end}}\n{{end}}"},
		CleModele(ModeleAlerte, NomNtfy): {Corps: "ntfy : {{.Nb}}"},
	}
	o := Mise{Fuseau: time.UTC, Lien: "https://m.fr", Modeles: mods, Canal: NomTelegram, HTML: true}
	txt := Texte(m, o)
	if !strings.HasPrefix(txt, "<b>🔴 mirador : 2 alerte(s)</b>\n\n🔴 Disque presque plein · nas &lt;96&gt; https://m.fr/alertes/42") {
		t.Errorf("Telegram :\n%s", txt)
	}
	// Canal propre d'abord ; titre à part : pas répété.
	o2 := Mise{Fuseau: time.UTC, Modeles: mods, Canal: NomNtfy, TitreAPart: true}
	if got := Texte(m, o2); got != "ntfy : 2" {
		t.Errorf("ntfy : %q", got)
	}
	if got := TitreModele(m, Mise{Modeles: mods, Canal: NomEmail}); got != "🔴 mirador : 2 alerte(s)" {
		t.Errorf("titre : %q", got)
	}
	// Message système : jamais de modèle.
	if got := Texte(&Message{Systeme: "🧪 test"}, o); got != "🧪 test" {
		t.Errorf("système : %q", got)
	}
	// Modèle en erreur à l'exécution : mise en forme de mirador.
	o.Modeles = map[string]Modele{ModeleAlerte: {Corps: "{{.Inconnue}}"}}
	if got := Texte(m, o); !strings.Contains(got, "<b>Disque presque plein</b>") {
		t.Errorf("repli :\n%s", got)
	}
	// Résolution, site : types reconnus ; modèles de départ valides.
	for _, typ := range TypesModele {
		if err := ModeleDepart(typ[0]).Valider(); err != nil {
			t.Errorf("modèle de départ %s : %v", typ[0], err)
		}
		mx := MessageExemple(typ[0], t0)
		if got := typeModele(mx); got != typ[0] {
			t.Errorf("type de %s : %s", typ[0], got)
		}
		txt := Texte(mx, Mise{Fuseau: time.UTC, Modeles: map[string]Modele{typ[0]: ModeleDepart(typ[0])}})
		if strings.Contains(txt, "<no value>") || txt == "" {
			t.Errorf("%s :\n%s", typ[0], txt)
		}
	}
	// Validation : syntaxe, appels interdits, tailles, clés.
	for _, mauvais := range []Modele{
		{Corps: "{{range .Alertes}}"},
		{Corps: `{{define "x"}}{{template "x"}}{{end}}{{template "x"}}`},
		{Corps: "{{.Champ}}"},
		{Corps: strings.Repeat("a", 5000)},
		{Corps: ""},
	} {
		if err := mauvais.Valider(); err == nil {
			t.Errorf("modèle accepté : %.40q", mauvais.Corps)
		}
	}
	if err := validerModeles(map[string]Modele{"inconnu": {Corps: "x"}}); err == nil {
		t.Error("type inconnu accepté")
	}
	if err := validerModeles(map[string]Modele{"alerte:pigeon": {Corps: "x"}}); err == nil {
		t.Error("canal inconnu accepté")
	}
	// Sortie bornée.
	gros := Modele{Corps: "{{range .Alertes}}" + strings.Repeat("x", 3900) + "{{end}}"}
	if _, err := executer("m", strings.Repeat(gros.Corps, 1), DonneesModele{Alertes: make([]AlerteModele, 20)}); err == nil {
		t.Error("sortie non bornée")
	}
}
