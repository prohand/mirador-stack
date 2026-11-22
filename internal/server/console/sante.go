// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"context"
	"math"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"mirador-stack/internal/server/alertes"
	"mirador-stack/internal/server/certsyslog"
	"mirador-stack/internal/server/flotte"
	"mirador-stack/internal/server/notif"
	"mirador-stack/internal/server/sauvegarde"
	essais "mirador-stack/internal/sondes"
)

// Page « Santé de mirador » (SPEC § 121, maquette validée le 22/11) : tout
// ce qui fait tourner mirador, d'un coup d'œil. Une carte pour le serveur
// (VPS), une par collecteur, une pour les agents et les envois directs ;
// chaque ligne : état, une phrase, deux ou trois chiffres, lien vers son
// réglage. Services root repérés (mirador-maj-serveur, assistant root des
// agents).
//
// Lecture seule : la page lit ce que les services envoient déjà (métriques
// internes du serveur, séries des collecteurs et des agents, base) ; aucune
// commande à distance.
//
// Coût : une vingtaine de requêtes instantanées (cache de 20 s de la
// console) et quelques lectures en base, à l'ouverture de la page. La
// pastille du menu reprend le dernier calcul (2 min au plus), refait en
// arrière-plan (boucle de la console) : jamais sur le chemin d'une autre page.

// ligneSante : un service.
type ligneSante struct {
	Etat      string // ok, attention, erreur, eteint
	Icone     string
	Nom       string
	Version   string
	Root      bool
	Sous      string // phrase d'explication
	Alerte    string // ce qui ne va pas (mis en avant)
	Chiffres  []chiffreSante
	Lien      string
	LienTexte string
	Zone      string // serveur, lan
}

type chiffreSante struct{ Valeur, Legende string }

// carteSante : un bloc (serveur, un collecteur, agents).
type carteSante struct {
	Titre, Sous, Icone string
	Lignes             []ligneSante
	NbProblemes, NbOK  int
}

// caseSante : une des quatre cases du haut.
type caseSante struct {
	Titre, Icone, Valeur, Sur, Texte, Classe, Ancre string
}

type donneesSante struct {
	Cartes                   []carteSante
	Cases                    []caseSante
	NbServices, NbProblemes  int
	NbRoot, NbServeur, NbLAN int
	Problemes                []string // « Notifications : … » (bandeau)
	CalculeLe                time.Time
}

func (l ligneSante) Probleme() bool { return l.Etat == "attention" || l.Etat == "erreur" }

// TexteEtat : libellé de la pastille d'état.
func (l ligneSante) TexteEtat() string {
	switch l.Etat {
	case "attention":
		return "À regarder"
	case "erreur":
		return "En panne"
	case "eteint":
		return "Pas utilisé"
	}
	return "En marche"
}

// Groupes : filtres de la page (« probleme root serveur lan »).
func (l ligneSante) Groupes() string {
	g := []string{l.Zone}
	if l.Probleme() {
		g = append(g, "probleme")
	}
	if l.Root {
		g = append(g, "root")
	}
	return strings.Join(g, " ")
}

// cacheSante : dernier calcul (pastille du menu).
type cacheSante struct {
	mu sync.Mutex
	nb int
	le time.Time
}

const fraicheurSante = 2 * time.Minute

// nbSante : services à regarder, pour la pastille du menu (dernier calcul :
// la page elle-même, ou la boucle de la console toutes les 2 min).
func (c *Console) nbSante() int {
	c.sante.mu.Lock()
	defer c.sante.mu.Unlock()
	return c.sante.nb
}

// tourSante : recalcul de fond (boucle de la console), jamais sur le
// chemin d'une autre page.
func (c *Console) tourSante(ctx context.Context, t time.Time) {
	c.sante.mu.Lock()
	frais := t.Sub(c.sante.le) < fraicheurSante
	c.sante.mu.Unlock()
	if !frais {
		c.noterSante(c.calculerSante(ctx, t))
	}
}

func (c *Console) noterSante(d donneesSante) {
	c.sante.mu.Lock()
	c.sante.nb, c.sante.le = d.NbProblemes, d.CalculeLe
	c.sante.mu.Unlock()
}

func (c *Console) pageSante(q *req) {
	d := c.calculerSante(q.r.Context(), q.t)
	c.noterSante(d)
	c.rendre(q, "sante", "Santé de mirador", "sante", d)
}

