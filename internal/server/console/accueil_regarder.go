// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"context"
	"fmt"
	"math"
	"mirador-stack/internal/server/alertes"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Vue d'ensemble, nouvelle présentation (maquette validée le 22/11, SPEC
// § 120) : un bandeau pour le plus grave, des cases (machines, alertes,
// sondes, certificats, stockage), la liste « À regarder » du plus grave au
// moins grave, « Le parc d'un coup d'œil » (une pastille par machine et la
// carte de chaleur du processeur réduite aux 5 plus chargées) et une ligne
// de pied (sauvegarde, canaux, mode ombre).
//
// Coût : 3 requêtes instantanées en plus (sondes, deux échéances de
// certificats), passées par le cache de requêtes ; le reste vient des
// données déjà lues pour la page (parc, alertes, canaux, sauvegarde).

// aRegarder : une ligne de la liste « À regarder ».
type aRegarder struct {
	Classe       string // erreur, attention, info
	Icone, Puce  string // icône et couleur de sa puce
	Titre, Texte string
	Lien, Action string
	Court        string // mot court pour le bandeau (« disque », « paquets de sécurité »…)
}

// bandeauAccueil : le plus grave en haut de la page (ou « tout va bien »).
type bandeauAccueil struct {
	Classe       string // erreur, attention, ok
	Titre, Texte string
	Lien, Action string
}

// casesSondes, caseCertificat : cases du haut.
type casesSondes struct {
	Total, OK int
	Echecs    string
}

type caseCertificat struct {
	Jours   int
	Nom     string
	Machine string // certificat lu par l'agent d'une machine ("" : sonde)
	Lien    string // page où le voir (retour du 26/11 : « Voir » menait à /sondes)
	Classe  string // ok, attention, erreur
	Connu   bool
}

// siteApercu : pastilles d'un site (« Le parc d'un coup d'œil »).
type siteApercu struct {
	Titre    string
	Machines []puceEquipement
	// Grand parc (SPEC § 141, maquette validée le 27/11) : une ligne par
	// site, barre d'état, seules les machines à regarder sont nommées.
	Total, Erreurs, Attentions, Ok int
	ARegarder                      int
	PctErreur, PctAttention, PctOk int
	Lien                           string
}

// donneesRegarder : tout ce qui est propre à la nouvelle présentation.
type donneesRegarder struct {
	Bandeau  bandeauAccueil
	Regarder []aRegarder
	Sondes   casesSondes
	Cert     caseCertificat
	Apercu   []siteApercu
	NbSites  int // sites nommés (sous-titre de la page)
	// ApercuCompact : grand parc, une ligne par site ; SitesVerts : sites
	// tout verts non montrés (« + N autres sites, tous verts »).
	ApercuCompact bool
	SitesVerts    int
	// ChaleurCPU : carte du processeur réduite aux plus chargées.
	ChaleurCPU *vueChaleur
}

// lignesChaleurApercu : machines de la carte de chaleur réduite.
const lignesChaleurApercu = 5

