// SPDX-License-Identifier: GPL-3.0-or-later

package notif

import (
	"fmt"
	"html"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"mirador-stack/internal/server/alertes"
)

// Types de message.
const (
	TypeAlerte     = "alerte"     // nouvelles alertes (et résolutions du même groupe)
	TypeRappel     = "rappel"     // toujours en cours, non acquittées
	TypeResolution = "resolution" // seulement des fins
)

// Message : ce qui part pour un groupe d'alertes.
type Message struct {
	Le          time.Time
	Groupe      string // clé de regroupement (alertname, instance…)
	Gravite     string // la plus haute du groupe
	Type        string
	Actives     []alertes.Alerte // en cours (nouvelles d'abord)
	Nouvelles   int              // les Nouvelles premières de Actives sont nouvelles
	Resolues    []alertes.Alerte
	Dependantes int  // alertes d'équipements dépendants tues à cause de ce groupe
	Son         bool // notification avec son
	// Remplis par l'envoi : canal de secours utilisé, canal en panne.
	Secours, CanalEnPanne string
	// Systeme : message de mirador lui-même (canal en panne, test…), sans alerte.
	Systeme string
	// EnFileDepuis : message gardé en file d'attente (tous les canaux en panne).
	EnFileDepuis time.Time
	// Escalade : alertes pas prises en charge à temps : envoyé aussi sur le
	// canal de secours (SPEC § 30).
	Escalade bool
	// Site : message d'un site injoignable (toutes ses machines muettes en
	// même temps, SPEC § 39) : mis en forme à part, une ligne par machine.
	Site string
	// Extraits : dernières lignes de logs par alerte (empreinte des
	// étiquettes), remplies juste avant l'envoi (SPEC § 41).
	Extraits map[uint64][]string
	// Changements : « 12 min avant : 14 paquets mis à jour » par alerte
	// (SPEC § 47), rempli juste avant l'envoi.
	Changements map[uint64]string
}

// Nb : nombre d'alertes dans le message.
func (m *Message) Nb() int { return len(m.Actives) + len(m.Resolues) }

// Mise en forme, reprise du modèle Telegram actuel (modèle Alertmanager d'origine,
// SPEC § 4.7) avec les ajouts mirador : valeur, durée, lien direct, canal de
// secours.

func pastille(g string) string {
	switch g {
	case alertes.Critique:
		return "🔴"
	case alertes.Avertissement:
		return "🟠"
	}
	return "🔵"
}

// dureeCourte : « 2h 5m », « 3m 10s », « 4j 2h ».
func dureeCourte(d time.Duration) string {
	d = d.Round(time.Second)
	switch {
	case d >= 24*time.Hour:
		return fmt.Sprintf("%dj %dh", d/(24*time.Hour), d%(24*time.Hour)/time.Hour)
	case d >= time.Hour:
		return fmt.Sprintf("%dh %dm", d/time.Hour, d%time.Hour/time.Minute)
	case d >= time.Minute:
		return fmt.Sprintf("%dm %ds", d/time.Minute, d%time.Minute/time.Second)
	}
	return fmt.Sprintf("%ds", d/time.Second)
}

func valeur(v float64) string {
	return strconv.FormatFloat(v, 'g', 4, 64)
}

// Mise : options de mise en forme.
type Mise struct {
	HTML   bool           // HTML de Telegram (sinon texte brut)
	Fuseau *time.Location // heures affichées
	Lien   string         // adresse de l'interface ("" = pas de lien)
	Max    int            // longueur maximale (0 = sans limite) ; au-delà : coupé proprement
	// Modèles des messages (SPEC § 82) et canal qui envoie ; TitreAPart :
	// le canal montre le titre à part (ntfy, e-mail, téléphone, Gotify).
	Modeles    map[string]Modele
	Canal      string
	TitreAPart bool
	// BlocCode : extraits de logs dans un bloc de code Markdown (« ``` » :
	// Discord, Slack, Mattermost) ; rien de ce qu'ils contiennent n'est
	// alors lu comme mise en forme, lien ou mention (revue du 28/11, M5).
	BlocCode bool
}

// extraitOctets : taille maximale des extraits de logs d'une alerte, en
// octets, comptée APRÈS échappement (revue du 28/11, B6 : en HTML, un
// guillemet devient « &#34; », 5 octets ; des logs pleins de guillemets
// dépassaient la limite de Telegram à eux seuls).
const extraitOctets = 1200

