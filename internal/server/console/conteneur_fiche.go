// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"context"
	"math"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"mirador-stack/internal/server/stockage/journaux"
)

// Fiche d'un conteneur (SPEC § 123, maquette validée le 23/11) : pastilles
// (image, projet, machine, politique, démarré, ports, application),
// bandeau « pourquoi » (sortie du test de santé ou code de sortie, plus les
// dernières erreurs de ses logs), 4 cases, graphiques comme ceux d'une
// machine (tableau « conteneur », même choix de période), conteneurs du
// même projet.

type pastilleCt struct{ Avant, Texte, Lien string }

type ligneLogCt struct{ Heure, Niveau, Classe, Msg string }

type ficheConteneur struct {
	L          ligneConteneur
	Pastilles  []pastilleCt
	Pourquoi   string // titre du bandeau ("" : pas de bandeau)
	Detail     string
	Logs       []ligneLogCt // dernières erreurs, dans l'ordre
	Cases      []caseConteneurs
	MemeProjet []ligneConteneur
	Rangees    []vueRangee
	Periode    string
	Periodes   []string
}

// lireFicheConteneur : nil si le conteneur n'est pas (ou plus) dans les métriques.
func (c *Console) lireFicheConteneur(ctx context.Context, machine, nom, periode string, t time.Time) *ficheConteneur {
	var f *ficheConteneur
	var autres []ligneConteneur
	for _, l := range c.conteneursDuParc(ctx, t) {
		if l.Machine != machine {
			continue
		}
		if l.Nom == nom {
			f = &ficheConteneur{L: l}
		} else {
			autres = append(autres, l)
		}
	}
	if f == nil {
		return nil
	}
	un := []ligneConteneur{f.L}
	c.completerConteneurs(ctx, un, t)
	f.L = un[0]
	l := f.L
	if l.Projet != "" {
		for _, a := range autres {
			if a.Projet == l.Projet {
				f.MemeProjet = append(f.MemeProjet, a)
			}
		}
	}
	// Pastilles.
	if l.Image != "" {
		f.Pastilles = append(f.Pastilles, pastilleCt{Texte: l.Image})
	}
	if l.Projet != "" {
		f.Pastilles = append(f.Pastilles, pastilleCt{Avant: "projet", Texte: l.Projet, Lien: "/conteneurs?machine=" + url.QueryEscape(l.Machine)})
	}
	f.Pastilles = append(f.Pastilles, pastilleCt{Avant: "sur", Texte: l.Machine, Lien: "/machines/" + url.PathEscape(l.Machine)})
	if l.Politique != "" {
		f.Pastilles = append(f.Pastilles, pastilleCt{Avant: "redémarrage :", Texte: l.Politique})
	}
	if l.Depuis != "" {
		f.Pastilles = append(f.Pastilles, pastilleCt{Avant: "démarré", Texte: "il y a " + l.Depuis})
	}
	for _, p := range l.Ports {
		f.Pastilles = append(f.Pastilles, pastilleCt{Avant: "port", Texte: p})
	}
	if l.Application != "" {
		f.Pastilles = append(f.Pastilles, pastilleCt{Avant: "application", Texte: l.Application, Lien: "/applications/" + url.PathEscape(l.Application)})
	}
	if l.ImageChangee != "" {
		f.Pastilles = append(f.Pastilles, pastilleCt{Texte: l.ImageChangee})
	}
	// Bandeau « pourquoi ».
	switch {
	case l.ClasseSante == "erreur":
		f.Pourquoi = "Test de santé en échec"
		if l.ProblemeDepuis != "" {
			f.Pourquoi += " (" + l.ProblemeDepuis + ")"
		}
		if l.SortieSante != "" {
			f.Detail = "Dernière sortie du test : « " + l.SortieSante + " »"
		}
	case l.ArreteATort():
		f.Pourquoi = "Arrêté alors qu'il doit redémarrer"
		if l.ProblemeDepuis != "" {
			f.Pourquoi += " (" + l.ProblemeDepuis + ")"
		}
		f.Detail = "politique « " + l.Politique + " »"
		if l.CodeSortie != "" {
			f.Detail = texteCodeSortie(l.CodeSortie) + " · " + f.Detail
		}
	case l.OOM:
		f.Pourquoi = "Tué faute de mémoire (OOM)"
		if l.Limite != "" {
			f.Detail = "limite du conteneur : " + l.Limite
		}
	}
	if f.Pourquoi != "" {
		f.Logs = c.dernieresErreursConteneur(ctx, machine, nom, t)
	}
	f.Cases = c.casesFicheConteneur(ctx, l, t)
	// Graphiques : tableau « conteneur », comme ceux d'une machine.
	if tb, _ := c.tableauOuCatalogue("conteneur"); tb != nil {
		v := url.Values{"periode": {periode}, "instance": {machine}, "name": {nom}}
		if vals, p, duree, err := lireFiltres(tb, v); err == nil {
			f.Periode, f.Periodes = p, periodesProposees
			f.Rangees = c.evaluerTableau(ctx, tb, vals, p, duree, t, nil)
		}
	}
	return f
}

