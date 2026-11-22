// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sort"
	"strconv"
	"strings"
	"time"

	"mirador-stack/internal/agent/config"
	"mirador-stack/internal/server/certsyslog"
	"mirador-stack/internal/server/flotte"
)

// Syslog TLS d'un collecteur dans l'interface (SPEC § 23.14) : réglages
// (adresse d'écoute, nom du certificat, émetteurs), avancement du
// certificat, « Renouveler maintenant ». Le compte DNS et Let's Encrypt,
// communs à tous les collecteurs, sont dans Réglages › Certificats (§ 32).

// vueSyslog : bloc « Syslog TLS » de la page d'un collecteur.
type vueSyslog struct {
	Regle           bool // syslog réglé pour ce collecteur
	Ecoute, Nom     string
	Emetteurs       string // une ligne par émetteur : « adresse équipement »
	GarderFilterlog bool
	Cert            certsyslog.Etat
	// Écoute choisie dans l'interface (retour du 22/11) : port et une
	// adresse parmi celles des interfaces du collecteur (mirador_adresse_info).
	Port     string
	Adresses []choixAdresse
	// AvecPareFeu : un émetteur est un pare-feu OPNsense, ou des lignes
	// « filterlog » ont déjà été reçues : la case « garder filterlog » ne
	// sert qu'à eux (retour du 22/11 : affichée pour tout le monde).
	AvecPareFeu   bool
	ReglagesFaits bool       // compte DNS réglé (Réglages › Certificats)
	Avancement    avancement // de la configuration à l'écoute ouverte (SPEC § 32)
}

// syslogDe : réglages actuels (configuration du collecteur) et certificat.
func (c *Console) syslogDe(ctx context.Context, m flotte.InfoMachine) vueSyslog {
	var v vueSyslog
	if cfg, err := flotte.ConfigEffective(ctx, c.db, m.ID); err == nil && cfg.Collecteur != nil && cfg.Collecteur.Syslog != nil {
		s := cfg.Collecteur.Syslog
		v.Regle, v.Ecoute, v.Nom, v.GarderFilterlog = true, s.Ecoute, s.NomCertificat, s.GarderFilterlog
		var lignes []string
		for _, e := range s.Emetteurs {
			lignes = append(lignes, e.Adresse+" "+e.Machine)
		}
		v.Emetteurs = strings.Join(lignes, "\n")
	}
	v.Cert, _ = certsyslog.LireEtat(ctx, c.db, m.ID)
	return v
}

// choixAdresse : une adresse privée d'une interface du collecteur.
type choixAdresse struct {
	IP, Interface string
	Principale    bool
	Cochee        bool
}

// adressesCollecteur : adresses privées des interfaces du collecteur (route
// par défaut d'abord), l'adresse d'écoute actuelle cochée.
func (c *Console) adressesCollecteur(ctx context.Context, nom, actuelle string, t time.Time) []choixAdresse {
	ech, err := c.instant(ctx, `max by (adresse, interface, principale) (last_over_time(mirador_adresse_info{instance=`+strconv.Quote(nom)+`}[15m]))`, t)
	if err != nil {
		return nil
	}
	var out []choixAdresse
	for _, e := range ech {
		ip := e.Etiquettes.Get("adresse")
		if a, err := netip.ParseAddr(ip); err != nil || !config.AdressePrivee(a) {
			continue
		}
		out = append(out, choixAdresse{IP: ip, Interface: e.Etiquettes.Get("interface"), Principale: e.Etiquettes.Get("principale") == "1", Cochee: ip == actuelle})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Principale != out[j].Principale {
			return out[i].Principale
		}
		return out[i].IP < out[j].IP
	})
	// Rien de réglé encore : l'interface de la route par défaut est proposée.
	if actuelle == "" && len(out) > 0 {
		out[0].Cochee = true
	}
	return out
}

// pareFeuParmiEmetteurs : lignes « filterlog » déjà jetées par ce
// collecteur (7 derniers jours), ou un équipement émetteur qui a les
// mesures d'un pare-feu OPNsense (pf_entries, Telegraf).
func (c *Console) pareFeuParmiEmetteurs(ctx context.Context, collecteur, emetteurs string, t time.Time) bool {
	if e, err := c.instant(ctx, `max(max_over_time(mirador_collecteur_syslog_refus_total{instance=`+strconv.Quote(collecteur)+`, motif="filterlog"}[7d])) > 0`, t); err == nil && len(e) > 0 {
		return true
	}
	for _, l := range strings.Split(emetteurs, "\n") {
		ch := strings.Fields(l)
		if len(ch) < 2 {
			continue
		}
		expr := `count(pf_entries{instance=` + strconv.Quote(ch[1]) + `}) or count(pf_entries{host=` + strconv.Quote(ch[1]) + `})`
		if e, err := c.instant(ctx, expr, t); err == nil && len(e) > 0 {
			return true
		}
	}
	return false
}

