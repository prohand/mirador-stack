// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"

	"mirador-stack/internal/agent/config"
	"mirador-stack/internal/server/flotte"
)

// Onglet « Collecte » d'un collecteur, nouvelle présentation (maquette
// validée le 22/11, SPEC § 119) : quatre cases d'état, un tableau
// « équipement × méthode », puis une carte par méthode où chaque sonde et
// chaque émetteur est une ligne (ajout, retrait) au lieu d'une zone de texte.
//
// Coût : 6 requêtes instantanées au plus par affichage (état SNMP, UniFi,
// sondes de certificats, lignes syslog, refus), passées par le cache de
// requêtes de la console ; aucune donnée gardée en mémoire en plus.

// vueCollecte : données de l'onglet.
type vueCollecte struct {
	Lignes []ligneCollecte // une par équipement rattaché
	// Cases d'état du haut.
	SyslogEtat, SyslogClasse, SyslogDetail string
	CertTexte, CertDetail, CertClasse      string
	Refus24h                               int
	// Cartes par méthode.
	Sondes     []sondeCert
	Emetteurs  []emetteurVue
	UniFi      cellule // état du contrôleur (pastille de la carte)
	AvecDirect bool    // un équipement au moins envoie lui-même (colonne « Envoi direct »)
	SNMPActifs int
	// Pour : équipement choisi d'avance (lien « + » du tableau) ; Modif :
	// relevé SNMP à modifier (lien « Modifier »), formulaire prérempli.
	Pour, Ajout     string // Ajout : carte dont le formulaire d'ajout s'ouvre (snmp, unifi)
	Modif           *config.CibleSNMP
	ModifIntervalle string // « 30s » : intervalle du relevé modifié
	// EmpreinteCourte : empreinte épinglée du contrôleur UniFi, abrégée.
	EmpreinteCourte string
	// Machines qui peuvent recevoir un relevé UniFi : toutes s'il n'y en a
	// pas encore, sinon celle déjà réglée (un contrôleur par collecteur).
	UniFiLibre bool
}

// ligneCollecte : un équipement et l'état de chaque méthode.
type ligneCollecte struct {
	Nom, Detail string // nom, « adresse · type »
	Icone       string // stockage (NAS), wifi (UniFi), bouclier (pare-feu), reseau
	Statut      string // approuvee, en_attente…
	VuLe        int64
	SNMP, UniFi *cellule
	Certs       []cellule
	Direct      *cellule // envoi direct au serveur avec son token (Telegraf)
	Syslog      *cellule
}

// cellule : une méthode pour un équipement (point de couleur, texte, détail).
type cellule struct {
	Classe, Texte, Detail string // Classe : ok, attention, erreur, gris
}

// sondeCert : une sonde de certificat (carte « Sondes de certificats »).
type sondeCert struct {
	Adresse, Machine, Nom string
	Etat                  cellule
}

// emetteurVue : un émetteur syslog (carte « Syslog TLS »).
type emetteurVue struct {
	Adresse, Machine string
	PareFeu          bool // pare-feu OPNsense : son « filterlog » est jeté et compté (ou gardé)
}

// instancesRe : sélecteur « instance=~"a|b" » sûr (noms échappés).
func instancesRe(noms []string) string {
	q := make([]string, len(noms))
	for i, n := range noms {
		q[i] = regexp.QuoteMeta(n)
	}
	return `instance=~` + strconv.Quote(strings.Join(q, "|"))
}

// parEtiquette : dernière valeur de chaque série, rangée par les étiquettes
// demandées (instance, chemin d'une sonde…).
func (c *Console) parEtiquette(ctx context.Context, expr string, t time.Time, cles ...string) map[string]float64 {
	out := map[string]float64{}
	ech, err := c.instant(ctx, expr, t)
	if err != nil {
		return out
	}
	for _, e := range ech {
		k := make([]string, len(cles))
		for i, cle := range cles {
			k[i] = e.Etiquettes.Get(cle)
		}
		out[strings.Join(k, "\x00")] = e.Valeur
	}
	return out
}