// calculerSante : toute la page.
func (c *Console) calculerSante(ctx context.Context, t time.Time) donneesSante {
	d := donneesSante{CalculeLe: t}
	machines, _ := flotte.ListerMachines(ctx, c.db)
	sondes, _ := flotte.LireSondes(ctx, c.db)
	lues := c.sondesLues(ctx, t)

	serveur := carteSante{Titre: "Serveur · VPS", Icone: "base", Sous: c.adresseInterface(ctx)}
	serveur.Lignes = c.lignesServeur(ctx, t, sondes, lues)
	d.Cartes = append(d.Cartes, serveur)

	var collecteurs []flotte.InfoMachine
	for _, m := range machines {
		if m.Type == "collecteur" && m.Statut == "approuvee" {
			collecteurs = append(collecteurs, m)
		}
	}
	for _, m := range collecteurs {
		d.Cartes = append(d.Cartes, c.carteCollecteurSante(ctx, m, sondes, lues, t))
	}
	d.Cartes = append(d.Cartes, c.carteAgentsSante(ctx, machines, t))

	// Totaux, bandeau et cases.
	var caseServeur, caseCol, caseAgents, caseEnvois caseSante
	for i := range d.Cartes {
		k := &d.Cartes[i]
		for _, l := range k.Lignes {
			d.NbServices++
			if l.Probleme() {
				k.NbProblemes++
				d.NbProblemes++
				d.Problemes = append(d.Problemes, nomSimple(l.Nom)+" : "+premierTexte(l.Alerte, l.Sous))
			} else {
				k.NbOK++
			}
			if l.Root {
				d.NbRoot++
			}
			if l.Zone == "lan" {
				d.NbLAN++
			} else {
				d.NbServeur++
			}
		}
	}
	// Serveur.
	srv := d.Cartes[0]
	caseServeur = caseSante{Titre: "Serveur (VPS)", Icone: "base", Ancre: "#carte-0",
		Valeur: strconv.Itoa(len(srv.Lignes) - srv.NbProblemes - eteints(srv.Lignes)), Sur: strconv.Itoa(len(srv.Lignes) - eteints(srv.Lignes)),
		Texte: resumeCarte(srv), Classe: classeCarte(srv)}
	// Collecteurs.
	caseCol = caseSante{Titre: "Collecteurs", Icone: "reseau", Ancre: "#carte-1", Valeur: strconv.Itoa(len(collecteurs))}
	var noms []string
	pb := 0
	for i := range collecteurs {
		k := d.Cartes[1+i]
		pb += k.NbProblemes
		noms = append(noms, collecteurs[i].Nom)
		if classeCarte(k) == "erreur" || caseCol.Classe == "" && classeCarte(k) == "attention" {
			caseCol.Classe = classeCarte(k)
		}
	}
	switch {
	case len(collecteurs) == 0:
		caseCol.Texte = "aucun collecteur sur le LAN"
	case pb == 0:
		caseCol.Texte = resumeNoms(noms) + " · tout va bien"
	default:
		caseCol.Texte = resumeNoms(noms) + " · " + accorderTexte(strconv.Itoa(pb)+" partie(s) à regarder")
	}
	// Agents et envois.
	ag := d.Cartes[len(d.Cartes)-1]
	caseAgents = caseSante{Titre: "Agents", Icone: "ecran", Ancre: "#carte-" + strconv.Itoa(len(d.Cartes)-1)}
	caseEnvois = caseSante{Titre: "Envois sans agent", Icone: "envoi", Ancre: caseAgents.Ancre, Valeur: "0", Texte: "aucun équipement n'envoie lui-même"}
	var envois []string
	for _, l := range ag.Lignes {
		switch {
		case l.Nom == "mirador-agent":
			caseAgents.Valeur, caseAgents.Sur, caseAgents.Texte = l.Chiffres[0].Valeur, l.Chiffres[1].Valeur, l.Sous
			if l.Probleme() {
				caseAgents.Classe = l.Etat
			}
		case strings.HasPrefix(l.Nom, "Envoi direct"):
			envois = append(envois, strings.TrimPrefix(l.Nom, "Envoi direct · "))
			if l.Probleme() {
				caseEnvois.Classe = l.Etat
			}
		}
	}
	if len(envois) > 0 {
		caseEnvois.Valeur, caseEnvois.Texte = strconv.Itoa(len(envois)), resumeNoms(envois)
	}
	d.Cases = []caseSante{caseServeur, caseCol, caseAgents, caseEnvois}
	return d
}

// --- Serveur -----------------------------------------------------------------

