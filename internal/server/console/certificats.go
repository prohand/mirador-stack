// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"mirador-stack/internal/server/certsyslog"
	"mirador-stack/internal/server/domaines"
	"mirador-stack/internal/server/flotte"
)

// Certificats du syslog TLS des collecteurs (SPEC § 32) : réglages communs
// (compte DNS, Let's Encrypt) dans Réglages › Certificats, et avancement de
// la création du certificat, pas à pas, pour chaque collecteur.

// etapeCert : une ligne de l'avancement.
type etapeCert struct {
	Titre, Detail string
	Etat          string // fait, encours, erreur, afaire
}

// avancement : toutes les étapes, de la configuration à l'écoute ouverte.
type avancement struct {
	Etapes  []etapeCert
	EnCours bool   // une étape avance : page rechargée toute seule
	Resume  string // étape à retenir (tableau de Réglages › Certificats)
	Classe  string // ok, attention, erreur, ou vide (à venir)
}

// reglagesCertificats : compte DNS et Let's Encrypt (communs).
type reglagesCertificats struct {
	Domaine, Annuaire, Fournisseur string
	JetonEnregistre                bool
	Parametres                     map[string]string
	Fournisseurs                   []certsyslog.InfoFournisseur
	AttenteMax                     int // secondes, 0 = selon le fournisseur
	ChoixAttente                   []struct {
		Secondes int
		Titre    string
	}
}

func (r reglagesCertificats) Faits() bool { return r.Domaine != "" && r.JetonEnregistre }

func lireReglagesCertificats(ctx context.Context, c *Console) reglagesCertificats {
	v := reglagesCertificats{Fournisseurs: certsyslog.Fournisseurs, ChoixAttente: certsyslog.ChoixAttente}
	if r, err := certsyslog.LireReglages(ctx, c.db); err == nil {
		v.Domaine, v.Annuaire, v.JetonEnregistre, v.AttenteMax = r.Domaine, r.Annuaire, r.IdentifiantsEnregistres(), r.AttenteMax
		v.Fournisseur, v.Parametres = r.FournisseurEffectif(), r.Parametres
	}
	if v.Fournisseur == "" {
		v.Fournisseur = "desec"
	}
	return v
}

// heureCourte : « 23:41 » aujourd'hui, sinon « 15/11 23:41 ».
func heureCourte(s int64, t time.Time) string {
	if s == 0 {
		return ""
	}
	h := time.Unix(s, 0).In(fuseau.Load())
	if h.Format("2006-01-02") == t.In(fuseau.Load()).Format("2006-01-02") {
		return h.Format("15:04")
	}
	return h.Format("02/01 15:04")
}

// titreEtape : libellé d'une étape de la démarche Let's Encrypt.
func titreEtape(code string) string {
	for _, e := range certsyslog.EtapesACME {
		if e.Code == code {
			return e.Titre
		}
	}
	return code
}

// ecouteCollecteur : écoute syslog ouverte, et fin du certificat chargé par
// le collecteur (ses propres mesures ; 0 si inconnues).
func (c *Console) ecouteCollecteur(ctx context.Context, nom string, t time.Time) (ouverte bool, finCert int64) {
	if c.lecteur == nil || !reValeurVariable.MatchString(nom) {
		return false, 0
	}
	i := `{instance="` + nom + `"}`
	if ech, err := c.instant(ctx, `max(mirador_collecteur_syslog_ecoute`+i+`)`, t); err == nil && len(ech) == 1 {
		ouverte = ech[0].Valeur > 0
	}
	if ech, err := c.instant(ctx, `max(mirador_collecteur_syslog_certificat_fin_timestamp_seconds`+i+`)`, t); err == nil && len(ech) == 1 && !math.IsNaN(ech[0].Valeur) {
		finCert = int64(ech[0].Valeur)
	}
	return ouverte, finCert
}

