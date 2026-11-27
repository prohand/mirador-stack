// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strconv"
	"strings"

	"mirador-stack/internal/server/flotte"
	"mirador-stack/internal/server/fluxbloques"
)

// Réglage « Pare-feu » de chaque machine (SPEC § 136, maquette validée le
// 27/11) : comment lire ses blocages. Gardé en base (clé « parefeu », une
// entrée par nom de machine ; machine absente : réglage par défaut) et
// donné au magasin des flux bloqués, qui l'applique à la réception.
// mirador ne modifie jamais le pare-feu des machines.

const cleParefeu = "parefeu"

// lireReglagesPareFeu : réglages enregistrés (vide si rien ou illisible).
func lireReglagesPareFeu(ctx context.Context, db *sql.DB) map[string]fluxbloques.Reglage {
	r := map[string]fluxbloques.Reglage{}
	if _, err := lireJSONReglage(ctx, db, cleParefeu, &r); err != nil {
		return map[string]fluxbloques.Reglage{}
	}
	return r
}

// chargerPareFeu donne les réglages au magasin (démarrage, enregistrement).
func (c *Console) chargerPareFeu(ctx context.Context) {
	if c.opt.Flux != nil {
		c.opt.Flux.DefinirReglages(lireReglagesPareFeu(ctx, c.db))
	}
}

// reglagePareFeu : réglage d'une machine (défaut si rien n'est enregistré).
func (c *Console) reglagePareFeu(nom string) fluxbloques.Reglage {
	if c.opt.Flux == nil {
		return fluxbloques.ReglageDefaut()
	}
	return c.opt.Flux.Reglage(nom)
}

// postPareFeu : enregistre le réglage d'une machine depuis le bloc
// « Pare-feu » de sa page (admins).
func (c *Console) postPareFeu(q *req, nom string) (string, error) {
	ctx := q.r.Context()
	f := func(k string) string { return strings.TrimSpace(q.r.PostFormValue(k)) }
	r := fluxbloques.Reglage{Lecture: f("lecture"), Entrants: f("entrants") != "", Sortants: f("sortants") != "",
		GarderLignes: f("garder_lignes") != ""}
	if r.Lecture != fluxbloques.LectureAuto && r.Lecture != fluxbloques.LecturePrefixes && r.Lecture != fluxbloques.LectureNon {
		return "", errors.New("lecture des blocages : choix inconnu")
	}
	for _, p := range strings.Split(f("prefixes"), ",") {
		if p = strings.TrimSpace(p); p != "" {
			if len(p) > fluxbloques.TailleMaxPrefixe {
				return "", errors.New("préfixe trop long (40 caractères au plus)")
			}
			r.Prefixes = append(r.Prefixes, p)
		}
	}
	if len(r.Prefixes) > fluxbloques.MaxPrefixes {
		return "", errors.New("10 préfixes au plus")
	}
	if r.Lecture == fluxbloques.LecturePrefixes && len(r.Prefixes) == 0 {
		return "", errors.New("« seulement ces préfixes » : donner au moins un préfixe (ex. DROP_IN:)")
	}
	if r.Lecture != fluxbloques.LectureNon && !r.Entrants && !r.Sortants {
		return "", errors.New("cocher les entrants, les sortants, ou choisir « Ne pas lire »")
	}
	tous := lireReglagesPareFeu(ctx, c.db)
	avant, _ := json.Marshal(tous[nom])
	if n, d := r.Normaliser(), fluxbloques.ReglageDefaut(); n.Lecture == d.Lecture && len(n.Prefixes) == 0 &&
		n.Entrants == d.Entrants && n.Sortants == d.Sortants && n.GarderLignes == d.GarderLignes {
		delete(tous, nom) // réglage par défaut : rien à garder
	} else {
		tous[nom] = r.Normaliser()
	}
	val := ""
	if len(tous) > 0 {
		b, err := json.Marshal(tous)
		if err != nil {
			return "", err
		}
		val = string(b)
	}
	apres, _ := json.Marshal(r.Normaliser())
	if err := ecrireReglageAudit(ctx, c.db, cleParefeu, val, q.qui(), "parefeu "+nom, string(avant)+" → "+string(apres), q.t); err != nil {
		return "", err
	}
	c.chargerPareFeu(ctx)
	// Équipement d'un collecteur : « garder les lignes filterlog » est un
	// réglage du collecteur (il vaut pour tous ses pare-feu).
	if q.r.PostForm.Has("filterlog_present") {
		var id int64
		if err := c.db.QueryRowContext(ctx, `SELECT id FROM machines WHERE nom = ?`, nom).Scan(&id); err != nil {
			return "", err
		}
		colID, _, err := flotte.CollecteurDe(ctx, c.db, id)
		if err != nil {
			return "", errors.New("équipement sans collecteur : « garder les lignes filterlog » ne s'applique pas")
		}
		garder := f("garder_filterlog") != ""
		if err := flotte.ModifierConfig(ctx, c.db, "machine:"+strconv.FormatInt(colID, 10), func(doc map[string]any) {
			col, _ := doc["collecteur"].(map[string]any)
			if col == nil {
				return
			}
			sy, _ := col["syslog"].(map[string]any)
			if sy == nil {
				return
			}
			if garder {
				sy["garder_filterlog"] = true
			} else {
				delete(sy, "garder_filterlog")
			}
		}, q.qui(), q.t); err != nil {
			return "", err
		}
	}
	return "réglage du pare-feu enregistré : appliqué aux prochains blocages reçus", nil
}
