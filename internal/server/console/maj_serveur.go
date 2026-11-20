// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"mirador-stack/internal/majserveur"
	"mirador-stack/internal/server/flotte"
	"mirador-stack/internal/version"
)

// Réglages › Mise à jour du serveur (SPEC § 102, demande du 20/11, option B).
//
// Le serveur ne se met pas à jour lui-même (il tourne dans Docker sans
// droits) : il dépose une demande (numéro de version) que traite
// mirador-maj-serveur sur le VPS, et affiche où il en est. Bouton réservé
// aux administrateurs, avec un code 2FA frais (même en mode administration),
// noté dans l'audit.

// cacheDerniere : dernière version publiée, relue au plus une fois par heure
// (une requête HEAD vers GitHub, seulement quand la page est ouverte).
type cacheDerniere struct {
	mu        sync.Mutex
	version   string
	err       string
	lueLe     time.Time
	pourDepot string
}

func (c *Console) derniereVersionServeur(ctx context.Context, depot string, t time.Time, forcer bool) (string, string) {
	c.derniere.mu.Lock()
	defer c.derniere.mu.Unlock()
	if !forcer && c.derniere.pourDepot == depot && t.Sub(c.derniere.lueLe) < time.Hour {
		return c.derniere.version, c.derniere.err
	}
	src := c.sourceGitHub
	ctx, fin := context.WithTimeout(ctx, 10*time.Second)
	defer fin()
	v, err := flotte.DerniereVersionGitHub(ctx, src, depot)
	c.derniere.version, c.derniere.err, c.derniere.lueLe, c.derniere.pourDepot = v, "", t, depot
	if err != nil {
		c.derniere.err = err.Error()
	}
	return c.derniere.version, c.derniere.err
}

// --- Mise à jour la nuit (réglage, désactivé par défaut ; demande du 20/11) ---
//
// « Au bouton seulement, avec un réglage pour activer la mise à jour la nuit
// si on veut. » Activée, la console vérifie toutes les 10 minutes pendant
// l'heure choisie (heure du fuseau de l'interface) : une version plus récente
// est publiée, le service du VPS est installé, rien n'est en cours → elle
// dépose la demande elle-même (« nuit (automatique) », audit). Une version
// déjà essayée la nuit n'est pas redemandée (un échec attend le bouton).

const (
	cleMajNuit        = "maj_serveur.nuit"         // heure (0 à 23) ; absent : désactivée
	cleMajNuitEssayee = "maj_serveur.nuit_essayee" // dernière version demandée la nuit
)

// lireMajNuit : heure de la mise à jour de nuit (-1 : désactivée).
func lireMajNuit(ctx context.Context, db *sql.DB) int {
	var v string
	if db.QueryRowContext(ctx, `SELECT valeur FROM reglages WHERE cle = ?`, cleMajNuit).Scan(&v) != nil {
		return -1
	}
	h, err := strconv.Atoi(v)
	if err != nil || h < 0 || h > 23 {
		return -1
	}
	return h
}