// casesFicheConteneur : Santé (ou État), Processeur, Mémoire, Redémarrages.
func (c *Console) casesFicheConteneur(ctx context.Context, l ligneConteneur, t time.Time) []caseConteneurs {
	sel := `{instance="` + echapperPromQL(l.Machine) + `", name="` + echapperPromQL(l.Nom) + `"}`
	valeur := func(expr string) (float64, bool) {
		ech, err := c.instant(ctx, expr, t)
		if err != nil || len(ech) == 0 || math.IsNaN(ech[0].Valeur) {
			return 0, false
		}
		return ech[0].Valeur, true
	}
	var out []caseConteneurs
	switch {
	case !l.EnMarche:
		k := caseConteneurs{Titre: "État", Valeur: l.Etat, Texte: "arrêté"}
		if l.CodeSortie != "" {
			k.Texte = texteCodeSortie(l.CodeSortie)
		}
		if l.ArreteATort() {
			k.Classe = "erreur"
		}
		out = append(out, k)
	case l.ClasseSante == "erreur":
		k := caseConteneurs{Titre: "Santé", Valeur: "Non sain", Classe: "erreur", Texte: "test de santé en échec"}
		if l.ProblemeDepuis != "" {
			k.Texte += " · " + l.ProblemeDepuis
		}
		out = append(out, k)
	case l.Sante == "Sain":
		out = append(out, caseConteneurs{Titre: "Santé", Valeur: "Sain", Texte: "test de santé réussi"})
	default:
		out = append(out, caseConteneurs{Titre: "Santé", Valeur: "—", Texte: "pas de test : seul l'état est suivi"})
	}
	cpu := caseConteneurs{Titre: "Processeur", Valeur: l.CPU, Texte: "d'un cœur"}
	if v, ok := valeur(`max(max_over_time((100 * rate(node_docker_container_cpu_seconds_total` + sel + `[5m]))[24h:5m]))`); ok {
		cpu.Texte += " · max 24 h : " + virgule(formatPct(v))
	}
	mem := caseConteneurs{Titre: "Mémoire", Valeur: l.Memoire, Texte: "pas de limite"}
	if !l.EnMarche {
		cpu.Texte, mem.Texte = "arrêté", "arrêté"
	}
	out = append(out, cpu)
	if l.Limite != "" && l.PctMemoire >= 0 {
		mem.Texte = strconv.Itoa(l.PctMemoire) + " % de la limite (" + l.Limite + ")"
		switch {
		case l.PctMemoire >= 90:
			mem.Classe = "erreur"
		case l.PctMemoire >= 75:
			mem.Classe = "attention"
		}
	}
	out = append(out, mem)
	redem := caseConteneurs{Titre: "Redémarrages", Valeur: strconv.Itoa(l.Redemarrages), Texte: "24 h"}
	if v, ok := valeur(`max(round(increase(node_docker_container_restart_count` + sel + `[7d])))`); ok {
		redem.Texte += " · " + strconv.FormatFloat(v, 'f', 0, 64) + " sur 7 j"
	}
	if l.OOM {
		redem.Texte += " · tué faute de mémoire"
		redem.Classe = "erreur"
	}
	return append(out, redem)
}

// reErreurLog : mots qui trahissent une erreur dans une ligne sans niveau.
var reErreurLog = regexp.MustCompile(`(?i)\b(error|err|fatal|panic|exception|critical|warn|warning)\b`)

// dernieresErreursConteneur : 3 dernières lignes d'erreur ou d'avertissement
// de ses logs sur 6 h (niveau lu, ou mot « error », « warn »… dans le texte).
func (c *Console) dernieresErreursConteneur(ctx context.Context, machine, nom string, t time.Time) []ligneLogCt {
	if c.opt.Logs == nil {
		return nil
	}
	ctx, fin := context.WithTimeout(ctx, 3*time.Second)
	defer fin()
	res, err := c.chercherLogs(ctx, journaux.Requete{Debut: t.Add(-6 * time.Hour).UnixMilli(), Fin: t.UnixMilli(),
		Champs: map[string]string{"host": machine, "container": nom}, Limite: 300})
	if err != nil {
		return nil
	}
	var out []ligneLogCt
	for _, l := range res.Lignes { // les plus récentes d'abord
		niveau := l.Champs["level"]
		classe := classeNiveau(niveau)
		if classe == "" {
			m := reErreurLog.FindString(l.Msg)
			if m == "" {
				continue
			}
			if classe = classeNiveau(strings.ToLower(m)); classe == "" {
				classe = "err" // exception, err…
			}
			niveau = "" // le mot est déjà dans le message
		}
		msg := l.Msg
		if r := []rune(msg); len(r) > 200 {
			msg = string(r[:200]) + "…"
		}
		out = append(out, ligneLogCt{Heure: time.UnixMilli(l.T).In(fuseau.Load()).Format("15:04:05"), Niveau: strings.ToUpper(niveau), Classe: classe, Msg: msg})
		if len(out) == 3 {
			break
		}
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 { // ordre chronologique
		out[i], out[j] = out[j], out[i]
	}
	return out
}
