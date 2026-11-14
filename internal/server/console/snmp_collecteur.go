// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"mirador-stack/internal/agent/config"
	"mirador-stack/internal/server/flotte"
)

// SNMP v3 et sondes de certificats d'un collecteur dans l'interface
// (SPEC § 23.15). Les mots de passe SNMP sont chiffrés avant d'être rangés,
// jamais réaffichés ; laissés vides à la modification, ils sont gardés.

// vueSNMP : bloc « SNMP et sondes » de la page d'un collecteur.
type vueSNMP struct {
	Cibles      []config.CibleSNMP // mots de passe jamais affichés
	Sondes      string             // « adresse équipement [nom] » par ligne
	Prereglages []string
	UniFi       *config.UniFi // contrôleur UniFi (secrets jamais affichés)
	UniFiCle    bool          // clé API (sinon compte local)
}

func (c *Console) snmpDe(ctx context.Context, m flotte.InfoMachine) vueSNMP {
	v := vueSNMP{Prereglages: []string{"synology", "standard"}}
	cfg, err := flotte.ConfigEffective(ctx, c.db, m.ID)
	if err != nil || cfg.Collecteur == nil {
		return v
	}
	for _, x := range cfg.Collecteur.SNMP {
		x.MotDePasse, x.CleChiffre = "", ""
		v.Cibles = append(v.Cibles, x)
	}
	var lignes []string
	for _, s := range cfg.Collecteur.Sondes {
		l := s.Adresse + " " + s.Machine
		if s.Nom != "" {
			l += " " + s.Nom
		}
		lignes = append(lignes, l)
	}
	v.Sondes = strings.Join(lignes, "\n")
	if u := cfg.Collecteur.UniFi; u != nil {
		copie := *u
		v.UniFiCle = copie.CleAPI != ""
		copie.MotDePasse, copie.CleAPI = "", ""
		v.UniFi = &copie
	}
	return v
}

// lireCollecteurPortee : clé « collecteur » de la configuration propre à la
// machine (avec les secrets chiffrés), pour la modifier.
func lireCollecteurPortee(doc map[string]any) map[string]any {
	col, _ := doc["collecteur"].(map[string]any)
	if col == nil {
		col = map[string]any{}
	}
	return col
}