// syslogAvecAvancement : réglages, certificat et avancement (page du collecteur).
func (c *Console) syslogAvecAvancement(ctx context.Context, m flotte.InfoMachine, t time.Time) vueSyslog {
	v := c.syslogDe(ctx, m)
	ip, port := "", "6514"
	if h, p, err := net.SplitHostPort(v.Ecoute); err == nil {
		ip, port = h, p
	}
	v.Port = port
	v.Adresses = c.adressesCollecteur(ctx, m.Nom, ip, t)
	v.AvecPareFeu = v.GarderFilterlog || c.pareFeuParmiEmetteurs(ctx, m.Nom, v.Emetteurs, t)
	r := lireReglagesCertificats(ctx, c)
	v.ReglagesFaits = r.Faits()
	v.Avancement = c.avancementCertificat(ctx, m.Nom, v, r, t)
	return v
}

// lireEmetteurs : « adresse équipement » par ligne.
func lireEmetteurs(texte string) ([]config.Emetteur, error) {
	var out []config.Emetteur
	for i, ligne := range strings.Split(strings.ReplaceAll(texte, "\r\n", "\n"), "\n") {
		champs := strings.Fields(ligne)
		if len(champs) == 0 || strings.HasPrefix(champs[0], "#") {
			continue
		}
		if len(champs) != 2 {
			return nil, fmt.Errorf("émetteurs, ligne %d : « adresse équipement » attendu", i+1)
		}
		out = append(out, config.Emetteur{Adresse: champs[0], Machine: champs[1]})
	}
	return out, nil
}

// postSyslog : actions du bloc (code 2FA ou mode administration).
func (c *Console) postSyslog(q *req, m flotte.InfoMachine, action string) (string, error) {
	ctx := q.r.Context()
	f := func(k string) string { return strings.TrimSpace(q.r.PostFormValue(k)) }
	switch action {
	case "syslog":
		ems, err := lireEmetteurs(q.r.PostFormValue("emetteurs"))
		if err != nil {
			return "", err
		}
		// Écoute : interface cochée et port (retour du 22/11), ou adresse
		// tapée (collecteur qui n'a pas encore envoyé ses adresses).
		ecoute := f("ecoute")
		if port := f("ecoute_port"); port != "" {
			if f("ecoute_ip") == "" {
				return "", errors.New("syslog : cocher l'interface d'écoute du collecteur")
			}
			ecoute = net.JoinHostPort(f("ecoute_ip"), port)
		}
		s := config.Syslog{Ecoute: ecoute, NomCertificat: strings.ToLower(f("nom_certificat")), Emetteurs: ems,
			GarderFilterlog: f("garder_filterlog") != ""}
		eqs, err := flotte.EquipementsDe(ctx, c.db, m.ID)
		if err != nil {
			return "", err
		}
		noms := make([]string, 0, len(eqs))
		for _, e := range eqs {
			noms = append(noms, e.Nom)
		}
		if err := s.Valider(noms); err != nil {
			return "", err
		}
		brut, err := json.Marshal(s)
		if err != nil {
			return "", err
		}
		var champ map[string]any
		_ = json.Unmarshal(brut, &champ)
		delete(champ, "certificat") // posé par le serveur
		err = flotte.ModifierConfig(ctx, c.db, "machine:"+strconv.FormatInt(m.ID, 10), func(doc map[string]any) {
			col, _ := doc["collecteur"].(map[string]any)
			if col == nil {
				col = map[string]any{}
			}
			col["syslog"] = champ
			doc["collecteur"] = col
		}, q.qui(), q.t)
		return "syslog enregistré : le collecteur crée sa clé et demande son certificat (quelques minutes, puis l'écoute s'ouvre)", err
	case "syslog_retirer":
		err := flotte.ModifierConfig(ctx, c.db, "machine:"+strconv.FormatInt(m.ID, 10), func(doc map[string]any) {
			if col, ok := doc["collecteur"].(map[string]any); ok {
				delete(col, "syslog")
				if len(col) == 0 {
					delete(doc, "collecteur")
				} else {
					doc["collecteur"] = col
				}
			}
		}, q.qui(), q.t)
		return "syslog arrêté sur ce collecteur", err
	case "certificat_renouveler":
		return "renouvellement demandé : fait sous une minute (Let's Encrypt et le défi DNS prennent 1 à 3 min, jusqu'à 20 min chez CSC)",
			certsyslog.Demander(ctx, c.db, m.ID, q.qui(), q.t)
	}
	return "", errors.New("action inconnue")
}