// regarderAccueil : liste, bandeau, cases et aperçu, d'après d (déjà rempli).
func (c *Console) regarderAccueil(ctx context.Context, d *donneesAccueil, majEchec map[string]string, t time.Time) donneesRegarder {
	var r donneesRegarder
	r.Sondes = c.casesSondes(ctx, t)
	r.Cert = c.caseCertificat(ctx, t)

	// Machines muettes, mises à jour en échec, disques, paquets, redémarrages.
	var muettes, disques, disquesDe, securite, redemarrer []string
	nbSecu := 0
	for _, l := range d.Parc {
		if l.Muette {
			muettes = append(muettes, l.Nom)
		}
		if l.PleinDans != "" {
			disques = append(disques, l.Nom+" : "+l.PleinDans)
			disquesDe = append(disquesDe, l.Nom)
		}
		if l.PaquetsSecurite > 0 {
			nbSecu += l.PaquetsSecurite
			securite = append(securite, l.Nom)
		}
		if l.Redemarrage {
			redemarrer = append(redemarrer, l.Nom)
		}
	}
	add := func(x aRegarder) { r.Regarder = append(r.Regarder, x) }
	if n := len(muettes); n > 0 {
		add(aRegarder{Classe: "erreur", Icone: "machines", Puce: "rouge", Titre: accorderTexte(strconv.Itoa(n) + " machine(s) muette(s)"),
			Texte: resumeNoms(muettes) + " · plus de 5 min sans données", Lien: "/machines?filtre=problemes", Action: "Voir"})
	}
	for _, nom := range trieesCles(majEchec) {
		add(aRegarder{Classe: "erreur", Icone: "telechargement", Puce: "rouge", Titre: nom + " : mise à jour en échec",
			Texte: majEchec[nom], Lien: "/machines/" + url.PathEscape(nom) + "#maj", Action: "Voir"})
	}
	if !d.SauvegardeOK {
		add(aRegarder{Classe: "erreur", Icone: "sauvegarde", Puce: "rouge", Titre: "Aucune sauvegarde réussie du serveur",
			Texte: "la base du serveur n'est pas sauvegardée", Lien: "/reglages/sauvegardes", Action: "Régler"})
	}
	for _, e := range d.Canaux {
		if e.EnPanne {
			add(aRegarder{Classe: "erreur", Icone: "courrier", Puce: "rouge", Titre: "Canal " + majusculeTexte(e.Nom) + " en panne",
				Texte: e.Erreur, Lien: "/reglages/notifications", Action: "Voir"})
		}
	}
	if r.Sondes.Total > r.Sondes.OK {
		n := r.Sondes.Total - r.Sondes.OK
		add(aRegarder{Classe: "erreur", Icone: "globe", Puce: "rouge", Titre: accorderTexte(strconv.Itoa(n) + " sonde(s) en échec"),
			Texte: r.Sondes.Echecs, Lien: "/sondes", Action: "Voir"})
	}
	if n := c.nbTachesEnProbleme(ctx, t); n > 0 {
		add(aRegarder{Classe: "attention", Icone: "calendrier", Puce: "orange", Court: "tâches", Titre: accorderTexte(strconv.Itoa(n) + " tâche(s) planifiée(s) en échec ou en retard"),
			Texte: "sauvegardes, timers, tâches suivies", Lien: "/taches?filtre=probleme", Action: "Détail"})
	}
	if len(disques) > 0 {
		titre := "1 disque bientôt plein"
		if len(disques) > 1 {
			titre = strconv.Itoa(len(disques)) + " disques bientôt pleins"
		}
		// Une machine : ses graphiques Disques ; plusieurs : machines à
		// regarder (retour du 26/11 : « Voir » menait à toutes les machines).
		lien := "/machines?filtre=problemes"
		if len(disquesDe) == 1 {
			lien = "/machines/" + url.PathEscape(disquesDe[0]) + "#" + ancre("Disques")
		}
		add(aRegarder{Classe: "attention", Icone: "disque", Puce: "orange", Court: "disque", Titre: titre,
			Texte: resumeNoms(disques), Lien: lien, Action: "Voir"})
	}
	if r.Cert.Connu && r.Cert.Classe != "ok" {
		add(aRegarder{Classe: r.Cert.Classe, Icone: "cadenas", Puce: "orange", Court: "certificat", Titre: "Certificat bientôt expiré : " + r.Cert.Nom,
			Texte: fmt.Sprintf("expire dans %d j", r.Cert.Jours) + texteSi(r.Cert.Machine != "", " · "+r.Cert.Machine), Lien: r.Cert.Lien, Action: "Voir"})
	}
	if nbSecu > 0 {
		add(aRegarder{Classe: "attention", Icone: "securite", Puce: "orange", Court: "paquets de sécurité", Titre: accorderTexte(strconv.Itoa(nbSecu) + " paquet(s) de sécurité à installer"),
			Texte: resumeNoms(securite), Lien: "/machines", Action: "Voir"})
	}
	if len(redemarrer) > 0 {
		add(aRegarder{Classe: "attention", Icone: "eclair", Puce: "orange", Court: "redémarrage", Titre: accorderTexte(strconv.Itoa(len(redemarrer)) + " machine(s) à redémarrer"),
			Texte: resumeNoms(redemarrer) + " · après une mise à jour", Lien: "/machines", Action: "Voir"})
	}
	if d.Nouveautes > 0 {
		add(aRegarder{Classe: "info", Icone: "loupe", Puce: "bleu", Court: "éléments à valider", Titre: accorderTexte(strconv.Itoa(d.Nouveautes) + " élément(s) détecté(s) à valider"),
			Texte: "services, conteneurs, tâches trouvés par les agents", Lien: "/machines", Action: "Voir"})
	}
	if d.EnAttente > 0 {
		add(aRegarder{Classe: "info", Icone: "machines", Puce: "bleu", Court: "machine en attente", Titre: accorderTexte(strconv.Itoa(d.EnAttente) + " machine(s) en attente d'approbation"),
			Texte: "elles n'envoient rien tant qu'elles ne sont pas approuvées", Lien: "/machines#attente", Action: "Examiner"})
	}
	if len(d.Canaux) == 0 {
		add(aRegarder{Classe: "info", Icone: "courrier", Puce: "bleu", Court: "aucun canal", Titre: "Aucun canal de notification",
			Texte: "les alertes ne partent nulle part", Lien: "/reglages/notifications", Action: "Configurer"})
	}

	// Bandeau : le plus grave, sinon « tout va bien ».
	switch {
	case len(d.Sites) > 0:
		// Le bloc « Sites injoignables » le dit déjà, en plus détaillé.
	case len(muettes) > 0:
		titre := "1 machine ne répond plus"
		if len(muettes) > 1 {
			titre = strconv.Itoa(len(muettes)) + " machines ne répondent plus"
		}
		r.Bandeau = bandeauAccueil{Classe: "erreur", Titre: titre,
			Texte: resumeNoms(muettes) + modeOmbre(d.Mode), Lien: "/machines?filtre=problemes", Action: "Voir les machines"}
	case d.Critiques > 0:
		r.Bandeau = bandeauAccueil{Classe: "erreur", Titre: accorderTexte(strconv.Itoa(d.Critiques) + " alerte(s) critique(s) en cours"),
			Texte: "voir la liste des alertes en cours plus bas" + modeOmbre(d.Mode), Lien: "/alertes", Action: "Voir les alertes"}
	case len(r.Regarder) > 0 && r.Regarder[0].Classe == "erreur":
		x := r.Regarder[0]
		r.Bandeau = bandeauAccueil{Classe: "attention", Titre: x.Titre, Texte: x.Texte, Lien: x.Lien, Action: x.Action}
	// Retour du 25/11 (SPEC § 127) : « Tout va bien » s'affichait avec des
	// avertissements en cours ou des pastilles orange ou rouges.
	case d.Avertissements > 0:
		r.Bandeau = bandeauAccueil{Classe: "attention", Titre: accorderTexte(strconv.Itoa(d.Avertissements) + " avertissement(s) en cours"),
			Texte: resumeAvertissements(d.Alertes) + modeOmbre(d.Mode), Lien: "/alertes", Action: "Voir les alertes"}
	case len(aRegarderMachines(d, majEchec)) > 0:
		// Retour du 26/11 : le bandeau dit pourquoi (« sup : disque plein
		// dans 13 j ») et mène à l'endroit qui le montre.
		rs := raisonsMachines(d, majEchec)
		textes := make([]string, len(rs))
		for i, x := range rs {
			textes[i] = x.Nom + " : " + x.Raison
		}
		b := bandeauAccueil{Classe: "attention", Titre: accorderTexte(strconv.Itoa(len(rs)) + " machine(s) à regarder"),
			Texte: resumeNoms(textes), Lien: "/machines?filtre=problemes", Action: "Voir les machines"}
		if len(rs) == 1 {
			b.Lien, b.Action = rs[0].Lien, "Voir "+rs[0].Nom
		}
		r.Bandeau = b
	default:
		// Texte : ce que les cases du dessous ne disent pas (retour du
		// 22/11 : « 5 sur 5 répondent » répétait la case Machines) ; les
		// points sans urgence et les simples infos sont comptés à part.
		var points, infos []string
		for _, x := range r.Regarder {
			if x.Classe == "info" {
				infos = append(infos, x.Court)
			} else {
				points = append(points, x.Court)
			}
		}
		var morceaux []string
		if n := len(points); n > 0 {
			morceaux = append(morceaux, accorderTexte(strconv.Itoa(n)+" point(s) sans urgence")+" plus bas ("+strings.Join(points, ", ")+")")
		}
		if n := len(infos); n > 0 {
			morceaux = append(morceaux, accorderTexte(strconv.Itoa(n)+" info(s)")+" ("+strings.Join(infos, ", ")+")")
		}
		texte := "rien à regarder pour l'instant"
		if len(morceaux) > 0 {
			texte = strings.Join(morceaux, " · ")
		}
		r.Bandeau = bandeauAccueil{Classe: "ok", Titre: "Tout va bien", Texte: texte}
	}

	// Aperçu par site.
	parSite := map[string][]puceEquipement{}
	for _, l := range d.Parc {
		p := puceEquipement{Nom: l.Nom, Point: "ok"}
		switch {
		case l.Muette || l.ClasseEtat == "erreur" || majEchec[l.Nom] != "":
			p.Point = "erreur"
		case l.ClasseEtat == "attention" || l.PleinDans != "":
			p.Point = "attention"
		}
		parSite[l.Site] = append(parSite[l.Site], p)
	}
	for _, s := range trieesCles(parSite) {
		ms := parSite[s]
		rang := map[string]int{"erreur": 0, "attention": 1, "ok": 2}
		sort.SliceStable(ms, func(i, j int) bool {
			if rang[ms[i].Point] != rang[ms[j].Point] {
				return rang[ms[i].Point] < rang[ms[j].Point]
			}
			return ms[i].Nom < ms[j].Nom
		})
		r.Apercu = append(r.Apercu, siteApercu{Titre: s, Machines: ms})
	}
	for _, a := range r.Apercu {
		if a.Titre != "" {
			r.NbSites++
		}
	}
	// « sans site » en dernier.
	sort.SliceStable(r.Apercu, func(i, j int) bool { return r.Apercu[i].Titre != "" && r.Apercu[j].Titre == "" })
	if len(d.Parc) > seuilCompact {
		r.ApercuCompact = true
		r.Apercu, r.SitesVerts = compacterApercu(r.Apercu)
	}

	if len(d.Chaleur) > 0 {
		h := d.Chaleur[0] // processeur
		if len(h.Lignes) > lignesChaleurApercu {
			h.Lignes = h.Lignes[:lignesChaleurApercu]
		}
		h.Cachees = 0
		r.ChaleurCPU = &h
	}
	return r
}