func (c *Console) lignesServeur(ctx context.Context, t time.Time, sondes []essais.Sonde, lues map[string]map[string]alertes.Echantillon) []ligneSante {
	var out []ligneSante
	srv := func(l ligneSante) { l.Zone = "serveur"; out = append(out, l) }

	// mirador-server.
	l := ligneSante{Etat: "ok", Icone: "base", Nom: "mirador-server", Version: versionCourte(c.opt.Version), Lien: "/reglages/serveur", LienTexte: "Mise à jour"}
	l.Sous = "le serveur et l'interface"
	if !c.opt.Debut.IsZero() {
		l.Sous += " · en marche depuis " + dureeLisible(t.Sub(c.opt.Debut))
	}
	if v, ok := c.une(ctx, `mirador_processus_rss_octets{job="mirador"}`, t); ok {
		l.Chiffres = append(l.Chiffres, chiffreSante{tailleLisible(int64(v)), "mémoire"})
	}
	e := c.etatStockage(t)
	if total := e.Brut + e.Agrege + e.Logs + e.Base; total > 0 {
		l.Chiffres = append(l.Chiffres, chiffreSante{tailleLisible(total), "données"})
	}
	srv(l)

	// mirador-maj-serveur (root, sur le VPS).
	srv(c.ligneMajServeur(ctx, t))

	// Réception des données.
	l = ligneSante{Etat: "ok", Icone: "envoi", Nom: "Réception des données", Sous: "agents, collecteurs et équipements · /api/agent/v1", Lien: "/machines", LienTexte: "Machines"}
	if v, ok := c.une(ctx, `60 * sum(rate(mirador_points_recus_total{job="mirador"}[5m]))`, t); ok {
		l.Chiffres = append(l.Chiffres, chiffreSante{nombreCourt(v) + " /min", "points reçus"})
		if v == 0 && t.Sub(c.opt.Debut) > 10*time.Minute {
			l.Etat, l.Alerte = "erreur", "aucune donnée reçue depuis 5 min"
		}
	}
	if v, ok := c.une(ctx, `sum(increase(mirador_points_rejetes_total{job="mirador"}[1h]))`, t); ok {
		l.Chiffres = append(l.Chiffres, chiffreSante{nombreCourt(v), "rejetés 1 h"})
	}
	if c.opt.Ingestion != nil {
		k := &c.opt.Ingestion.Compteurs
		refus := k.Refus401.Load() + k.Refus403.Load()
		l.Chiffres = append(l.Chiffres, chiffreSante{strconv.FormatUint(refus, 10), "tokens refusés"})
	}
	srv(l)

	// Stockage des métriques et logs.
	cons, _ := LireConservation(ctx, c.db)
	_, disque := c.cartesConservation(e, cons, t)
	l = ligneSante{Etat: "ok", Icone: "graphique", Nom: "Stockage des métriques", Lien: "/reglages/conservation", LienTexte: "Conservation"}
	l.Sous = "base de séries · " + strings.ToLower(premierTexte(disque.Texte, "place sur le disque"))
	switch disque.Classe {
	case "erreur":
		l.Etat, l.Alerte = "erreur", disque.Texte
	case "attention":
		l.Etat, l.Alerte = "attention", disque.Texte
	}
	l.Chiffres = append(l.Chiffres, chiffreSante{tailleLisible(e.Brut + e.Agrege), "taille"})
	if e.Series > 0 {
		l.Chiffres = append(l.Chiffres, chiffreSante{nombreCourt(float64(e.Series)), "séries"})
	}
	if disque.Total > 0 {
		l.Chiffres = append(l.Chiffres, chiffreSante{tailleLisible(disque.Libre), "disque libre"})
	}
	srv(l)
	l = ligneSante{Etat: "ok", Icone: "logs", Nom: "Logs", Sous: "syslog des équipements et journaux des agents", Lien: "/logs", LienTexte: "Logs"}
	if v, ok := c.une(ctx, `60 * sum(rate(mirador_logs_recues_total{job="mirador"}[5m]))`, t); ok {
		l.Chiffres = append(l.Chiffres, chiffreSante{nombreCourt(v) + " /min", "lignes"})
	}
	l.Chiffres = append(l.Chiffres, chiffreSante{tailleLisible(e.Logs), "taille"})
	srv(l)

	// Règles et alertes.
	srv(c.ligneRegles(t))
	// Notifications.
	srv(c.ligneNotifications(ctx, t))
	// Sauvegarde.
	srv(c.ligneSauvegarde(ctx, t))
	// Certificats des collecteurs.
	srv(c.ligneCertificats(ctx, t))
	// Sondes du serveur.
	srv(ligneSondes(sondes, lues, essais.ServeurLocal, "Sondes du serveur", "depuis le VPS : sites publics, ping, DNS"))
	// Page de statut publique.
	st := lireStatut(ctx, c.db)
	l = ligneSante{Etat: "eteint", Icone: "mur", Nom: "Page de statut publique", Sous: "désactivée · aucune adresse publique", Lien: "/reglages/statut", LienTexte: "Réglages"}
	if st.Actif && st.Jeton != "" {
		l.Etat, l.Sous = "ok", accorderTexte("publiée · "+strconv.Itoa(len(st.Applications))+" application(s) montrée(s)")
	}
	srv(l)
	return out
}