// blocLogs : extraits de logs mis en forme et bornés (les lignes les plus
// récentes sont gardées ; une ligne trop longue est raccourcie).
//   - HTML (Telegram, Matrix) : bloc <pre>, texte échappé ;
//   - BlocCode : bloc ``` ; les accents graves des logs sont remplacés par
//     « ˋ » (ils ne peuvent plus fermer le bloc) ;
//   - sinon : lignes en retrait, texte brut.
func blocLogs(lignes []string, o Mise) string {
	prep := func(l string) string {
		switch {
		case o.HTML:
			return html.EscapeString(l)
		case o.BlocCode:
			return strings.ReplaceAll(l, "`", "ˋ")
		}
		return "  " + l
	}
	var garde []string
	reste := extraitOctets
	for i := len(lignes) - 1; i >= 0; i-- {
		p := prep(lignes[i])
		if len(p)+1 > reste {
			if len(garde) > 0 {
				break // plus de place : les lignes plus anciennes sont laissées
			}
			// La plus récente ne tient pas seule : raccourcie (160
			// caractères au plus, la boucle est courte).
			r := []rune(lignes[i])
			for len(r) > 0 && len(p)+1 > reste {
				r = r[:len(r)-1]
				p = prep(string(r) + "…")
			}
			if len(r) == 0 {
				break
			}
		}
		garde = append([]string{p}, garde...)
		reste -= len(p) + 1
	}
	if len(garde) == 0 {
		return ""
	}
	corps := strings.Join(garde, "\n")
	switch {
	case o.HTML:
		return "<pre>" + corps + "</pre>\n"
	case o.BlocCode:
		return "```\n" + corps + "\n```\n"
	}
	return corps + "\n"
}

