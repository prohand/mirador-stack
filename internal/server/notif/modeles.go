// SPDX-License-Identifier: GPL-3.0-or-later

package notif

import (
	"bytes"
	"errors"
	"fmt"
	"html"
	"strings"
	"text/template"
	"text/template/parse"
	"time"

	"github.com/prometheus/prometheus/model/labels"

	"mirador-stack/internal/server/alertes"
)

// Modèles des messages (SPEC § 82, idée reprise de nyx-stack, idée
// seulement) : titre et texte écrits par l'administrateur, par type de
// message et, au besoin, par canal. Sans modèle, ou si le modèle échoue,
// le message part avec la mise en forme de mirador (jamais d'alerte
// perdue).
//
// Les modèles sont du texte simple (syntaxe des modèles de Go : {{.Nb}},
// {{range .Alertes}}…{{end}}). Pour Telegram (HTML), le résultat est
// échappé : pas de balise, les émojis passent tels quels.

// Types de modèle.
const (
	ModeleAlerte     = "alerte"
	ModeleResolution = "resolution"
	ModeleRappel     = "rappel"
	ModeleEscalade   = "escalade"
	ModeleSite       = "site"
)

// TypesModele : types proposés dans l'interface (code, nom).
var TypesModele = [][2]string{
	{ModeleAlerte, "Alerte"}, {ModeleResolution, "Résolution"}, {ModeleRappel, "Rappel"},
	{ModeleEscalade, "Escalade"}, {ModeleSite, "Site injoignable"},
}

// Modele : titre (ntfy, e-mail, téléphone, Gotify ; première ligne ailleurs)
// et texte.
type Modele struct {
	Titre string `json:"titre"`
	Corps string `json:"corps"`
}

// Bornes.
const (
	maxTitreModele  = 300
	maxCorpsModele  = 4000
	maxSortieModele = 32 << 10
	maxModeles      = 40
)

// CleModele : « alerte » (tous les canaux) ou « alerte:telegram ».
func CleModele(typ, canal string) string {
	if canal == "" {
		return typ
	}
	return typ + ":" + canal
}

// typeModele : type de modèle d'un message ("" : jamais de modèle, ex.
// message système ou de test).
func typeModele(m *Message) string {
	switch {
	case m.Systeme != "":
		return ""
	case m.Site != "":
		return ModeleSite
	case m.Escalade:
		return ModeleEscalade
	case m.Type == TypeRappel:
		return ModeleRappel
	case m.Type == TypeResolution:
		return ModeleResolution
	case m.Type == TypeAlerte:
		return ModeleAlerte
	}
	return ""
}

// AlerteModele : variables d'une alerte dans un modèle.
type AlerteModele struct {
	Icone       string // 🔴 critique, 🟠 avertissement, 🔵 info, ✅ résolue
	Titre       string // libellé lisible : « Disque presque plein »
	Regle       string // nom technique : « DisquePresquePlein »
	Machine     string
	Gravite     string
	Resume      string
	Description string
	Valeur      string
	Depuis      string // « 20/11 08:12 »
	Duree       string // « 14m 3s »
	Fin         string // résolue : « 20/11 08:40 »
	Lien        string // page de l'alerte (adresse de l'interface réglée)
	Logs        string // dernières lignes de logs (SPEC § 41)
	Changement  string // « 12 min avant : … » (SPEC § 47)
	Acquittee   string // acquittée par…
	Resolue     bool
}

// DonneesModele : variables d'un message.
type DonneesModele struct {
	Nb          int
	Icone       string // selon la plus grave
	Type        string // alerte, resolution, rappel, escalade, site
	Gravite     string
	Alertes     []AlerteModele // en cours puis résolues
	Actives     []AlerteModele
	Resolues    []AlerteModele
	Site        string
	Dependantes int
	Lien        string // adresse de l'interface
	Canal       string
}