func (c *Console) ligneMajServeur(ctx context.Context, t time.Time) ligneSante {
	l := ligneSante{Etat: "ok", Icone: "telechargement", Nom: "mirador-maj-serveur", Root: true, Lien: "/reglages/serveur", LienTexte: "Mise à jour",
		Sous: "VPS · lancé seulement à la demande (root : il pilote Docker) · n'ouvre aucun port · vérifie la signature"}
	v := c.vueMajServeur(ctx, t, false)
	switch {
	case !v.Active:
		l.Etat, l.Sous = "eteint", "pas installé : mise à jour du serveur à la main (docker pull)"
		return l
	case !v.Installe:
		l.Etat, l.Alerte = "attention", "service du VPS absent (dossier d'échange introuvable)"
	case v.Etat != nil && v.Etat.Etat == "echec":
		l.Etat, l.Alerte = "attention", premierTexte(v.Etat.Message, "dernière mise à jour en échec")
	case v.Service != nil && len(v.Service.Problemes) > 0:
		l.Etat, l.Alerte = "attention", v.Service.Problemes[0]
	}
	if v.Service != nil {
		l.Version = versionCourte(v.Service.Version)
		if v.Service.Le > 0 {
			l.Chiffres = append(l.Chiffres, chiffreSante{dureeLisible(t.Sub(time.Unix(v.Service.Le, 0))), "dernier contrôle"})
		}
	}
	switch {
	case v.Proposer && v.Derniere != "":
		l.Chiffres = append(l.Chiffres, chiffreSante{v.Derniere, "disponible"})
	case v.Derniere != "":
		l.Chiffres = append(l.Chiffres, chiffreSante{"à jour", "serveur"})
	}
	return l
}

func (c *Console) ligneRegles(t time.Time) ligneSante {
	l := ligneSante{Etat: "ok", Icone: "regles", Nom: "Règles et alertes", Lien: "/regles", LienTexte: "Règles"}
	if c.opt.Alertes == nil {
		l.Etat, l.Sous = "eteint", "moteur d'alertes arrêté"
		return l
	}
	s := c.opt.Alertes.Sante()
	enErreur, enAlerte := 0, 0
	for _, r := range s {
		if r.Erreur != "" {
			enErreur++
		}
		enAlerte += r.EnAlerte
	}
	l.Sous = accorderTexte(strconv.Itoa(len(s)) + " règle(s) chargée(s)")
	dern := c.opt.Alertes.DerniereEvaluationOK()
	switch {
	case dern.IsZero() && t.Sub(c.opt.Debut) > 5*time.Minute:
		l.Etat, l.Alerte = "erreur", "aucune évaluation réussie depuis le démarrage"
	case !dern.IsZero() && t.Sub(dern) > 5*time.Minute:
		l.Etat, l.Alerte = "erreur", "plus d'évaluation réussie depuis "+dureeLisible(t.Sub(dern))
	case enErreur > 0:
		l.Etat, l.Alerte = "attention", accorderTexte(strconv.Itoa(enErreur)+" règle(s) en erreur")
	}
	if !dern.IsZero() {
		l.Chiffres = append(l.Chiffres, chiffreSante{dureeLisible(t.Sub(dern)), "dernier tour"})
	}
	l.Chiffres = append(l.Chiffres, chiffreSante{strconv.Itoa(enErreur), "en erreur"}, chiffreSante{strconv.Itoa(enAlerte), "en alerte"})
	return l
}

func (c *Console) ligneNotifications(ctx context.Context, t time.Time) ligneSante {
	l := ligneSante{Etat: "ok", Icone: "courrier", Nom: "Notifications", Lien: "/reglages/notifications", LienTexte: "Canaux"}
	if c.opt.Postier == nil {
		l.Etat, l.Sous = "eteint", "envoi des notifications arrêté"
		return l
	}
	canaux, _, _ := c.opt.Postier.Etat()
	var parts, pannes []string
	for _, k := range canaux {
		if k.EnPanne {
			txt := majusculeDebut(k.Nom) + " en panne"
			if k.Erreur != "" {
				txt += " (" + k.Erreur + ")"
			}
			pannes = append(pannes, txt)
			if k.Role == "principal" {
				l.Etat = "erreur"
			}
		} else {
			parts = append(parts, majusculeDebut(k.Nom)+" OK")
		}
	}
	switch {
	case len(canaux) == 0:
		l.Etat, l.Alerte = "attention", "aucun canal : les alertes ne partent nulle part"
	case len(pannes) > 0:
		if l.Etat != "erreur" {
			l.Etat = "attention"
		}
		l.Alerte = strings.Join(pannes, " · ")
	}
	l.Sous = strings.Join(parts, " · ")
	if c.opt.Aiguilleur != nil && c.opt.Aiguilleur.Mode() == notif.Ombre {
		l.Sous = strings.TrimPrefix(l.Sous+" · mode ombre : aucune notification envoyée", " · ")
	}
	// Envois des dernières 24 h (table envois, heures en millisecondes).
	ok, echec := 0, 0
	if rows, err := c.db.QueryContext(ctx, `SELECT statut, COUNT(*) FROM envois WHERE le >= ? AND type <> 'test' GROUP BY statut`, t.Add(-24*time.Hour).UnixMilli()); err == nil {
		for rows.Next() {
			var st string
			var n int
			if rows.Scan(&st, &n) == nil {
				switch st {
				case "ok":
					ok = n
				case "echec":
					echec = n
				}
			}
		}
		_ = rows.Close()
	}
	l.Chiffres = append(l.Chiffres, chiffreSante{strconv.Itoa(ok), "envoyées 24 h"}, chiffreSante{strconv.Itoa(echec), "échecs 24 h"})
	return l
}