// collecteDe : tableau et cartes de l'onglet « Collecte ».
func (c *Console) collecteDe(ctx context.Context, col flotte.InfoMachine, eqs []flotte.Equipement, s vueSyslog, n vueSNMP,
	pour, ajout, modif string, t time.Time) vueCollecte {
	v := vueCollecte{Pour: pour, Ajout: ajout, UniFiLibre: n.UniFi == nil, SNMPActifs: len(n.Cibles)}
	noms := make([]string, 0, len(eqs))
	for _, e := range eqs {
		noms = append(noms, e.Nom)
	}
	sel := instancesRe(noms)
	var upSNMP, upUniFi, finCert, succesCert map[string]float64
	if len(noms) > 0 {
		upSNMP = c.parEtiquette(ctx, `last_over_time(up{job="snmp", `+sel+`}[10m])`, t, "instance")
		upUniFi = c.parEtiquette(ctx, `last_over_time(up{job="unifi", `+sel+`}[10m])`, t, "instance")
		finCert = c.parEtiquette(ctx, `last_over_time(ssl_cert_not_after_timestamp_seconds{job="certs", `+sel+`}[15m])`, t, "instance", "path")
		succesCert = c.parEtiquette(ctx, `last_over_time(probe_success{job="certs", `+sel+`}[15m])`, t, "instance", "path")
	}
	inst := `instance=` + strconv.Quote(col.Nom)
	lignesH := c.parEtiquette(ctx, `sum by (equipement) (increase(mirador_collecteur_syslog_lignes_total{`+inst+`}[1h]))`, t, "equipement")
	// Équipement discret (un NAS envoie parfois quelques lignes par jour) :
	// « reçu » aussi sur 24 h (retour du 22/11 : syslog du NAS « rien reçu »
	// alors que sa connexion TLS était ouverte).
	lignesJ := c.parEtiquette(ctx, `sum by (equipement) (increase(mirador_collecteur_syslog_lignes_total{`+inst+`}[24h]))`, t, "equipement")
	if r := c.parEtiquette(ctx, `sum(increase(mirador_collecteur_syslog_refus_total{`+inst+`, motif!="filterlog"}[24h]))`, t); len(r) > 0 {
		for _, x := range r {
			v.Refus24h = int(math.Round(x))
		}
	}

	// Sondes de certificats : état lu par adresse.
	etatSonde := func(machine, adresse string) cellule {
		k := machine + "\x00" + adresse
		fin, aFin := finCert[k]
		ok, vu := succesCert[k]
		switch {
		case vu && ok == 0:
			return cellule{Classe: "erreur", Texte: "injoignable"}
		case aFin:
			j := int(math.Floor(time.Unix(int64(fin), 0).Sub(t).Hours() / 24))
			cl := "ok"
			if j < 7 {
				cl = "erreur"
			} else if j < 21 {
				cl = "attention"
			}
			if j < 0 {
				return cellule{Classe: "erreur", Texte: "expiré"}
			}
			return cellule{Classe: cl, Texte: strconv.Itoa(j) + " j"}
		}
		return cellule{Classe: "gris", Texte: "en attente"}
	}
	if cfg, err := flotte.ConfigEffective(ctx, c.db, col.ID); err == nil && cfg.Collecteur != nil {
		for _, x := range cfg.Collecteur.Sondes {
			v.Sondes = append(v.Sondes, sondeCert{Adresse: x.Adresse, Machine: x.Machine, Nom: x.Nom, Etat: etatSonde(x.Machine, x.Adresse)})
		}
		if sy := cfg.Collecteur.Syslog; sy != nil {
			pf := c.pareFeux(ctx, sy.Emetteurs, t)
			for _, e := range sy.Emetteurs {
				v.Emetteurs = append(v.Emetteurs, emetteurVue{Adresse: e.Adresse, Machine: e.Machine, PareFeu: pf[e.Machine]})
			}
		}
	}

	// UniFi : état du contrôleur.
	if n.UniFi != nil {
		v.UniFi = etatUp(upUniFi, n.UniFi.Machine, "relevé", "en échec")
	}

	// Une ligne par équipement.
	for _, e := range eqs {
		l := ligneCollecte{Nom: e.Nom, Statut: e.Statut, VuLe: e.VuLe, Icone: "reseau"}
		var adresse string
		for _, x := range n.Cibles {
			if x.Machine == e.Nom {
				ce := etatUp(upSNMP, e.Nom, x.Prereglage, "en échec")
				iv := time.Duration(x.Intervalle)
				if iv == 0 {
					iv = config.IntervalleSNMPDefaut
				}
				ce.Detail = dureeCourte(iv)
				l.SNMP, adresse = &ce, x.Adresse
				if x.Prereglage == "synology" {
					l.Icone = "stockage"
				}
			}
		}
		if n.UniFi != nil && n.UniFi.Machine == e.Nom {
			ce := v.UniFi
			if n.UniFi.Empreinte != "" {
				ce.Detail = "certificat épinglé"
			}
			l.UniFi, l.Icone = &ce, "wifi"
		}
		for _, x := range v.Sondes {
			if x.Machine == e.Nom {
				ce := x.Etat
				ce.Texte, ce.Detail = ":"+portDe(x.Adresse), x.Etat.Texte
				if ce.Classe == "ok" || ce.Classe == "attention" {
					ce.Detail = "expire dans " + x.Etat.Texte
				}
				l.Certs = append(l.Certs, ce)
			}
		}
		for _, x := range v.Emetteurs {
			if x.Machine == e.Nom {
				ce := cellule{Classe: "gris", Texte: "rien reçu", Detail: "depuis 24 h"}
				switch nb, nj := lignesH[e.Nom], lignesJ[e.Nom]; {
				case nb >= 1:
					ce = cellule{Classe: "ok", Texte: "reçu", Detail: fmt.Sprintf("%.0f lignes/h", nb)}
				case nj >= 0.5:
					ce = cellule{Classe: "ok", Texte: "reçu", Detail: accorderTexte(fmt.Sprintf("%.0f ligne(s) en 24 h", math.Max(1, math.Round(nj))))}
				}
				if x.PareFeu {
					if s.GarderFilterlog {
						ce.Detail += " · filterlog gardé"
					} else {
						ce.Detail += " · filterlog compté"
					}
				}
				l.Syslog = &ce
				if x.PareFeu {
					l.Icone = "bouclier"
				}
				if adresse == "" {
					adresse = x.Adresse
				}
				break
			}
		}
		// Envoi direct (Telegraf d'OPNsense…) : l'équipement envoie lui-même
		// au serveur avec son token, sans passer par le collecteur (retour
		// du 22/11 : « on ne voit pas Telegraf ici »).
		if ce, ok := c.celluleDirect(ctx, e, t); ok {
			l.Direct, v.AvecDirect = &ce, true
		}
		var d []string
		if adresse != "" {
			d = append(d, adresse)
		}
		for _, x := range []string{e.Env, e.Site} {
			if x != "" {
				d = append(d, x)
			}
		}
		l.Detail = strings.Join(d, " · ")
		v.Lignes = append(v.Lignes, l)
	}

	// Relevé SNMP à modifier (lien « Modifier ») : formulaire prérempli.
	for i := range n.Cibles {
		if n.Cibles[i].Machine == modif {
			x := n.Cibles[i]
			v.Modif = &x
			if x.Intervalle > 0 {
				v.ModifIntervalle = time.Duration(x.Intervalle).String()
			}
		}
	}

	if n.UniFi != nil && len(n.UniFi.Empreinte) > 12 {
		e := n.UniFi.Empreinte
		v.EmpreinteCourte = e[:8] + "…" + e[len(e)-4:]
	}

	// Cases d'état du haut.
	v.SyslogEtat, v.SyslogClasse = s.Avancement.Resume, s.Avancement.Classe
	if !s.Regle {
		v.SyslogEtat, v.SyslogClasse = "Pas réglé", ""
	}
	if s.Regle {
		total := 0.0
		for _, x := range lignesH {
			total += x
		}
		v.SyslogDetail = fmt.Sprintf("%s · %d %s · %.0f lignes/h", s.Ecoute, len(v.Emetteurs), accorder(len(v.Emetteurs), "émetteur(s)"), total)
	} else {
		v.SyslogDetail = "réception des journaux des équipements, en TLS"
	}
	v.CertTexte, v.CertDetail = "—", "pas encore obtenu"
	if s.Cert.Fin > 0 {
		j := int(math.Floor(time.Unix(s.Cert.Fin, 0).Sub(t).Hours() / 24))
		v.CertTexte, v.CertDetail, v.CertClasse = strconv.Itoa(j)+" jours", s.Cert.Nom+" · renouvelé seul", "ok"
		if j < 0 {
			v.CertTexte, v.CertClasse = "Expiré", "erreur"
		} else if j < 21 {
			v.CertClasse = "attention"
		}
	}
	return v
}