// resumeAvertissements : « Service en échec (vps-web) · Disque bientôt
// plein (nas) » pour les avertissements actifs (3 au plus, puis « et N autres »).
func resumeAvertissements(as []resumeAlerte) string {
	var morceaux []string
	n := 0
	for _, a := range as {
		if a.Etat != "active" || a.Sourdine != "" || a.Gravite != alertes.Avertissement {
			continue
		}
		n++
		if len(morceaux) < 3 {
			t := libelleRegle(a.Regle)
			if a.Machine != "" {
				t += " (" + a.Machine + ")"
			}
			morceaux = append(morceaux, t)
		}
	}
	if n > len(morceaux) {
		morceaux = append(morceaux, "et "+strconv.Itoa(n-len(morceaux))+" autre(s)")
	}
	return strings.Join(morceaux, " · ")
}

// aRegarderMachines : machines dont la pastille de l'aperçu est orange ou
// rouge (même règle que l'aperçu plus bas), par nom.
func aRegarderMachines(d *donneesAccueil, majEchec map[string]string) []string {
	var out []string
	for _, l := range d.Parc {
		if l.Muette || l.ClasseEtat == "erreur" || l.ClasseEtat == "attention" || l.PleinDans != "" || majEchec[l.Nom] != "" {
			out = append(out, l.Nom)
		}
	}
	sort.Strings(out)
	return out
}