func (c *Console) ligneSauvegarde(ctx context.Context, t time.Time) ligneSante {
	l := ligneSante{Etat: "ok", Icone: "sauvegarde", Nom: "Sauvegarde du serveur", Lien: "/reglages/sauvegardes", LienTexte: "Sauvegardes"}
	r, _ := sauvegarde.LireReglages(ctx, c.db)
	der, trouvee, _ := sauvegarde.DerniereReussie(ctx, c.db)
	switch {
	case !trouvee:
		l.Etat, l.Alerte = "erreur", "aucune sauvegarde réussie : la base du serveur n'est pas sauvegardée"
	case t.Sub(time.Unix(der.Fin, 0)) > 48*time.Hour:
		l.Etat, l.Alerte = "attention", "dernière réussite il y a "+dureeLisible(t.Sub(time.Unix(der.Fin, 0)))
	}
	if trouvee {
		l.Sous = "dernière réussie il y a " + dureeLisible(t.Sub(time.Unix(der.Fin, 0)))
		l.Chiffres = append(l.Chiffres, chiffreSante{tailleLisible(der.Taille), "taille"})
	}
	if r.Actif {
		l.Sous = strings.TrimPrefix(l.Sous+" · chaque jour", " · ")
		if p := prochaineSauvegarde(r.Heure, t); !p.IsZero() {
			l.Chiffres = append(l.Chiffres, chiffreSante{"dans " + dureeLisible(p.Sub(t)), "prochaine"})
		}
	} else {
		l.Sous = strings.TrimPrefix(l.Sous+" · pas de sauvegarde planifiée", " · ")
		if l.Etat == "ok" {
			l.Etat, l.Alerte = "attention", "sauvegarde quotidienne désactivée"
		}
	}
	return l
}

func (c *Console) ligneCertificats(ctx context.Context, t time.Time) ligneSante {
	l := ligneSante{Etat: "eteint", Icone: "cadenas", Nom: "Certificats (Let's Encrypt)", Sous: "aucun certificat demandé (syslog TLS des collecteurs)",
		Lien: "/reglages/certificats", LienTexte: "Certificats"}
	cs, _ := certsyslog.ListerCollecteurs(ctx, c.db)
	jours := math.MaxInt
	var noms []string
	for _, k := range cs {
		if k.Etat.Fin == 0 && !k.Etat.CSR {
			continue
		}
		noms = append(noms, premierTexte(k.Etat.Nom, k.Nom))
		if k.Etat.Erreur != "" && l.Alerte == "" {
			l.Alerte = k.Nom + " : " + k.Etat.Erreur
		}
		if k.Etat.Fin > 0 {
			jours = min(jours, int(time.Unix(k.Etat.Fin, 0).Sub(t).Hours()/24))
		}
	}
	if len(noms) == 0 {
		return l
	}
	l.Etat, l.Sous = "ok", strings.Join(noms, " · ")+" · renouvellement automatique"
	switch {
	case jours != math.MaxInt && jours <= 7:
		l.Etat, l.Alerte = "erreur", "expire dans "+strconv.Itoa(jours)+" j"
	case jours != math.MaxInt && jours <= 30, l.Alerte != "":
		l.Etat = "attention"
	}
	if jours != math.MaxInt {
		l.Chiffres = append(l.Chiffres, chiffreSante{strconv.Itoa(jours) + " j", "expire dans"})
	}
	return l
}

// ligneSondes : sondes faites par depuis (serveur ou un collecteur).
func ligneSondes(sondes []essais.Sonde, lues map[string]map[string]alertes.Echantillon, depuis, nom, sous string) ligneSante {
	l := ligneSante{Etat: "eteint", Icone: "globe", Nom: nom, Sous: "aucune sonde", Lien: "/sondes", LienTexte: "Sondes"}
	n, echecs := 0, 0
	for _, s := range sondes {
		if s.Depuis != depuis {
			continue
		}
		n++
		if _, classe, _ := etatSondeLue(lues, s); classe == "erreur" {
			echecs++
		}
	}
	if n == 0 {
		return l
	}
	l.Etat, l.Sous = "ok", sous
	if echecs > 0 {
		l.Etat, l.Alerte = "attention", accorderTexte(strconv.Itoa(echecs)+" sonde(s) en échec")
	}
	l.Chiffres = []chiffreSante{{strconv.Itoa(n), "sondes"}, {strconv.Itoa(echecs), "en échec"}}
	return l
}