// donneesModele : variables d'un message pour un modèle.
func donneesModele(m *Message, o Mise) DonneesModele {
	loc := o.Fuseau
	if loc == nil {
		loc = time.UTC
	}
	d := DonneesModele{Nb: m.Nb(), Icone: pastille(m.Gravite), Type: typeModele(m), Gravite: m.Gravite, Site: m.Site,
		Dependantes: m.Dependantes, Lien: o.Lien, Canal: o.Canal}
	if len(m.Actives) == 0 && len(m.Resolues) > 0 {
		d.Icone = "✅"
	}
	une := func(a alertes.Alerte, resolue bool) AlerteModele {
		x := AlerteModele{Icone: pastille(a.Gravite), Titre: alertes.Libelle(a.Regle), Regle: a.Regle, Machine: a.Machine(),
			Gravite: a.Gravite, Resume: a.Resume, Description: a.Description, Valeur: valeur(a.Valeur),
			Acquittee: a.AcquitteePar, Resolue: resolue, Changement: m.Changements[a.Etiquettes.Hash()],
			Logs: strings.Join(m.Extraits[a.Etiquettes.Hash()], "\n")}
		if d := debutActif(&a); !d.IsZero() {
			x.Depuis = d.In(loc).Format("02/01 15:04")
			fin := m.Le
			if resolue && !a.Fin.IsZero() {
				fin = a.Fin
			}
			x.Duree = dureeCourte(fin.Sub(d))
		}
		if resolue {
			x.Icone = "✅"
			if !a.Fin.IsZero() {
				x.Fin = a.Fin.In(loc).Format("02/01 15:04")
			}
		}
		if o.Lien != "" && a.ID > 0 {
			x.Lien = fmt.Sprintf("%s/alertes/%d", o.Lien, a.ID)
		}
		return x
	}
	for _, a := range m.Actives {
		d.Actives = append(d.Actives, une(a, false))
	}
	for _, a := range m.Resolues {
		d.Resolues = append(d.Resolues, une(a, true))
	}
	d.Alertes = append(append([]AlerteModele{}, d.Actives...), d.Resolues...)
	return d
}

// lireModele : analyse d'un texte de modèle. Interdits : define, template,
// block (pas de modèle qui s'appelle lui-même : le temps de rendu reste
// borné par la taille des données).
func lireModele(nom, texte string) (*template.Template, error) {
	t, err := template.New(nom).Option("missingkey=error").Parse(texte)
	if err != nil {
		return nil, errors.New(strings.TrimPrefix(err.Error(), "template: "))
	}
	if len(t.Templates()) > 1 {
		return nil, errors.New("define, template et block ne sont pas permis")
	}
	if t.Tree != nil && contientAppel(t.Tree.Root) {
		return nil, errors.New("define, template et block ne sont pas permis")
	}
	return t, nil
}

func contientAppel(n parse.Node) bool {
	switch x := n.(type) {
	case *parse.TemplateNode:
		return true
	case *parse.ListNode:
		if x == nil {
			return false
		}
		for _, e := range x.Nodes {
			if contientAppel(e) {
				return true
			}
		}
	case *parse.IfNode:
		return contientAppel(x.List) || contientAppel(x.ElseList)
	case *parse.RangeNode:
		return contientAppel(x.List) || contientAppel(x.ElseList)
	case *parse.WithNode:
		return contientAppel(x.List) || contientAppel(x.ElseList)
	}
	return false
}

// sortieBornee : écriture refusée au-delà de maxSortieModele.
type sortieBornee struct{ b bytes.Buffer }

func (s *sortieBornee) Write(p []byte) (int, error) {
	if s.b.Len()+len(p) > maxSortieModele {
		return 0, errors.New("message trop long")
	}
	return s.b.Write(p)
}

func executer(nom, texte string, d DonneesModele) (string, error) {
	t, err := lireModele(nom, texte)
	if err != nil {
		return "", err
	}
	var s sortieBornee
	if err := t.Execute(&s, d); err != nil {
		return "", errors.New(strings.TrimPrefix(err.Error(), "template: "))
	}
	return strings.TrimSpace(s.b.String()), nil
}