// Texte met un message en forme.
func Texte(m *Message, o Mise) string {
	if o.Fuseau == nil {
		o.Fuseau = time.UTC
	}
	e := func(s string) string {
		if o.HTML {
			return html.EscapeString(s)
		}
		return s
	}
	gras := func(s string) string {
		if o.HTML {
			return "<b>" + e(s) + "</b>"
		}
		return s
	}
	ital := func(s string) string {
		if o.HTML {
			return "<i>" + e(s) + "</i>"
		}
		return s
	}
	code := func(s string) string {
		if o.HTML {
			return "<code>" + e(s) + "</code>"
		}
		return s
	}
	var b strings.Builder
	if !m.EnFileDepuis.IsZero() {
		fmt.Fprintf(&b, "⏳ %s\n\n", e("En attente depuis le "+m.EnFileDepuis.In(o.Fuseau).Format("02/01 15:04")+
			" (tous les canaux étaient en panne)"))
	}
	if m.Systeme != "" {
		b.WriteString(e(m.Systeme))
		return b.String()
	}
	if m.CanalEnPanne != "" {
		fmt.Fprintf(&b, "⚠️ %s (canal %s en panne)\n\n", e("Envoyé par le canal de secours "+m.Secours), e(m.CanalEnPanne))
	}
	if txt, ok := texteModele(m, o); ok { // modèle de l'administrateur (SPEC § 82)
		b.WriteString(txt)
		return finir(b.String(), o)
	}
	switch {
	case m.Escalade:
		b.WriteString("⏫ " + gras("Escalade") + " : pas prise en charge à temps\n\n")
	case m.Type == TypeRappel:
		b.WriteString("🔁 " + gras("Rappel") + " : toujours en cours\n\n")
	}
	if m.Site != "" {
		texteSite(&b, m, o, e, gras, code)
		return finir(b.String(), o)
	}
	for _, a := range m.Actives {
		depuis := a.ActiveLe
		if depuis.IsZero() {
			depuis = a.Debut
		}
		fmt.Fprintf(&b, "%s %s\n", pastille(a.Gravite), gras(alertes.Libelle(alertes.Libelle(a.Regle))))
		if a.Resume != "" {
			b.WriteString(e(a.Resume) + "\n")
		}
		if a.Description != "" {
			b.WriteString(ital(a.Description) + "\n")
		}
		if !a.AbsenteDepuis.IsZero() { // SPEC § 46
			fmt.Fprintf(&b, "%s\n", e("📡 Données absentes depuis "+a.AbsenteDepuis.In(o.Fuseau).Format("02/01 15:04")+
				" ("+dureeCourte(m.Le.Sub(a.AbsenteDepuis))+") : la machine n'envoie plus rien"))
		}
		if a.AvecProcedure && a.ID > 0 && o.Lien != "" {
			b.WriteString(e("📝 Procédure : voir l'alerte") + "\n")
		}
		if ch := m.Changements[a.Etiquettes.Hash()]; ch != "" {
			b.WriteString(e("🔧 "+ch) + "\n")
		}
		if lignes := m.Extraits[a.Etiquettes.Hash()]; len(lignes) > 0 {
			b.WriteString(e("📄 Derniers logs :") + "\n")
			b.WriteString(blocLogs(lignes, o))
		}
		machine := a.Machine()
		if machine == "" {
			machine = "-"
		}
		fmt.Fprintf(&b, "%s · depuis %s (%s) · valeur %s\n", code(machine), depuis.In(o.Fuseau).Format("02/01 15:04"),
			dureeCourte(m.Le.Sub(depuis)), e(valeur(a.Valeur)))
		if a.AcquitteePar != "" {
			fmt.Fprintf(&b, "✔ %s\n", e("acquittée par "+a.AcquitteePar))
		}
		if o.Lien != "" && a.ID > 0 {
			fmt.Fprintf(&b, "%s/alertes/%d\n", e(o.Lien), a.ID)
		}
		b.WriteString("\n")
	}
	for _, a := range m.Resolues {
		fmt.Fprintf(&b, "✅ %s : %s\n", gras("Résolu"), e(alertes.Libelle(a.Regle)))
		if a.Resume != "" {
			b.WriteString(e(a.Resume) + "\n")
		}
		machine := a.Machine()
		if machine == "" {
			machine = "-"
		}
		duree := ""
		if !a.ActiveLe.IsZero() {
			duree = " (durée " + dureeCourte(a.Fin.Sub(a.ActiveLe)) + ")"
		}
		fmt.Fprintf(&b, "%s · fin %s%s\n\n", code(machine), a.Fin.In(o.Fuseau).Format("02/01 15:04"), duree)
	}
	if m.Dependantes > 0 {
		fmt.Fprintf(&b, "%s\n", e(fmt.Sprintf("(+ %d alerte(s) d'équipements dépendants en sourdine)", m.Dependantes)))
	}
	return finir(b.String(), o)
}

// finir : retire les lignes vides de la fin et coupe si trop long.
func finir(s string, o Mise) string {
	s = strings.TrimRight(s, "\n")
	if o.Max > 0 && len(s) > o.Max {
		s = couperTexte(s, o.Max, o.HTML, o.BlocCode)
	}
	return s
}

// texteSite : message d'un site injoignable (SPEC § 39). Une ligne par
// machine plutôt qu'un bloc par alerte : le message reste court, même pour
// un site de 20 machines.
func texteSite(b *strings.Builder, m *Message, o Mise, e, gras, code func(string) string) {
	heure := func(t time.Time) string { return t.In(o.Fuseau).Format("02/01 15:04") }
	machines := func(as []alertes.Alerte) string {
		noms := make([]string, 0, len(as))
		vu := map[string]bool{}
		for _, a := range as {
			if n := a.Machine(); n != "" && !vu[n] {
				vu[n] = true
				noms = append(noms, n)
			}
		}
		sort.Strings(noms)
		for i := range noms {
			noms[i] = code(noms[i])
		}
		return strings.Join(noms, ", ")
	}
	lien := func() {
		if o.Lien != "" {
			fmt.Fprintf(b, "%s\n", e(o.Lien+"/coupures?site="+url.QueryEscape(m.Site)))
		}
	}
	if len(m.Actives) == 0 { // toutes les machines sont revenues
		debut, fin := m.Resolues[0].ActiveLe, m.Resolues[0].Fin
		for _, a := range m.Resolues {
			if d := debutActif(&a); d.Before(debut) || debut.IsZero() {
				debut = d
			}
			if a.Fin.After(fin) {
				fin = a.Fin
			}
		}
		fmt.Fprintf(b, "✅ %s\n", gras("Site "+m.Site+" de nouveau joignable"))
		fmt.Fprintf(b, "%s\n", e(fmt.Sprintf("Coupure de %s (%s → %s), %d machine(s) revenue(s) : ",
			dureeCourte(fin.Sub(debut)), heure(debut), heure(fin), len(m.Resolues)))+machines(m.Resolues))
		b.WriteString(e("Les agents renvoient ce qu'ils ont gardé en mémoire (jusqu'à ~19 h) : le bilan est sur la page des coupures.") + "\n")
		lien()
		return
	}
	depuis := debutActif(&m.Actives[0])
	for _, a := range m.Actives {
		if d := debutActif(&a); d.Before(depuis) {
			depuis = d
		}
	}
	fmt.Fprintf(b, "%s %s\n", pastille(m.Gravite), gras("Site "+m.Site+" injoignable"))
	fmt.Fprintf(b, "%s\n", e(fmt.Sprintf("Toutes ses machines se sont tues en même temps : coupure d'internet ou de courant sur le site ? "+
		"Depuis %s (%s).", heure(depuis), dureeCourte(m.Le.Sub(depuis)))))
	fmt.Fprintf(b, "%s %s\n", e("Muettes :"), machines(m.Actives))
	if len(m.Resolues) > 0 {
		fmt.Fprintf(b, "%s %s\n", e("Revenues :"), machines(m.Resolues))
	}
	b.WriteString(e("Un seul message pour le site ; un autre au retour.") + "\n")
	lien()
}