// --- Collecteurs ---------------------------------------------------------------

func (c *Console) carteCollecteurSante(ctx context.Context, m flotte.InfoMachine, sondes []essais.Sonde, lues map[string]map[string]alertes.Echantillon, t time.Time) carteSante {
	k := carteSante{Titre: "Collecteur · " + m.Nom, Icone: "reseau"}
	k.Sous = premierTexte(m.Site, "sans site") + " · aucun service root"
	page := "/machines/" + url.PathEscape(m.Nom)
	lan := func(l ligneSante) { l.Zone = "lan"; k.Lignes = append(k.Lignes, l) }
	inst := `instance=` + strconv.Quote(m.Nom)

	// Le collecteur lui-même.
	l := ligneSante{Etat: "ok", Icone: "reseau", Nom: "mirador-collector", Version: versionCourte(m.Version), Lien: page, LienTexte: "Page",
		Sous: "écoute seulement sur le LAN · mise à jour signée"}
	if m.VuLe == 0 || t.Sub(time.Unix(m.VuLe, 0)) > delaiMuette {
		l.Etat, l.Alerte = "erreur", "muet : plus de données depuis "+dureeLisible(t.Sub(time.Unix(m.VuLe, 0)))
	}
	if m.VuLe > 0 {
		l.Chiffres = append(l.Chiffres, chiffreSante{dureeLisible(t.Sub(time.Unix(m.VuLe, 0))), "dernier envoi"})
	}
	if v, ok := c.une(ctx, `max(mirador_agent_resident_memory_bytes{`+inst+`})`, t); ok {
		l.Chiffres = append(l.Chiffres, chiffreSante{tailleLisible(int64(v)), "mémoire"})
	}
	if v, ok := c.une(ctx, `100 * (1 - avg(rate(node_cpu_seconds_total{mode="idle", `+inst+`}[5m])))`, t); ok {
		l.Chiffres = append(l.Chiffres, chiffreSante{strconv.FormatFloat(v, 'f', 0, 64) + " %", "CPU machine"})
	}
	lan(l)

	// Compagnon des processus (0.6.13).
	l = ligneSante{Etat: "ok", Icone: "cpu", Nom: "Compagnon des processus", Lien: page, LienTexte: "Page",
		Sous: "mirador-collector-processus · sans réseau · voit tous les processus de la machine"}
	if v, ok := c.une(ctx, `max(last_over_time(mirador_collecteur_compagnon{`+inst+`}[10m]))`, t); !ok {
		l.Etat, l.Sous = "eteint", "arrive avec le collecteur 0.6.13 (puis relancer une fois le script d'installation)"
	} else if v == 0 {
		l.Etat, l.Alerte = "attention", "pas installé : relancer une fois le script d'installation du collecteur"
	}
	lan(l)

	// Syslog, SNMP, UniFi : vue de l'onglet « Collecte ».
	eqs, _ := flotte.EquipementsDe(ctx, c.db, m.ID)
	sys := c.syslogAvecAvancement(ctx, m, t)
	snmp := c.snmpDe(ctx, m)
	v := c.collecteDe(ctx, m, eqs, sys, snmp, "", "", "", t)

	l = ligneSante{Etat: etatDeClasse(v.SyslogClasse), Icone: "cadenas", Nom: "Syslog TLS", Lien: page + "#syslog", LienTexte: "Voir",
		Sous: strings.TrimPrefix(strings.TrimSpace(v.SyslogEtat+" · "+v.SyslogDetail), "· ")}
	if l.Probleme() {
		l.Alerte = v.SyslogEtat
	}
	if l.Etat != "eteint" {
		l.Chiffres = append(l.Chiffres, chiffreSante{strconv.Itoa(len(v.Emetteurs)), "émetteurs"}, chiffreSante{strconv.Itoa(v.Refus24h), "refus 24 h"})
	}
	lan(l)

	l = ligneSante{Etat: "eteint", Icone: "stockage", Nom: "SNMP v3", Sous: "aucun relevé SNMP", Lien: page + "#sondes", LienTexte: "Voir"}
	if v.SNMPActifs > 0 {
		l.Etat, l.Sous = "ok", "relevé en lecture seule, authentifié et chiffré"
		ko := 0
		for _, x := range v.Lignes {
			if x.SNMP != nil && x.SNMP.Classe == "erreur" {
				ko++
			}
		}
		if ko > 0 {
			l.Etat, l.Alerte = "attention", accorderTexte(strconv.Itoa(ko)+" équipement(s) sans réponse")
		}
		l.Chiffres = []chiffreSante{{strconv.Itoa(v.SNMPActifs), "équipements"}, {strconv.Itoa(ko), "sans réponse"}}
	}
	lan(l)

	l = ligneSante{Etat: etatDeClasse(v.UniFi.Classe), Icone: "wifi", Nom: "Contrôleur UniFi", Lien: page + "#unifi", LienTexte: "Voir",
		Sous: premierTexte(strings.TrimSpace(v.UniFi.Texte+" · "+v.UniFi.Detail), "aucun contrôleur")}
	l.Sous = strings.Trim(l.Sous, " ·")
	if v.UniFi.Texte == "" {
		l.Etat, l.Sous = "eteint", "aucun contrôleur UniFi"
	}
	if l.Probleme() {
		l.Alerte = v.UniFi.Texte
	}
	lan(l)

	lan(ligneSondes(sondes, lues, m.Nom, "Sondes du LAN", "faites depuis le collecteur"))
	return k
}