// Valider : modèle lisible, tailles bornées, rendu d'essai sans erreur.
func (md Modele) Valider() error {
	if strings.TrimSpace(md.Corps) == "" {
		return errors.New("modèle : texte du message vide")
	}
	if len(md.Titre) > maxTitreModele || len(md.Corps) > maxCorpsModele {
		return fmt.Errorf("modèle : titre %d caractères au plus, texte %d", maxTitreModele, maxCorpsModele)
	}
	ex := donneesModele(MessageExemple(ModeleAlerte, time.Now()), Mise{Fuseau: time.UTC, Lien: "https://mirador.exemple.fr"})
	if _, err := executer("titre", md.Titre, ex); err != nil {
		return fmt.Errorf("titre : %w", err)
	}
	if _, err := executer("message", md.Corps, ex); err != nil {
		return fmt.Errorf("message : %w", err)
	}
	return nil
}

// validerModeles : clés connues, nombre borné, chaque modèle valide.
func validerModeles(ms map[string]Modele) error {
	if len(ms) > maxModeles {
		return fmt.Errorf("modèles : %d au plus", maxModeles)
	}
	var errs []error
	for cle, md := range ms {
		typ, canal, _ := strings.Cut(cle, ":")
		connu := false
		for _, t := range TypesModele {
			connu = connu || t[0] == typ
		}
		if !connu || canal != "" && !containsCanal(canal) {
			errs = append(errs, fmt.Errorf("modèle %q : type ou canal inconnu", cle))
			continue
		}
		if err := md.Valider(); err != nil {
			errs = append(errs, fmt.Errorf("modèle %s : %w", cle, err))
		}
	}
	return errors.Join(errs...)
}

func containsCanal(c string) bool {
	for _, n := range nomsCanaux() {
		if n == c {
			return true
		}
	}
	return false
}

// modeleDe : modèle à utiliser (canal d'abord, puis tous les canaux).
func modeleDe(m *Message, o Mise) (Modele, bool) {
	typ := typeModele(m)
	if typ == "" || len(o.Modeles) == 0 {
		return Modele{}, false
	}
	if md, ok := o.Modeles[CleModele(typ, o.Canal)]; ok && o.Canal != "" {
		return md, true
	}
	md, ok := o.Modeles[typ]
	return md, ok
}

// rendreModele : titre et texte d'un message avec son modèle ; ok faux :
// pas de modèle, ou modèle en erreur (le message garde la mise en forme
// de mirador).
func rendreModele(m *Message, o Mise) (titre, corps string, ok bool) {
	md, ok := modeleDe(m, o)
	if !ok {
		return "", "", false
	}
	d := donneesModele(m, o)
	corps, err := executer("message", md.Corps, d)
	if err != nil || corps == "" {
		return "", "", false
	}
	if md.Titre != "" {
		if titre, err = executer("titre", md.Titre, d); err != nil {
			return "", "", false
		}
		titre = strings.Join(strings.Fields(titre), " ") // une ligne
	}
	return titre, corps, true
}

// texteModele : texte final d'un message avec modèle (titre en première
// ligne sauf si le canal le montre à part ; échappé pour Telegram).
func texteModele(m *Message, o Mise) (string, bool) {
	titre, corps, ok := rendreModele(m, o)
	if !ok {
		return "", false
	}
	if titre != "" && !o.TitreAPart {
		corps = titre + "\n\n" + corps
	}
	if o.HTML {
		corps = html.EscapeString(corps)
		if titre != "" && !o.TitreAPart {
			// Titre en gras : seule balise ajoutée, autour d'un texte échappé.
			et := html.EscapeString(titre)
			corps = "<b>" + et + "</b>" + strings.TrimPrefix(corps, et)
		}
	}
	return corps, true
}

// TitreModele : titre d'un message avec modèle ("" : pas de modèle ou pas
// de titre ; le canal garde alors le sien).
func TitreModele(m *Message, o Mise) string {
	titre, _, ok := rendreModele(m, o)
	if !ok {
		return ""
	}
	return titre
}

