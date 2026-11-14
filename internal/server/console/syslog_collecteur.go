// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"mirador-stack/internal/agent/config"
	"mirador-stack/internal/server/certsyslog"
	"mirador-stack/internal/server/flotte"
)

// Syslog TLS d'un collecteur dans l'interface (SPEC § 23.14) : réglages
// (adresse d'écoute, nom du certificat, émetteurs), état du certificat,
// « Renouveler maintenant », réglages DNS / ACME (communs à tous les
// collecteurs).

// vueSyslog : bloc « Syslog TLS » de la page d'un collecteur.
type vueSyslog struct {
	Regle           bool // syslog réglé pour ce collecteur
	Ecoute, Nom     string
	Emetteurs       string // une ligne par émetteur : « adresse équipement »
	GarderFilterlog bool
	Cert            certsyslog.Etat
	Domaine         string
	Annuaire        string
	JetonEnregistre bool // identifiants DNS déjà rangés
	Fournisseur     string
	Parametres      map[string]string // champs non secrets enregistrés
	Fournisseurs    []certsyslog.InfoFournisseur
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
	if r, err := certsyslog.LireReglages(ctx, c.db); err == nil {
		v.Domaine, v.Annuaire, v.JetonEnregistre = r.Domaine, r.Annuaire, r.IdentifiantsEnregistres()
		v.Fournisseur, v.Parametres = r.FournisseurEffectif(), r.Parametres
	}
	if v.Fournisseur == "" {
		v.Fournisseur = "desec"
	}
	v.Fournisseurs = certsyslog.Fournisseurs
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
		s := config.Syslog{Ecoute: f("ecoute"), NomCertificat: strings.ToLower(f("nom_certificat")), Emetteurs: ems,
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
		return "renouvellement demandé : fait sous une minute (Let's Encrypt et le défi DNS prennent 1 à 3 min)",
			certsyslog.Demander(ctx, c.db, m.ID, q.qui(), q.t)
	case "certificat_reglages":
		// Champs du fournisseur choisi : « dns_<fournisseur>_<champ> ».
		s := certsyslog.Saisie{Fournisseur: f("fournisseur"), Domaine: f("domaine"), Annuaire: f("annuaire"), Identifiants: map[string]string{}}
		if info := certsyslog.InfoDe(s.Fournisseur); info != nil {
			for _, ch := range info.Champs {
				s.Identifiants[ch.Nom] = q.r.PostFormValue("dns_" + info.Nom + "_" + ch.Nom)
			}
		}
		err := certsyslog.DefinirReglages(ctx, c.db, c.opt.Cle, s, q.qui(), q.t)
		return "réglages du certificat enregistrés (les secrets ne sont jamais réaffichés)", err
	}
	return "", errors.New("action inconnue")
}