// --- Agents et envois directs ------------------------------------------------------

func (c *Console) carteAgentsSante(ctx context.Context, machines []flotte.InfoMachine, t time.Time) carteSante {
	k := carteSante{Titre: "Agents et envois des équipements", Icone: "ecran"}
	cible, _ := flotte.CibleGlobale(ctx, c.db)

	// Agents.
	var avecAgent []flotte.InfoMachine
	for _, m := range machines {
		if m.Type == "agent" && m.Statut == "approuvee" && m.Version != "" {
			avecAgent = append(avecAgent, m)
		}
	}
	l := ligneSante{Etat: "ok", Icone: "ecran", Nom: "mirador-agent", Lien: "/mises-a-jour", LienTexte: "Mises à jour", Zone: "lan"}
	enLigne, aJour, retard, echec := 0, 0, 0, 0
	var muettes, enEchec []string
	for _, m := range avecAgent {
		if t.Sub(time.Unix(m.VuLe, 0)) <= delaiMuette {
			enLigne++
		} else {
			muettes = append(muettes, m.Nom)
		}
		cm := cible
		if m.VersionCible != "" {
			cm = m.VersionCible
		}
		switch etatMaj(m, cm) {
		case majAJour:
			aJour++
		case majEchec:
			echec++
			enEchec = append(enEchec, m.Nom)
		default:
			retard++
		}
	}
	switch {
	case len(avecAgent) == 0:
		l.Etat, l.Sous = "eteint", "aucune machine avec agent"
	case aJour == len(avecAgent):
		l.Sous = "tous à jour" + texteSi(cible != "", " ("+cible+")") + " · mise à jour signée, par machine"
	default:
		l.Sous = strconv.Itoa(aJour) + " sur " + strconv.Itoa(len(avecAgent)) + " à jour" + texteSi(cible != "", " ("+cible+")")
	}
	switch {
	case echec > 0:
		l.Etat, l.Alerte = "attention", "mise à jour en échec : "+resumeNoms(enEchec)
	case len(muettes) > 0:
		l.Etat, l.Alerte = "attention", accorderTexte(strconv.Itoa(len(muettes))+" muette(s) : ")+resumeNoms(muettes)
	}
	l.Chiffres = []chiffreSante{{strconv.Itoa(enLigne), "en ligne"}, {strconv.Itoa(len(avecAgent)), "agents"}, {strconv.Itoa(retard), "en retard"}}
	k.Lignes = append(k.Lignes, l)

	// Assistant root des agents.
	l = ligneSante{Etat: "eteint", Icone: "cle", Nom: "Assistant root des agents", Root: true, Zone: "lan", Lien: "/machines", LienTexte: "Machines",
		Sous: "aucun agent n'en envoie"}
	ages := c.parEtiquette(ctx, `max by (instance) (mirador_agent_root_age_seconds)`, t, "instance")
	if len(ages) > 0 {
		erreurs := c.parEtiquette(ctx, `max by (instance) (mirador_agent_root_errors)`, t, "instance")
		ajour := c.parEtiquette(ctx, `max by (instance) (mirador_agent_root_a_jour)`, t, "instance")
		vieux, nbErr, nbAJour := 0.0, 0, 0
		var anciens, fautifs []string
		for inst, a := range ages {
			vieux = math.Max(vieux, a)
			if a > 45*60 { // relevé toutes les 15 min : 3 tours manqués
				anciens = append(anciens, inst)
			}
			if erreurs[inst] > 0 {
				nbErr++
				fautifs = append(fautifs, inst)
			}
			if ajour[inst] == 1 {
				nbAJour++
			}
		}
		sort.Strings(anciens)
		sort.Strings(fautifs)
		l.Etat = "ok"
		l.Sous = "mirador-agent-root · toutes les 15 min et à la demande · sans réseau · pare-feu, SMART, certificats réservés à root"
		switch {
		case len(anciens) > 0:
			l.Etat, l.Alerte = "attention", "relevé ancien : "+resumeNoms(anciens)
		case nbErr > 0:
			l.Etat, l.Alerte = "attention", "erreurs : "+resumeNoms(fautifs)
		case nbAJour < len(ages):
			l.Etat, l.Alerte = "attention", accorderTexte(strconv.Itoa(len(ages)-nbAJour)+" assistant(s) à mettre à jour")
		}
		l.Chiffres = []chiffreSante{{dureeLisible(time.Duration(vieux) * time.Second), "plus ancien relevé"},
			{strconv.Itoa(nbErr), "en erreur"}, {strconv.Itoa(nbAJour) + " / " + strconv.Itoa(len(ages)), "à jour"}}
	}
	k.Lignes = append(k.Lignes, l)

	// Envois directs (Telegraf d'OPNsense…) : machine sans agent avec son token.
	for _, m := range machines {
		if m.Type != "agent" || m.Statut != "approuvee" || m.Version != "" || m.TokensActifs == 0 {
			continue
		}
		l := ligneSante{Etat: "ok", Icone: "bouclier", Nom: "Envoi direct · " + m.Nom, Zone: "lan",
			Lien: "/machines/" + url.PathEscape(m.Nom), LienTexte: "Voir", Sous: "token propre (Telegraf ou autre)"}
		if m.IPsAutorisees != "" {
			l.Sous += " · adresses autorisées " + m.IPsAutorisees
		}
		var envoi int64
		var refus bool
		if c.opt.Ingestion != nil {
			envoi, _ = c.opt.Ingestion.DerniereReception(m.ID)
			if r, ok := c.opt.Ingestion.DernierRefus(m.ID); ok && r.T > envoi {
				refus = true
				l.Etat, l.Alerte = "erreur", "envoi refusé : "+r.Motif+texteSi(r.Adresse != "", " (depuis "+r.Adresse+")")
			}
		}
		switch {
		case envoi > 0:
			l.Chiffres = append(l.Chiffres, chiffreSante{dureeLisible(t.Sub(time.Unix(envoi, 0))), "dernier envoi"})
			if !refus && t.Sub(time.Unix(envoi, 0)) > 10*time.Minute {
				l.Etat, l.Alerte = "attention", "plus rien reçu depuis "+dureeLisible(t.Sub(time.Unix(envoi, 0)))
			}
		case !refus && t.Sub(c.opt.Debut) > 10*time.Minute:
			l.Etat, l.Alerte = "attention", "rien reçu avec ce token depuis le démarrage du serveur"
		}
		k.Lignes = append(k.Lignes, l)
	}
	return k
}