// postSNMP : enregistrer ou retirer une cible SNMP, ou les sondes.
func (c *Console) postSNMP(q *req, m flotte.InfoMachine, action string) (string, error) {
	ctx := q.r.Context()
	f := func(k string) string { return strings.TrimSpace(q.r.PostFormValue(k)) }
	eqs, err := flotte.EquipementsDe(ctx, c.db, m.ID)
	if err != nil {
		return "", err
	}
	noms := make([]string, 0, len(eqs))
	for _, e := range eqs {
		noms = append(noms, e.Nom)
	}
	portee := "machine:" + strconv.FormatInt(m.ID, 10)
	var errValid error
	modifier := func(changer func(col map[string]any) error) error {
		err := flotte.ModifierConfig(ctx, c.db, portee, func(doc map[string]any) {
			col := lireCollecteurPortee(doc)
			if errValid = changer(col); errValid != nil {
				return
			}
			if len(col) == 0 {
				delete(doc, "collecteur")
			} else {
				doc["collecteur"] = col
			}
		}, q.qui(), q.t)
		return errors.Join(errValid, err)
	}
	switch action {
	case "snmp":
		cible := config.CibleSNMP{Machine: f("machine"), Adresse: f("adresse"), Prereglage: f("prereglage"), Utilisateur: f("utilisateur"),
			Auth: f("auth"), Chiffrement: f("chiffrement")}
		if v := f("intervalle"); v != "" {
			d, err := time.ParseDuration(v)
			if err != nil {
				return "", errors.New("intervalle : durée attendue (ex. 60s, 2m)")
			}
			cible.Intervalle = config.Duree(d)
		}
		mdp, cleCh := q.r.PostFormValue("mot_de_passe"), q.r.PostFormValue("cle_chiffrement")
		err := modifier(func(col map[string]any) error {
			liste, _ := col["snmp"].([]any)
			var garde []any
			var ancien map[string]any
			for _, e := range liste {
				if x, _ := e.(map[string]any); x != nil && x["machine"] == cible.Machine {
					ancien = x
					continue
				}
				garde = append(garde, e)
			}
			// Mots de passe : nouveaux (chiffrés), sinon ceux déjà rangés.
			for _, s := range []struct {
				clair, champ string
				dest         *string
			}{{mdp, "mot_de_passe", &cible.MotDePasse}, {cleCh, "cle_chiffrement", &cible.CleChiffre}} {
				if s.clair == "" {
					if ancien == nil {
						return errors.New("mots de passe SNMP obligatoires pour un nouvel équipement")
					}
					*s.dest, _ = ancien[s.champ].(string)
					continue
				}
				if len(s.clair) < 8 || len(s.clair) > 64 {
					return errors.New("mots de passe SNMP de 8 à 64 caractères")
				}
				chiffre, err := flotte.ChiffrerSecretSNMP(c.opt.Cle, s.clair)
				if err != nil {
					return err
				}
				*s.dest = chiffre
			}
			brut, _ := json.Marshal(cible)
			var nouvelle map[string]any
			_ = json.Unmarshal(brut, &nouvelle)
			garde = append(garde, nouvelle)
			if err := verifierCollecteur(col, "snmp", garde, noms); err != nil {
				return err
			}
			col["snmp"] = garde
			return nil
		})
		return "relevé SNMP de « " + cible.Machine + " » enregistré : le collecteur le lance tout de suite", err
	case "snmp_retirer":
		machine := f("machine")
		err := modifier(func(col map[string]any) error {
			liste, _ := col["snmp"].([]any)
			var garde []any
			for _, e := range liste {
				if x, _ := e.(map[string]any); x != nil && x["machine"] != machine {
					garde = append(garde, e)
				}
			}
			if len(garde) == 0 {
				delete(col, "snmp")
			} else {
				col["snmp"] = garde
			}
			return nil
		})
		return "relevé SNMP de « " + machine + " » retiré", err
	case "unifi":
		u := config.UniFi{Machine: f("machine"), Adresse: strings.TrimRight(f("adresse"), "/"), Utilisateur: f("utilisateur"),
			Empreinte: strings.ToLower(strings.ReplaceAll(f("empreinte"), ":", ""))}
		for _, site := range strings.FieldsFunc(f("sites"), func(r rune) bool { return r == ',' || r == ' ' }) {
			u.Sites = append(u.Sites, site)
		}
		if v := f("intervalle"); v != "" {
			d, err := time.ParseDuration(v)
			if err != nil {
				return "", errors.New("intervalle : durée attendue (ex. 60s, 2m)")
			}
			u.Intervalle = config.Duree(d)
		}
		avecCle := f("mode") == "cle"
		secret := q.r.PostFormValue("secret")
		err := modifier(func(col map[string]any) error {
			ancien, _ := col["unifi"].(map[string]any)
			champ := "mot_de_passe"
			if avecCle {
				champ, u.Utilisateur = "cle_api", ""
			}
			var valeur string
			if secret == "" {
				if ancien == nil {
					return errors.New("mot de passe ou clé API obligatoire")
				}
				valeur, _ = ancien[champ].(string)
				if valeur == "" {
					return errors.New("mot de passe ou clé API obligatoire (mode changé)")
				}
			} else {
				chiffre, err := flotte.ChiffrerSecretSNMP(c.opt.Cle, secret)
				if err != nil {
					return err
				}
				valeur = chiffre
			}
			if avecCle {
				u.CleAPI = valeur
			} else {
				u.MotDePasse = valeur
			}
			brut, _ := json.Marshal(u)
			var nouveau map[string]any
			_ = json.Unmarshal(brut, &nouveau)
			copie := map[string]any{}
			for k, v := range col {
				copie[k] = v
			}
			copie["unifi"] = nouveau
			copie["equipements"] = noms
			brut, _ = json.Marshal(copie)
			var cc config.Collecteur
			if err := json.Unmarshal(brut, &cc); err != nil {
				return err
			}
			if err := cc.Valider(); err != nil {
				return err
			}
			col["unifi"] = nouveau
			return nil
		})
		return "contrôleur UniFi enregistré : relevé par le collecteur à partir de maintenant (unpoller peut être arrêté après comparaison)", err
	case "unifi_retirer":
		err := modifier(func(col map[string]any) error { delete(col, "unifi"); return nil })
		return "relevé UniFi arrêté", err
	case "sondes":
		var liste []any
		for i, ligne := range strings.Split(strings.ReplaceAll(q.r.PostFormValue("sondes"), "\r\n", "\n"), "\n") {
			ch := strings.Fields(ligne)
			if len(ch) == 0 || strings.HasPrefix(ch[0], "#") {
				continue
			}
			if len(ch) < 2 || len(ch) > 3 {
				return "", fmt.Errorf("sondes, ligne %d : « adresse:port équipement [nom] » attendu", i+1)
			}
			s := map[string]any{"adresse": ch[0], "machine": ch[1]}
			if len(ch) == 3 {
				s["nom"] = strings.ToLower(ch[2])
			}
			liste = append(liste, s)
		}
		err := modifier(func(col map[string]any) error {
			if len(liste) == 0 {
				delete(col, "sondes")
				return nil
			}
			if err := verifierCollecteur(col, "sondes", liste, noms); err != nil {
				return err
			}
			col["sondes"] = liste
			return nil
		})
		return "sondes de certificats enregistrées", err
	}
	return "", errors.New("action inconnue")
}

// verifierCollecteur : la clé modifiée passe les contrôles du collecteur
// (équipements rattachés, adresses du LAN, authPriv…).
func verifierCollecteur(col map[string]any, cle string, valeur []any, equipements []string) error {
	copie := map[string]any{}
	for k, v := range col {
		copie[k] = v
	}
	copie[cle] = valeur
	copie["equipements"] = equipements
	brut, err := json.Marshal(copie)
	if err != nil {
		return err
	}
	var cc config.Collecteur
	dec := json.NewDecoder(strings.NewReader(string(brut)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cc); err != nil {
		return err
	}
	return cc.Valider()
}