// etatUp : état d'un relevé d'après « up » (1 : ok, 0 : échec, absent : en attente).
func etatUp(up map[string]float64, instance, texteOK, texteKO string) cellule {
	x, vu := up[instance]
	switch {
	case !vu:
		return cellule{Classe: "gris", Texte: "en attente"}
	case x >= 1:
		return cellule{Classe: "ok", Texte: texteOK}
	}
	return cellule{Classe: "erreur", Texte: texteKO}
}

// portDe : port d'une adresse « hôte:port » (vide sinon).
func portDe(adresse string) string {
	if i := strings.LastIndexByte(adresse, ':'); i >= 0 {
		return adresse[i+1:]
	}
	return adresse
}

// pareFeux : équipements émetteurs qui ont les mesures d'un pare-feu
// OPNsense (pf_entries). Une requête pour tous.
func (c *Console) pareFeux(ctx context.Context, ems []config.Emetteur, t time.Time) map[string]bool {
	out := map[string]bool{}
	var noms []string
	for _, e := range ems {
		noms = append(noms, e.Machine)
	}
	if len(noms) == 0 {
		return out
	}
	q := make([]string, len(noms))
	for i, n := range noms {
		q[i] = regexp.QuoteMeta(n)
	}
	re := strconv.Quote(strings.Join(q, "|"))
	ech, err := c.instant(ctx, `count by (instance, host) (pf_entries{instance=~`+re+`} or pf_entries{host=~`+re+`})`, t)
	if err != nil {
		return out
	}
	for _, e := range ech {
		for _, n := range noms {
			if e.Etiquettes.Get("instance") == n || e.Etiquettes.Get("host") == n {
				out[n] = true
			}
		}
	}
	return out
}