// --- Petits outils ---------------------------------------------------------------

// une : valeur d'une requête à un seul résultat.
func (c *Console) une(ctx context.Context, expr string, t time.Time) (float64, bool) {
	ech, err := c.instant(ctx, expr, t)
	if err != nil || len(ech) != 1 || math.IsNaN(ech[0].Valeur) || math.IsInf(ech[0].Valeur, 0) {
		return 0, false
	}
	return ech[0].Valeur, true
}

func (c *Console) adresseInterface(ctx context.Context) string {
	a := lireInterface(ctx, c.db).Ingestion
	if u, err := url.Parse(a); err == nil && u.Host != "" {
		return u.Host
	}
	return ""
}

func etatDeClasse(classe string) string {
	switch classe {
	case "ok", "attention", "erreur":
		return classe
	case "", "gris":
		return "eteint"
	}
	return "ok"
}

func versionCourte(v string) string {
	if i := strings.IndexByte(v, ' '); i > 0 {
		return v[:i]
	}
	return v
}

func nombreCourt(v float64) string {
	switch {
	case v >= 1e6:
		return strconv.FormatFloat(v/1e6, 'f', 1, 64) + " M"
	case v >= 10000:
		return strconv.FormatFloat(v/1000, 'f', 0, 64) + " k"
	case v >= 10 || v == math.Trunc(v):
		return strconv.FormatFloat(v, 'f', 0, 64)
	}
	return strconv.FormatFloat(v, 'f', 1, 64)
}

func premierTexte(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	return b
}

func texteSi(c bool, s string) string {
	if c {
		return s
	}
	return ""
}

func nomSimple(n string) string { return strings.TrimPrefix(n, "Envoi direct · ") }

func eteints(ls []ligneSante) int {
	n := 0
	for _, l := range ls {
		if l.Etat == "eteint" {
			n++
		}
	}
	return n
}

func classeCarte(k carteSante) string {
	cl := ""
	for _, l := range k.Lignes {
		if l.Etat == "erreur" {
			return "erreur"
		}
		if l.Etat == "attention" {
			cl = "attention"
		}
	}
	return cl
}

func resumeCarte(k carteSante) string {
	e := eteints(k.Lignes)
	var p []string
	if k.NbProblemes > 0 {
		p = append(p, accorderTexte(strconv.Itoa(k.NbProblemes)+" à regarder"))
	}
	if e > 0 {
		p = append(p, accorderTexte(strconv.Itoa(e)+" pas utilisé(s)"))
	}
	if len(p) == 0 {
		return "tout va bien"
	}
	return strings.Join(p, " · ")
}
