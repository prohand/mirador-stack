// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"context"
	"fmt"
	"math"
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
	Jours  int
	Nom    string
	Classe string // ok, attention, erreur
	Connu  bool
}

// siteApercu : pastilles d'un site (« Le parc d'un coup d'œil »).
type siteApercu struct {
	Titre    string
	Machines []puceEquipement
}

// donneesRegarder : tout ce qui est propre à la nouvelle présentation.
type donneesRegarder struct {
	Bandeau  bandeauAccueil
	Regarder []aRegarder
	Sondes   casesSondes
	Cert     caseCertificat
	Apercu   []siteApercu
	NbSites  int // sites nommés (sous-titre de la page)
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
	var muettes, disques, securite, redemarrer []string
	nbSecu := 0
	for _, l := range d.Parc {
		if l.Muette {
			muettes = append(muettes, l.Nom)
		}
		if l.PleinDans != "" {
			disques = append(disques, l.Nom+" : "+l.PleinDans)
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
		add(aRegarder{Classe: "attention", Icone: "calendrier", Puce: "orange", Titre: accorderTexte(strconv.Itoa(n) + " tâche(s) planifiée(s) en échec ou en retard"),
			Texte: "sauvegardes, timers, tâches suivies", Lien: "/taches?filtre=probleme", Action: "Détail"})
	}
	if len(disques) > 0 {
		titre := "1 disque bientôt plein"
		if len(disques) > 1 {
			titre = strconv.Itoa(len(disques)) + " disques bientôt pleins"
		}
		add(aRegarder{Classe: "attention", Icone: "disque", Puce: "orange", Titre: titre,
			Texte: resumeNoms(disques), Lien: "/machines", Action: "Voir"})
	}
	if r.Cert.Connu && r.Cert.Classe != "ok" {
		add(aRegarder{Classe: r.Cert.Classe, Icone: "cadenas", Puce: "orange", Titre: "Certificat bientôt expiré : " + r.Cert.Nom,
			Texte: fmt.Sprintf("expire dans %d j", r.Cert.Jours), Lien: "/sondes", Action: "Voir"})
	}
	if nbSecu > 0 {
		add(aRegarder{Classe: "attention", Icone: "securite", Puce: "orange", Titre: accorderTexte(strconv.Itoa(nbSecu) + " paquet(s) de sécurité à installer"),
			Texte: resumeNoms(securite), Lien: "/machines", Action: "Voir"})
	}
	if len(redemarrer) > 0 {
		add(aRegarder{Classe: "attention", Icone: "eclair", Puce: "orange", Titre: accorderTexte(strconv.Itoa(len(redemarrer)) + " machine(s) à redémarrer"),
			Texte: resumeNoms(redemarrer) + " · après une mise à jour", Lien: "/machines", Action: "Voir"})
	}
	if d.Nouveautes > 0 {
		add(aRegarder{Classe: "info", Icone: "loupe", Puce: "bleu", Titre: accorderTexte(strconv.Itoa(d.Nouveautes) + " élément(s) détecté(s) à valider"),
			Texte: "services, conteneurs, tâches trouvés par les agents", Lien: "/machines", Action: "Voir"})
	}
	if d.EnAttente > 0 {
		add(aRegarder{Classe: "info", Icone: "machines", Puce: "bleu", Titre: accorderTexte(strconv.Itoa(d.EnAttente) + " machine(s) en attente d'approbation"),
			Texte: "elles n'envoient rien tant qu'elles ne sont pas approuvées", Lien: "/machines#attente", Action: "Examiner"})
	}
	if len(d.Canaux) == 0 {
		add(aRegarder{Classe: "info", Icone: "courrier", Puce: "bleu", Titre: "Aucun canal de notification",
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
	default:
		// Texte : ce que les cases du dessous ne disent pas (retour du
		// 22/11 : « 5 sur 5 répondent » répétait la case Machines).
		texte := "rien à regarder pour l'instant"
		if n := len(r.Regarder); n > 0 {
			texte = accorderTexte(strconv.Itoa(n) + " point(s) à regarder plus bas, sans urgence")
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
		{`min by (path) (last_over_time(ssl_cert_not_after_timestamp_seconds[1h]))`, "path"},
	} {
		ech, err := c.instant(ctx, x.expr, t)
		if err != nil {
			continue
		}
		for _, e := range ech {
			if e.Valeur < min {
				min, out.Nom, out.Connu = e.Valeur, e.Etiquettes.Get(x.etiq), true
			}
		}
	}
	if !out.Connu {
		return out
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