// --- Ajout et retrait ligne par ligne -----------------------------------------

// modifierSyslog : change le bloc syslog rangé (émetteurs) sans toucher au
// reste ; contrôlé comme à l'enregistrement complet.
func (c *Console) modifierSyslog(q *req, m flotte.InfoMachine, changer func(s *config.Syslog) error) error {
	ctx := q.r.Context()
	noms, err := nomsEquipements(ctx, c, m.ID)
	if err != nil {
		return err
	}
	return flotte.ModifierConfigSi(ctx, c.db, "machine:"+strconv.FormatInt(m.ID, 10), func(doc map[string]any) error {
		col := lireCollecteurPortee(doc)
		ancien, _ := col["syslog"].(map[string]any)
		if ancien == nil {
			return errors.New("syslog : régler d'abord l'écoute (port, interface, nom du certificat)")
		}
		brut, _ := json.Marshal(ancien)
		var s config.Syslog
		if err := json.Unmarshal(brut, &s); err != nil {
			return err
		}
		if err := changer(&s); err != nil {
			return err
		}
		if err := s.Valider(noms); err != nil {
			return err
		}
		brut, _ = json.Marshal(s)
		var champ map[string]any
		_ = json.Unmarshal(brut, &champ)
		delete(champ, "certificat") // posé par le serveur
		col["syslog"] = champ
		doc["collecteur"] = col
		return nil
	}, q.qui(), q.t)
}