// ModeleDepart : texte proposé quand on écrit un modèle (proche de la mise
// en forme de mirador).
func ModeleDepart(typ string) Modele {
	ligne := "{{.Icone}} {{.Titre}}{{if .Machine}} · {{.Machine}}{{end}}\n{{if .Resume}}{{.Resume}}\n{{end}}"
	switch typ {
	case ModeleResolution:
		return Modele{Titre: "✅ mirador : {{.Nb}} résolue(s)",
			Corps: "{{range .Resolues}}✅ {{.Titre}}{{if .Machine}} · {{.Machine}}{{end}}\nfin {{.Fin}} (durée {{.Duree}})\n\n{{end}}"}
	case ModeleRappel:
		return Modele{Titre: "🔁 mirador : toujours en cours",
			Corps: "{{range .Actives}}" + ligne + "depuis {{.Depuis}} ({{.Duree}}){{if .Lien}} · {{.Lien}}{{end}}\n\n{{end}}"}
	case ModeleEscalade:
		return Modele{Titre: "⏫ mirador : pas prise en charge à temps",
			Corps: "{{range .Actives}}" + ligne + "depuis {{.Depuis}} ({{.Duree}}){{if .Lien}} · {{.Lien}}{{end}}\n\n{{end}}"}
	case ModeleSite:
		return Modele{Titre: "{{.Icone}} Site {{.Site}}{{if .Actives}} injoignable{{else}} de nouveau joignable{{end}}",
			Corps: "{{if .Actives}}Machines muettes : {{range $i, $a := .Actives}}{{if $i}}, {{end}}{{$a.Machine}}{{end}}" +
				"{{else}}Toutes les machines sont revenues.{{end}}{{if .Lien}}\n{{.Lien}}/coupures?site={{.Site}}{{end}}"}
	}
	return Modele{Titre: "{{.Icone}} mirador : {{.Nb}} alerte(s)",
		Corps: "{{range .Alertes}}" + ligne + "{{if .Resolue}}fin {{.Fin}}{{else}}depuis {{.Depuis}} ({{.Duree}}){{end}}{{if .Lien}} · {{.Lien}}{{end}}\n\n{{end}}"}
}

// MessageExemple : message d'exemple d'un type (aperçu, essai, validation).
func MessageExemple(typ string, t time.Time) *Message {
	a := func(id int64, regle, gravite, machine, resume string, depuis time.Duration) alertes.Alerte {
		return alertes.Alerte{ID: id, Regle: regle, Gravite: gravite, Etat: alertes.Active, Resume: resume, Valeur: 96,
			Etiquettes: labelsExemple(regle, gravite, machine), Debut: t.Add(-depuis), ActiveLe: t.Add(-depuis)}
	}
	disque := a(42, "DisquePresquePlein", alertes.Critique, "nas", "/volume1 : 96 % utilisés", 14*time.Minute)
	agent := a(41, "MiseAJourAgentEchouee", alertes.Avertissement, "pve1", "La mise à jour de l'agent a échoué sur pve1", 31*time.Minute)
	m := &Message{Le: t, Groupe: "exemple", Gravite: alertes.Critique, Type: TypeAlerte, Actives: []alertes.Alerte{disque, agent}, Nouvelles: 2}
	switch typ {
	case ModeleResolution:
		disque.Fin, disque.Etat = t, alertes.Resolue
		m.Type, m.Actives, m.Resolues, m.Gravite = TypeResolution, nil, []alertes.Alerte{disque}, alertes.Critique
	case ModeleRappel:
		m.Type, m.Nouvelles = TypeRappel, 0
	case ModeleEscalade:
		m.Escalade = true
	case ModeleSite:
		m.Site = "bureau"
		m.Actives = []alertes.Alerte{a(43, "AgentMuet", alertes.Critique, "pc-accueil", "pc-accueil ne remonte plus", 6*time.Minute),
			a(44, "AgentMuet", alertes.Critique, "nas-bureau", "nas-bureau ne remonte plus", 6*time.Minute)}
	}
	return m
}

func labelsExemple(regle, gravite, machine string) labels.Labels {
	return labels.FromStrings("alertname", regle, "severity", gravite, "instance", machine)
}