func ecrireReglageAudit(ctx context.Context, db *sql.DB, cle, val, qui, action, avant string, t time.Time) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // sans effet après Commit
	if val == "" {
		_, err = tx.ExecContext(ctx, `DELETE FROM reglages WHERE cle = ?`, cle)
	} else {
		_, err = tx.ExecContext(ctx, `INSERT INTO reglages(cle, valeur, modifie_le, modifie_par) VALUES (?, ?, ?, ?)
			ON CONFLICT(cle) DO UPDATE SET valeur = excluded.valeur, modifie_le = excluded.modifie_le, modifie_par = excluded.modifie_par`,
			cle, val, t.Unix(), qui)
	}
	if err != nil {
		return err
	}
	if action != "" {
		if _, err := tx.ExecContext(ctx, `INSERT INTO audit(le, qui, action, objet, avant, apres, adresse) VALUES (?, ?, ?, 'serveur', ?, ?, '')`,
			t.Unix(), qui, action, avant, val); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// majNuit : appelée par Tour ; ne fait quelque chose qu'à l'heure réglée.
func (c *Console) majNuit(ctx context.Context, t time.Time) {
	if c.opt.MajDemande == "" || t.Sub(c.dernierMajNuit) < 10*time.Minute {
		return
	}
	c.dernierMajNuit = t
	h := lireMajNuit(ctx, c.db)
	if h < 0 || t.In(fuseau.Load()).Hour() != h {
		return
	}
	v := c.vueMajServeur(ctx, t, false)
	if !v.Proposer || !v.ActuelleValide {
		return
	}
	var essayee string
	_ = c.db.QueryRowContext(ctx, `SELECT valeur FROM reglages WHERE cle = ?`, cleMajNuitEssayee).Scan(&essayee)
	if essayee == v.Derniere {
		return // déjà demandée une nuit : un échec attend le bouton
	}
	d := majserveur.Demande{Version: v.Derniere, Le: t.Unix(), Par: "nuit (automatique)"}
	if err := majserveur.EcrireJSON(c.opt.MajDemande, majserveur.NomDemande, d, 0o644); err != nil { // #nosec G306 -- lu par le service du VPS (root)
		c.journal.Warn("mise à jour de nuit non demandée", "erreur", err)
		return
	}
	_ = ecrireReglageAudit(ctx, c.db, cleMajNuitEssayee, v.Derniere, "nuit", "maj_serveur_demandee", v.Actuelle, t)
	c.journal.Info("mise à jour du serveur demandée (nuit)", "de", v.Actuelle, "vers", v.Derniere)
}

type vueMajServeur struct {
	Actuelle, Derniere, ErreurDerniere string
	Depot, LienNotes                   string
	Active                             bool // dossiers réglés (MIRADOR_MAJ_*)
	Installe                           bool // dossier d'échange présent : service du VPS installé
	EnAttente                          bool // demande déposée, pas encore prise
	DemandeVersion                     string
	Etat                               *majserveur.Etat
	EnCours                            bool
	Proposer                           bool                    // une version plus récente peut être demandée
	ActuelleValide                     bool                    // version de ce serveur au format X.Y.Z (pas « dev »)
	Classe, Texte                      string                  // pastille de l'état
	Nuit                               int                     // heure de la mise à jour de nuit (-1 : désactivée)
	Service                            *majserveur.InfoService // réglages du service du VPS (lecture seule)
	Heures                             []int
}

func (c *Console) vueMajServeur(ctx context.Context, t time.Time, forcer bool) vueMajServeur {
	v := vueMajServeur{Actuelle: version.Version, Active: c.opt.MajDemande != "", Nuit: lireMajNuit(ctx, c.db)}
	for h := 0; h < 24; h++ {
		v.Heures = append(v.Heures, h)
	}
	v.ActuelleValide = majserveur.ValiderVersion(v.Actuelle) == nil
	v.Depot, _ = flotte.LireDepot(ctx, c.db)
	v.Derniere, v.ErreurDerniere = c.derniereVersionServeur(ctx, v.Depot, t, forcer)
	if v.Derniere != "" {
		v.LienNotes = "https://github.com/" + v.Depot + "/releases/tag/v" + v.Derniere
	}
	if !v.Active {
		return v
	}
	if info, err := os.Stat(c.opt.MajDemande); err == nil && info.IsDir() {
		v.Installe = true
	}
	var d majserveur.Demande
	if majserveur.LireJSON(filepath.Join(c.opt.MajDemande, majserveur.NomDemande), &d) == nil {
		v.EnAttente, v.DemandeVersion = true, d.Version
	}
	if c.opt.MajEtat != "" {
		var i majserveur.InfoService
		if majserveur.LireJSON(filepath.Join(c.opt.MajEtat, majserveur.NomService), &i) == nil {
			v.Service = &i
		}
		var e majserveur.Etat
		if majserveur.LireJSON(filepath.Join(c.opt.MajEtat, majserveur.NomEtat), &e) == nil {
			v.Etat = &e
			// Un « en cours » de plus de 30 min : le service s'est arrêté en route.
			v.EnCours = e.Etat == majserveur.EtatEnCours && t.Unix()-e.Le < 30*60
		}
	}
	switch {
	case v.EnAttente:
		v.Classe, v.Texte = "info", "Demande envoyée : en attente du service du VPS"
	case v.EnCours:
		v.Classe, v.Texte = "info", "Mise à jour en cours"
	case v.Etat != nil && v.Etat.Etat == majserveur.EtatOK:
		v.Classe, v.Texte = "ok", "Dernière mise à jour réussie"
	case v.Etat != nil && v.Etat.Etat == majserveur.EtatEchec:
		v.Classe, v.Texte = "erreur", "Dernière mise à jour en échec"
	case v.Etat != nil && v.Etat.Etat == majserveur.EtatRefuse:
		v.Classe, v.Texte = "attention", "Dernière demande refusée"
	}
	v.Proposer = v.Installe && !v.EnAttente && !v.EnCours && v.Derniere != "" &&
		(!v.ActuelleValide || majserveur.Comparer(v.Derniere, v.Actuelle) > 0)
	return v
}

func (c *Console) pageMajServeur(q *req) {
	v := c.vueMajServeur(q.r.Context(), q.t, q.r.URL.Query().Get("relire") == "1" && q.admin())
	c.rendre(q, "maj-serveur", "Mise à jour du serveur", "reglages", v)
}

// postMajServeur : dépose la demande (version vérifiée ici aussi : le
// service du VPS revérifie tout de son côté).
func (c *Console) postMajServeur(q *req) {
	if q.r.PostFormValue("action") == "nuit" {
		c.postMajNuit(q)
		return
	}
	err := func() error {
		if c.opt.MajDemande == "" {
			return errors.New("mise à jour depuis l'interface désactivée (MIRADOR_MAJ_DEMANDE)")
		}
		if err := c.verifierCodeFrais(q); err != nil {
			return err
		}
		ver := strings.TrimPrefix(strings.TrimSpace(q.r.PostFormValue("version")), "v")
		if err := majserveur.ValiderVersion(ver); err != nil {
			return err
		}
		v := c.vueMajServeur(q.r.Context(), q.t, false)
		switch {
		case !v.Installe:
			return errors.New("service mirador-maj-serveur pas installé sur le VPS (voir l'aide de la page)")
		case v.EnAttente || v.EnCours:
			return errors.New("une mise à jour est déjà demandée ou en cours")
		case v.ActuelleValide && majserveur.Comparer(ver, v.Actuelle) <= 0:
			return errors.New("version " + ver + " pas plus récente que " + v.Actuelle)
		}
		d := majserveur.Demande{Version: ver, Le: q.t.Unix(), Par: q.u.Nom}
		if err := majserveur.EcrireJSON(c.opt.MajDemande, majserveur.NomDemande, d, 0o644); err != nil { // #nosec G306 -- lu par le service du VPS (root)
			return err
		}
		_, err := c.db.ExecContext(q.r.Context(), `INSERT INTO audit(le, qui, action, objet, avant, apres, adresse) VALUES (?, ?, 'maj_serveur_demandee', 'serveur', ?, ?, ?)`,
			q.t.Unix(), q.qui(), v.Actuelle, ver, q.ip.String())
		return err
	}()
	c.retour(q, "/reglages/serveur", err, "mise à jour demandée : le service du VPS la prend en charge (suivi ci-dessous)")
}

// postMajNuit : active (heure) ou désactive la mise à jour de nuit (code 2FA
// frais, audit).
func (c *Console) postMajNuit(q *req) {
	ok := "mise à jour de nuit désactivée : au bouton seulement"
	err := func() error {
		if err := c.verifierCodeFrais(q); err != nil {
			return err
		}
		val := ""
		if q.r.PostFormValue("actif") == "1" {
			h, err := strconv.Atoi(q.r.PostFormValue("heure"))
			if err != nil || h < 0 || h > 23 {
				return errors.New("heure : 0 à 23")
			}
			val = strconv.Itoa(h)
			ok = "mise à jour de nuit activée, vers " + val + " h"
		}
		avant := ""
		if h := lireMajNuit(q.r.Context(), c.db); h >= 0 {
			avant = strconv.Itoa(h)
		}
		return ecrireReglageAudit(q.r.Context(), c.db, cleMajNuit, val, q.qui(), "maj_serveur_nuit", avant, q.t)
	}()
	c.retour(q, "/reglages/serveur", err, ok)
}