// avancementCertificat : de la configuration à l'écoute ouverte.
func (c *Console) avancementCertificat(ctx context.Context, nomCollecteur string, s vueSyslog, r reglagesCertificats, t time.Time) avancement {
	var a avancement
	ajout := func(titre, etat, detail string) {
		a.Etapes = append(a.Etapes, etapeCert{Titre: titre, Etat: etat, Detail: detail})
	}
	e := s.Cert
	// 1. Compte DNS (commun à tous les collecteurs).
	if r.Faits() {
		ajout("Compte DNS", "fait", "réglé : zone "+r.Domaine)
	} else {
		ajout("Compte DNS", "afaire", "à régler une fois pour tous les collecteurs : Réglages › Certificats")
	}
	// 2. Syslog réglé sur ce collecteur.
	if s.Regle {
		ajout("Syslog du collecteur", "fait", "réglé : "+s.Nom+" · écoute "+s.Ecoute)
	} else {
		ajout("Syslog du collecteur", "afaire", "bloc « Syslog TLS » ci-dessus")
	}
	// 3. Demande (clé créée sur le collecteur, jamais envoyée).
	switch {
	case e.CSR:
		ajout("Demande du collecteur", "fait", "reçue à "+heureCourte(e.CSRRecueLe, t)+" (clé privée restée sur le collecteur)")
	case s.Regle:
		ajout("Demande du collecteur", "encours", "il l'envoie à son prochain contact, sous 5 min")
		a.EnCours = true
	default:
		ajout("Demande du collecteur", "afaire", "")
	}
	// 4. Démarche Let's Encrypt.
	obtenu := e.Certificat != "" && e.Nom == s.Nom && e.Etape == "" && e.Erreur == ""
	switch {
	case e.Erreur != "":
		titre := "Let's Encrypt"
		if e.Etape != "" {
			titre += " : " + strings.ToLower(titreEtape(e.Etape)[:1]) + titreEtape(e.Etape)[1:]
		}
		detail := e.Erreur
		if e.EssaiLe > 0 {
			detail += " · nouvel essai seul vers " + heureCourte(e.EssaiLe+int64(certsyslog.AttenteApresEchec/time.Second), t) + " (ou « Renouveler maintenant »)"
		}
		ajout(titre, "erreur", detail)
	case e.Etape != "":
		detail := "étape en cours depuis " + heureCourte(e.EtapeLe, t)
		if e.EtapeJusqua > 0 {
			detail = "au plus jusqu'à " + heureCourte(e.EtapeJusqua, t)
		}
		if e.EtapeInfo != "" {
			detail = e.EtapeInfo + " · " + detail
		}
		n := 0
		for i, x := range certsyslog.EtapesACME {
			if x.Code == e.Etape {
				n = i + 1
			}
		}
		ajout("Let's Encrypt : "+strings.ToLower(titreEtape(e.Etape)[:1])+titreEtape(e.Etape)[1:]+" ("+strconv.Itoa(n)+"/"+strconv.Itoa(len(certsyslog.EtapesACME))+")", "encours", detail)
		a.EnCours = true
	case obtenu:
		ajout("Let's Encrypt", "fait", "défi DNS validé")
	case e.CSR && r.Faits():
		ajout("Let's Encrypt", "encours", "démarche lancée au prochain passage du serveur (moins d'une minute)")
		a.EnCours = true
	default:
		ajout("Let's Encrypt", "afaire", "défi DNS fait par le serveur : 1 à 5 min (jusqu'à 20 chez CSC)")
	}
	// 5. Certificat obtenu.
	if e.Certificat != "" {
		etat := "fait"
		detail := "obtenu, valable jusqu'au " + time.Unix(e.Fin, 0).In(fuseau.Load()).Format("02/01/2006") + ", renouvelé seul 30 jours avant"
		if e.Nom != s.Nom && s.Nom != "" {
			etat, detail = "encours", "l'ancien ("+e.Nom+") reste en place jusqu'au nouveau"
		}
		ajout("Certificat", etat, detail)
	} else {
		ajout("Certificat", "afaire", "")
	}
	// 6. Chargé par le collecteur, écoute ouverte (ses propres mesures).
	ouverte, finCollecteur := c.ecouteCollecteur(ctx, nomCollecteur, t)
	pris := e.Certificat != "" && finCollecteur > 0 && math.Abs(float64(finCollecteur-e.Fin)) < 86400
	switch {
	case pris && ouverte:
		ajout("Installation sur le collecteur", "fait", "écoute ouverte : les équipements peuvent envoyer vers "+s.Nom)
	case e.Certificat != "" && s.Regle:
		ajout("Installation sur le collecteur", "encours", "envoyé avec sa configuration : pris en compte sous 5 min")
		a.EnCours = true
	default:
		ajout("Installation sur le collecteur", "afaire", "")
	}
	// Résumé : la première étape qui n'est pas faite.
	a.Resume, a.Classe = "Écoute ouverte", "ok"
	for _, x := range a.Etapes {
		if x.Etat != "fait" {
			prefixe := map[string]string{"encours": "En cours · ", "erreur": "Échec · ", "afaire": "À faire · "}[x.Etat]
			a.Resume = prefixe + x.Titre
			a.Classe = map[string]string{"encours": "attention", "erreur": "erreur", "afaire": ""}[x.Etat]
			break
		}
	}
	if !s.Regle && e.Certificat == "" {
		a.Resume, a.Classe = "Syslog pas réglé", ""
	}
	return a
}