// couper : garde le début d'un texte brut, sur une fin de ligne, avec une
// mention.
func couper(s string, max int) string { return couperTexte(s, max, false, false) }

// couperTexte : comme couper. En HTML, jamais au milieu d'une balise ni
// d'un bloc ouvert (<pre>, <b>…) : on coupe avant (revue du 28/11, B6 :
// Telegram refusait le message coupé dans un <pre>, et l'alerte était
// perdue). En Markdown (blocCode), jamais au milieu d'un bloc ```.
func couperTexte(s string, max int, enHTML, blocCode bool) string {
	const suite = "\n… (message coupé, voir l'interface)"
	limite := max - len(suite)
	if limite < 0 {
		limite = 0
	}
	coupe := s[:limite]
	if i := strings.LastIndex(coupe, "\n\n"); i > 0 {
		coupe = coupe[:i]
	} else if i := strings.LastIndex(coupe, "\n"); i > 0 {
		coupe = coupe[:i]
	}
	if enHTML {
		coupe = avantBaliseOuverte(coupe)
	}
	if blocCode && strings.Count(coupe, "```")%2 == 1 {
		coupe = coupe[:strings.LastIndex(coupe, "```")]
	}
	return strings.ToValidUTF8(strings.TrimRight(coupe, "\n"), "") + suite
}

// avantBaliseOuverte : texte HTML raccourci pour qu'aucune balise n'y reste
// ouverte (coupé avant la première balise non fermée) ni coupée en deux
// (« <pr », « &#3 »). Dans le HTML de Texte, tout « < » et tout « & »
// venus du texte sont échappés : chaque « < » est une vraie balise.
func avantBaliseOuverte(s string) string {
	if i := strings.LastIndexByte(s, '<'); i >= 0 && !strings.Contains(s[i:], ">") {
		s = s[:i]
	}
	if i := strings.LastIndexByte(s, '&'); i >= 0 && !strings.Contains(s[i:], ";") {
		s = s[:i]
	}
	type ouverte struct {
		nom string
		pos int
	}
	var pile []ouverte
	for i := 0; ; {
		j := strings.IndexByte(s[i:], '<')
		if j < 0 {
			break
		}
		j += i
		k := strings.IndexByte(s[j:], '>')
		if k < 0 {
			break
		}
		k += j
		balise := s[j+1 : k]
		if nom, ferme := strings.CutPrefix(balise, "/"); ferme {
			for n := len(pile) - 1; n >= 0; n-- {
				if pile[n].nom == nom {
					pile = pile[:n]
					break
				}
			}
		} else if !strings.HasSuffix(balise, "/") {
			nom, _, _ := strings.Cut(balise, " ")
			pile = append(pile, ouverte{nom, j})
		}
		i = k + 1
	}
	if len(pile) > 0 {
		s = s[:pile[0].pos]
	}
	return s
}
