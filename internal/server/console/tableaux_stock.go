// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"time"
)

// Tableaux modifiables (SPEC § 18.14). Les tableaux fournis sont embarqués
// (tableaux/*.json) et seulement PROPOSÉS (catalogue) : ils n'apparaissent
// qu'une fois installés. Une ligne de la table « tableaux » = un tableau en
// service : contenu vide = tableau fourni tel quel (il suit les nouvelles
// versions de mirador), sinon le JSON du tableau (fourni modifié, ou créé
// dans l'interface). La colonne « supprime » n'est plus utilisée (ligne
// ignorée).
// L'ensemble en service est recalculé après chaque changement et partagé en
// lecture seule (aucune modification sur place : on modifie une copie).

// Origines d'un tableau.
const (
	OrigineFourni  = "fourni"  // tel que livré
	OrigineModifie = "modifie" // fourni, modifié dans l'interface
	OriginePerso   = "perso"   // créé dans l'interface
)

const tailleTableauMax = 512 << 10 // JSON d'un tableau : 512 Kio au plus

type etatTableaux struct {
	parNom    map[string]*Tableau
	origine   map[string]string
	catalogue []*Tableau // tableaux fournis pas encore installés
}

// chargerTableaux : lignes de la base, puis catalogue (fournis non
// installés). Une ligne illisible est ignorée (journal).
func (c *Console) chargerTableaux(ctx context.Context) {
	e := &etatTableaux{parNom: map[string]*Tableau{}, origine: map[string]string{}}
	rows, err := c.db.QueryContext(ctx, `SELECT nom, contenu FROM tableaux WHERE supprime = 0`)
	if err == nil {
		for rows.Next() {
			var nom, contenu string
			if rows.Scan(&nom, &contenu) != nil {
				continue
			}
			if contenu == "" {
				if t := tableauxDepart[nom]; t != nil {
					e.parNom[nom], e.origine[nom] = t, OrigineFourni
				}
				continue
			}
			var t Tableau
			if err := json.Unmarshal([]byte(contenu), &t); err != nil || t.Nom != nom {
				c.journal.Warn("tableau illisible en base, ignoré", "nom", nom, "erreur", err)
				continue
			}
			e.parNom[nom] = &t
			e.origine[nom] = OriginePerso
			if tableauxDepart[nom] != nil {
				e.origine[nom] = OrigineModifie
			}
		}
		_ = rows.Close() // lecture seule
	} else {
		c.journal.Warn("tableaux de la base illisibles", "erreur", err)
	}
	for nom, t := range tableauxDepart {
		if e.parNom[nom] == nil {
			e.catalogue = append(e.catalogue, t)
		}
	}
	sort.Slice(e.catalogue, func(i, j int) bool { return e.catalogue[i].Titre < e.catalogue[j].Titre })
	c.tableaux.Store(e)
}

func (c *Console) etatTab() *etatTableaux {
	if e := c.tableaux.Load(); e != nil {
		return e
	}
	return &etatTableaux{parNom: map[string]*Tableau{}, origine: map[string]string{}}
}

// tableauOuCatalogue : le tableau en service, sinon celui du catalogue
// (aperçu avant installation, graphiques de la fiche d'une machine).
func (c *Console) tableauOuCatalogue(nom string) (t *Tableau, installe bool) {
	if t := c.tableau(nom); t != nil {
		return t, true
	}
	return tableauxDepart[nom], false
}

// tableau : le tableau en service (nil si inconnu ou supprimé).
func (c *Console) tableau(nom string) *Tableau { return c.etatTab().parNom[nom] }

// origineTableau : fourni, modifie ou perso.
func (c *Console) origineTableau(nom string) string { return c.etatTab().origine[nom] }

// listeTableaux : tableaux en service triés par titre.
func (c *Console) listeTableaux() []*Tableau {
	e := c.etatTab()
	out := make([]*Tableau, 0, len(e.parNom))
	for _, t := range e.parNom {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Titre < out[j].Titre })
	return out
}

// copierTableau : copie profonde (on ne modifie jamais un tableau en service).
func copierTableau(t *Tableau) (*Tableau, error) {
	b, err := json.Marshal(t)
	if err != nil {
		return nil, err
	}
	var n Tableau
	return &n, json.Unmarshal(b, &n)
}

// empreinteTableau : courte empreinte du contenu, pour refuser une
// modification faite sur une version dépassée (deux onglets ouverts…).
func empreinteTableau(t *Tableau) string {
	b, _ := json.Marshal(t)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:8])
}