// raisonMachine : pourquoi une machine est à regarder, et où le voir. Le
// disque mène à la rangée « Disques » des graphiques (ancre « r-disques »,
// la même que le sommaire de la page machine).
type raisonMachine struct{ Nom, Raison, Lien string }

// raisonsMachines : machines de aRegarderMachines avec la raison la plus
// grave de chacune (muette, mise à jour en échec, alerte, disque).
func raisonsMachines(d *donneesAccueil, majEchec map[string]string) []raisonMachine {
	parNom := map[string]ligneParc{}
	for _, l := range d.Parc {
		parNom[l.Nom] = l
	}
	var out []raisonMachine
	for _, nom := range aRegarderMachines(d, majEchec) {
		l, page := parNom[nom], "/machines/"+url.PathEscape(nom)
		x := raisonMachine{Nom: nom, Lien: page}
		switch {
		case l.Muette:
			x.Raison = "plus de données depuis plus de 5 min"
		case majEchec[nom] != "":
			x.Raison, x.Lien = "mise à jour de l'agent en échec", page+"#maj"
		case l.ClasseEtat == "erreur" || l.ClasseEtat == "attention":
			x.Raison, x.Lien = accorderTexte(strconv.Itoa(max(l.Alertes, 1))+" alerte(s) en cours"), page+"#activite"
			if n := nbPrises(d, nom); n > 0 {
				x.Raison = accorderTexte(strconv.Itoa(n) + " alerte(s) prise(s) en charge")
			}
		default:
			x.Raison, x.Lien = "disque "+l.PleinDans, page+"#"+ancre("Disques")
		}
		out = append(out, x)
	}
	return out
}

// nbPrises : alertes prises en charge (acquittées) d'une machine.
func nbPrises(d *donneesAccueil, machine string) int {
	n := 0
	for _, a := range d.Prises {
		if a.Machine == machine {
			n++
		}
	}
	return n
}