// --- Page Réglages › Certificats -------------------------------------------------

// machinesParNom : machines du parc, par nom.
func machinesParNom(ctx context.Context, c *Console) map[string]flotte.InfoMachine {
	out := map[string]flotte.InfoMachine{}
	if ms, err := flotte.ListerMachines(ctx, c.db); err == nil {
		for _, m := range ms {
			out[m.Nom] = m
		}
	}
	return out
}

type ligneCertificat struct {
	Collecteur string
	Nom        string // nom du certificat réglé
	Fin        int64
	Renouveler bool // une demande existe : « Renouveler maintenant » possible
	Avancement avancement
	Erreur     string
	// État résumé (retour du 21/11) : « Valide · expire dans 62 j ».
	Classe, Etat string
}

type donneesCertificats struct {
	R        reglagesCertificats
	Lignes   []ligneCertificat
	Domaines []ligneDomaine // expiration des noms de domaine (27/11)
}

// ligneDomaine : un domaine principal et sa date d'expiration.
type ligneDomaine struct {
	Nom, Sous, Expire, Classe, Erreur, Verifie string
}

// lignesDomaines : 30 j : attention, 7 j : erreur ; date inconnue : grise.
func lignesDomaines(ds []domaines.Domaine, t time.Time) []ligneDomaine {
	var out []ligneDomaine
	for _, d := range ds {
		l := ligneDomaine{Nom: d.Nom, Sous: strings.ReplaceAll(strings.Join(d.Sous, ", "), "@", d.Nom), Erreur: d.Erreur}
		if !d.VerifieLe.IsZero() {
			l.Verifie = "lu le " + d.VerifieLe.In(fuseau.Load()).Format("02/01 15:04")
		}
		if d.Expire.IsZero() {
			l.Expire = "inconnue"
		} else {
			j := int(math.Ceil(d.Expire.Sub(t).Hours() / 24)) // jours entamés
			switch {
			case !d.Expire.After(t): // expiré, même depuis moins d'un jour
				l.Expire, l.Classe = "expiré", "erreur"
			case j < 7:
				l.Expire, l.Classe = fmt.Sprintf("dans %d j", j), "erreur"
			case j < 30:
				l.Expire, l.Classe = fmt.Sprintf("dans %d j", j), "attention"
			default:
				l.Expire, l.Classe = fmt.Sprintf("dans %d j", j), "ok"
			}
		}
		out = append(out, l)
	}
	return out
}