// nomsEquipements : équipements rattachés au collecteur.
func nomsEquipements(ctx context.Context, c *Console, id int64) ([]string, error) {
	eqs, err := flotte.EquipementsDe(ctx, c.db, id)
	if err != nil {
		return nil, err
	}
	noms := make([]string, 0, len(eqs))
	for _, e := range eqs {
		noms = append(noms, e.Nom)
	}
	return noms, nil
}

// postEmetteur : ajouter ou retirer un émetteur syslog.
func (c *Console) postEmetteur(q *req, m flotte.InfoMachine, action string) (string, error) {
	adresse := strings.TrimSpace(q.r.PostFormValue("emetteur_adresse"))
	machine := strings.TrimSpace(q.r.PostFormValue("machine"))
	if adresse == "" {
		return "", errors.New("émetteur : adresse IP ou réseau attendue")
	}
	if action == "emetteur_retirer" {
		err := c.modifierSyslog(q, m, func(s *config.Syslog) error {
			garde := s.Emetteurs[:0]
			for _, e := range s.Emetteurs {
				if e.Adresse != adresse {
					garde = append(garde, e)
				}
			}
			s.Emetteurs = garde
			return nil
		})
		return "émetteur " + adresse + " retiré", err
	}
	if machine == "" {
		return "", errors.New("émetteur : choisir l'équipement")
	}
	err := c.modifierSyslog(q, m, func(s *config.Syslog) error {
		// Même adresse : l'émetteur change d'équipement (pas de doublon).
		garde := s.Emetteurs[:0]
		for _, e := range s.Emetteurs {
			if e.Adresse != adresse {
				garde = append(garde, e)
			}
		}
		s.Emetteurs = append(garde, config.Emetteur{Adresse: adresse, Machine: machine})
		return nil
	})
	return "émetteur " + adresse + " → " + machine + " ajouté : le collecteur l'accepte tout de suite", err
}

// celluleDirect : envoi direct d'un équipement avec son propre token
// (Telegraf), vu par la réception des données. Faux : pas de token.
func (c *Console) celluleDirect(ctx context.Context, e flotte.Equipement, t time.Time) (cellule, bool) {
	var n int
	if err := c.db.QueryRowContext(ctx, `SELECT count(*) FROM tokens WHERE machine_id = ? AND revoque_le = 0`, e.ID).Scan(&n); err != nil || n == 0 {
		return cellule{}, false
	}
	ce := cellule{Classe: "gris", Texte: "rien reçu", Detail: "token prêt"}
	if c.opt.Ingestion == nil {
		return ce, true
	}
	envoi, _ := c.opt.Ingestion.DerniereReception(e.ID)
	if r, ok := c.opt.Ingestion.DernierRefus(e.ID); ok && r.T > envoi {
		return cellule{Classe: "erreur", Texte: "refusé", Detail: r.Motif}, true
	}
	if envoi > 0 {
		age := t.Sub(time.Unix(envoi, 0))
		ce = cellule{Classe: "ok", Texte: "reçu", Detail: "il y a " + dureeLisible(age)}
		if age > 10*time.Minute {
			ce.Classe, ce.Texte = "attention", "plus rien"
		}
	}
	return ce, true
}