// modeOmbre : précision ajoutée au bandeau quand rien n'est envoyé.
func modeOmbre(mode string) string {
	if mode == "ombre" {
		return " · aucune notification envoyée (mode ombre)"
	}
	return ""
}

// majusculeTexte : première lettre en capitale.
func majusculeTexte(s string) string {
	if s == "" {
		return s
	}
	r := []rune(s)
	return strings.ToUpper(string(r[0])) + string(r[1:])
}

// trieesCles : clés d'une carte, triées (vide en dernier).
func trieesCles[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// casesSondes : sondes réussies sur le total, et les noms en échec.
func (c *Console) casesSondes(ctx context.Context, t time.Time) casesSondes {
	var s casesSondes
	ech, err := c.instant(ctx, `max by (sonde) (last_over_time(mirador_sonde_succes[10m]))`, t)
	if err != nil {
		return s
	}
	var echecs []string
	for _, e := range ech {
		s.Total++
		if e.Valeur >= 1 {
			s.OK++
		} else {
			echecs = append(echecs, e.Etiquettes.Get("sonde"))
		}
	}
	sort.Strings(echecs)
	s.Echecs = resumeNoms(echecs)
	return s
}

// caseCertificat : échéance la plus proche, sondes HTTP du serveur et des
// collecteurs, et sondes de certificats des collecteurs.
func (c *Console) caseCertificat(ctx context.Context, t time.Time) caseCertificat {
	var out caseCertificat
	min := math.Inf(1)
	for _, x := range []struct{ expr, etiq string }{
		{`min by (sonde) (last_over_time(mirador_sonde_certificat_fin_timestamp_seconds[1h]))`, "sonde"},
		{`min by (instance, path) (last_over_time(ssl_cert_not_after_timestamp_seconds[1h]))`, "path"},
	} {
		ech, err := c.instant(ctx, x.expr, t)
		if err != nil {
			continue
		}
		for _, e := range ech {
			if e.Valeur < min {
				min, out.Nom, out.Connu = e.Valeur, e.Etiquettes.Get(x.etiq), true
				out.Machine = ""
				if x.etiq == "path" {
					out.Machine = e.Etiquettes.Get("instance")
				}
			}
		}
	}
	if !out.Connu {
		return out
	}
	// Certificat d'un fichier lu par un agent : tableau « Certificats TLS »
	// filtré sur sa machine ; certificat d'une sonde : page Sondes.
	out.Lien = "/sondes"
	if out.Machine != "" {
		out.Lien = "/tableaux/certificats-tls?instance=" + url.QueryEscape(out.Machine)
	}
	out.Jours = int(math.Floor(time.Unix(int64(min), 0).Sub(t).Hours() / 24))
	switch {
	case out.Jours < 7:
		out.Classe = "erreur"
	case out.Jours < 21:
		out.Classe = "attention"
	default:
		out.Classe = "ok"
	}
	return out
}

// sitesApercuMax : lignes de sites montrées dans l'aperçu compact (tous les
// sites à problème, puis des sites verts jusqu'à ce nombre).
const sitesApercuMax = 8

// compacterApercu : grand parc. Comptes et barre par site, seules les
// machines à regarder gardées ; sites à problème d'abord (les plus touchés
// en tête), sites verts ensuite, le reste résumé.
func compacterApercu(sites []siteApercu) ([]siteApercu, int) {
	for i := range sites {
		s := &sites[i]
		s.Total = len(s.Machines)
		var garde []puceEquipement
		for _, m := range s.Machines {
			switch m.Point {
			case "erreur":
				s.Erreurs++
			case "attention":
				s.Attentions++
			default:
				s.Ok++
				continue
			}
			garde = append(garde, m)
		}
		s.Machines = garde
		s.ARegarder = s.Erreurs + s.Attentions
		if s.Total > 0 {
			s.PctErreur = (s.Erreurs*100 + s.Total - 1) / s.Total
			s.PctAttention = (s.Attentions*100 + s.Total - 1) / s.Total
			s.PctOk = max(0, 100-s.PctErreur-s.PctAttention)
		}
		s.Lien = "/machines?site=" + url.QueryEscape(s.Titre)
		if s.Titre == "" {
			s.Lien = "/machines"
		}
	}
	sort.SliceStable(sites, func(i, j int) bool {
		pi, pj := sites[i].Erreurs*1000+sites[i].Attentions, sites[j].Erreurs*1000+sites[j].Attentions
		return pi > pj
	})
	var out []siteApercu
	verts := 0
	for _, s := range sites {
		if s.Erreurs+s.Attentions > 0 || len(out) < sitesApercuMax {
			out = append(out, s)
			continue
		}
		verts++
	}
	return out, verts
}