func (c *Console) pageCertificats(q *req) {
	ctx := q.r.Context()
	d := donneesCertificats{R: lireReglagesCertificats(ctx, c)}
	cols, err := certsyslog.ListerCollecteurs(ctx, c.db)
	if err != nil {
		c.flash(q, "", err.Error())
	}
	parNom := machinesParNom(ctx, c)
	for _, col := range cols {
		m, ok := parNom[col.Nom]
		if !ok {
			continue
		}
		s := c.syslogDe(ctx, m)
		l := ligneCertificat{Collecteur: col.Nom, Nom: s.Nom, Fin: col.Etat.Fin, Renouveler: col.Etat.CSR,
			Avancement: c.avancementCertificat(ctx, col.Nom, s, d.R, q.t), Erreur: col.Etat.Erreur}
		l.Classe, l.Etat = l.Avancement.Classe, l.Avancement.Resume
		if l.Fin > 0 && !l.Avancement.EnCours && l.Erreur == "" {
			jours := int((l.Fin - q.t.Unix()) / 86400)
			switch {
			case l.Fin <= q.t.Unix(): // expiré, même depuis moins d'un jour
				l.Classe, l.Etat = "erreur", "Expiré"
			case jours <= 7:
				l.Classe, l.Etat = "erreur", fmt.Sprintf("Valide · expire dans %d j", jours)
			case jours <= 30:
				l.Classe, l.Etat = "attention", fmt.Sprintf("Valide · expire dans %d j (renouvellement en cours)", jours)
			default:
				l.Classe, l.Etat = "ok", fmt.Sprintf("Valide · expire dans %d j", jours)
			}
		}
		d.Lignes = append(d.Lignes, l)
	}
	if ds, err := domaines.Lister(ctx, c.db); err == nil {
		d.Domaines = lignesDomaines(ds, q.t)
	}
	c.rendre(q, "certificats", "Certificats", "reglages", d)
}

// postCertificats : réglages communs, ou « Renouveler maintenant ».
func (c *Console) postCertificats(q *req) {
	ctx := q.r.Context()
	if err := c.verifierCode(q); err != nil {
		c.retour(q, "/reglages/certificats", err, "")
		return
	}
	f := func(k string) string { return strings.TrimSpace(q.r.PostFormValue(k)) }
	switch q.r.PostFormValue("action") {
	case "reglages":
		// Champs du fournisseur choisi : « dns_<fournisseur>_<champ> ».
		attente, _ := strconv.Atoi(f("attente_max"))
		s := certsyslog.Saisie{Fournisseur: f("fournisseur"), Domaine: f("domaine"), Annuaire: f("annuaire"), AttenteMax: attente, Identifiants: map[string]string{}}
		if info := certsyslog.InfoDe(s.Fournisseur); info != nil {
			for _, ch := range info.Champs {
				s.Identifiants[ch.Nom] = q.r.PostFormValue("dns_" + info.Nom + "_" + ch.Nom)
			}
		}
		c.retour(q, "/reglages/certificats", certsyslog.DefinirReglages(ctx, c.db, c.opt.Cle, s, q.qui(), q.t),
			"réglages enregistrés (les secrets ne sont jamais réaffichés)")
	case "renouveler":
		var err error
		m, ok := machinesParNom(ctx, c)[f("collecteur")]
		if !ok || m.Type != "collecteur" {
			err = errors.New("collecteur inconnu")
		}
		if err == nil {
			err = certsyslog.Demander(ctx, c.db, m.ID, q.qui(), q.t)
		}
		c.retour(q, "/reglages/certificats", err, "renouvellement demandé : l'avancement s'affiche ci-dessous")
	default:
		c.retour(q, "/reglages/certificats", errors.New("action inconnue"), "")
	}
}