// ecrireTableau : une ligne de la table (contenu ” et supprime = 1 pour
// masquer) + l'audit, en une transaction ; puis rechargement.
func (c *Console) ecrireTableau(ctx context.Context, nom, contenu string, supprime bool, action, qui, adresse string, t time.Time) error {
	// Lecture de l'ancienne valeur AVANT la transaction (une seule connexion).
	var avant string
	_ = c.db.QueryRowContext(ctx, `SELECT contenu FROM tableaux WHERE nom = ?`, nom).Scan(&avant)
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // sans effet après Commit
	s := 0
	if supprime {
		s = 1
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO tableaux(nom, contenu, supprime, modifie_le, modifie_par) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(nom) DO UPDATE SET contenu = excluded.contenu, supprime = excluded.supprime,
		modifie_le = excluded.modifie_le, modifie_par = excluded.modifie_par`, nom, contenu, s, t.Unix(), qui); err != nil {
		return err
	}
	if err := Auditer(ctx, tx, qui, action, nom, resumeAudit(avant), resumeAudit(contenu), adresse, t); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	c.chargerTableaux(ctx)
	return nil
}

// resumeAudit : le JSON complet d'un tableau est long ; l'audit en garde
// les 4 premiers Kio (assez pour voir ce qui a changé sur un petit tableau).
func resumeAudit(s string) string {
	if len(s) > 4096 {
		return s[:4096] + "…"
	}
	return s
}

// EnregistrerTableau : vérifie puis enregistre un tableau (nouveau ou modifié).
func (c *Console) EnregistrerTableau(ctx context.Context, t *Tableau, action, qui, adresse string, quand time.Time) error {
	t.Version = 1
	if err := t.Valider(); err != nil {
		return err
	}
	b, err := json.Marshal(t)
	if err != nil {
		return err
	}
	if len(b) > tailleTableauMax {
		return fmt.Errorf("tableau trop gros (%d Kio, 512 au plus)", len(b)>>10)
	}
	return c.ecrireTableau(ctx, t.Nom, string(b), false, action, qui, adresse, quand)
}

// SupprimerTableau : la ligne est effacée ; un tableau fourni retourne dans
// le catalogue (réinstallable), un tableau créé ici disparaît.
func (c *Console) SupprimerTableau(ctx context.Context, nom, qui, adresse string, t time.Time) error {
	if c.tableau(nom) == nil {
		return errors.New("tableau inconnu")
	}
	return c.effacerLigne(ctx, nom, "tableau_supprimer", qui, adresse, t)
}

// InstallerTableau : un tableau du catalogue passe en service, tel que fourni.
func (c *Console) InstallerTableau(ctx context.Context, nom, qui, adresse string, t time.Time) error {
	if tableauxDepart[nom] == nil {
		return errors.New("tableau inconnu dans le catalogue")
	}
	if c.tableau(nom) != nil {
		return errors.New("tableau déjà installé")
	}
	return c.ecrireTableau(ctx, nom, "", false, "tableau_installer", qui, adresse, t)
}

// RetablirTableau : un tableau fourni et modifié revient à l'original livré
// avec mirador.
func (c *Console) RetablirTableau(ctx context.Context, nom, qui, adresse string, t time.Time) error {
	if tableauxDepart[nom] == nil || c.tableau(nom) == nil {
		return errors.New("seul un tableau fourni avec mirador, et installé, peut être rétabli")
	}
	return c.ecrireTableau(ctx, nom, "", false, "tableau_retablir", qui, adresse, t)
}

func (c *Console) effacerLigne(ctx context.Context, nom, action, qui, adresse string, t time.Time) error {
	var avant string
	_ = c.db.QueryRowContext(ctx, `SELECT contenu FROM tableaux WHERE nom = ?`, nom).Scan(&avant)
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // sans effet après Commit
	if _, err := tx.ExecContext(ctx, `DELETE FROM tableaux WHERE nom = ?`, nom); err != nil {
		return err
	}
	if err := Auditer(ctx, tx, qui, action, nom, resumeAudit(avant), "", adresse, t); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	c.chargerTableaux(ctx)
	return nil
}

// --- Raccourcis vers des pages de mirador (Logs, Flux bloqués) ---------------
//
// Ce ne sont pas des tableaux mais des pages ; ils sont proposés de la même
// façon (demande du 10/11) : rien n'est installé d'office. Réglage
// « tableaux.pages » : liste des raccourcis installés.

// PageRaccourci : une page proposée sur la page Tableaux.
type PageRaccourci struct {
	Nom, Titre, Description, Lien, Icone, Teinte string
}

var pagesRaccourcis = []PageRaccourci{
	{"logs", "Logs", "Recherche, filtres, palmarès", "/logs", "logs", "bleu"},
	{"flux-bloques", "Flux entrants bloqués", "Pare-feu des machines (pays avec la GéoIP)", "/flux-bloques", "mur", "rouge"},
}

const clePagesTableaux = "tableaux.pages"

// pagesInstallees : raccourcis installés, puis ceux proposés.
func (c *Console) pagesInstallees(ctx context.Context) (installees, proposees []PageRaccourci) {
	var noms []string
	_, _ = lireJSONReglage(ctx, c.db, clePagesTableaux, &noms)
	for _, p := range pagesRaccourcis {
		if slices.Contains(noms, p.Nom) {
			installees = append(installees, p)
		} else {
			proposees = append(proposees, p)
		}
	}
	return
}

// installerPage : ajoute ou retire un raccourci (audit).
func (c *Console) installerPage(ctx context.Context, nom string, installer bool, qui, adresse string, t time.Time) error {
	if !slices.ContainsFunc(pagesRaccourcis, func(p PageRaccourci) bool { return p.Nom == nom }) {
		return errors.New("page inconnue")
	}
	var noms []string
	_, _ = lireJSONReglage(ctx, c.db, clePagesTableaux, &noms)
	noms = slices.DeleteFunc(noms, func(s string) bool { return s == nom })
	action := "tableau_page_retirer"
	if installer {
		noms = append(noms, nom)
		action = "tableau_page_installer"
	}
	b, _ := json.Marshal(noms)
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // sans effet après Commit
	if err := ecrireReglage(ctx, tx, clePagesTableaux, string(b), qui, t); err != nil {
		return err
	}
	if err := Auditer(ctx, tx, qui, action, nom, "", "", adresse, t); err != nil {
		return err
	}
	return tx.Commit()
}
