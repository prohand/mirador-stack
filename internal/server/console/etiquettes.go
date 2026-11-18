// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"mirador-stack/internal/server/flotte"
	"mirador-stack/internal/server/servicesmetier"
)

// Étiquettes libres (SPEC § 59, idée reprise de New Relic « tags ») :
// « clé=valeur » sur n'importe quel élément (machine, équipement,
// conteneur, sonde, service), en plus de site, type et groupe. Gardées en
// base (réglage « etiquettes_elements ») ; utilisables dans la page
// Éléments (filtre), sur les fiches, et dans les règles par la série
// interne mirador_element_etiquette{element, cle, valeur}.

const (
	cleEtiquettes        = "etiquettes_elements"
	maxEtiquettes        = 16 // par élément
	maxElementsEtiquetes = 2000
)

var (
	reCleEtiquette    = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)
	reValeurEtiquette = regexp.MustCompile(`^[\pL\pN _.:/-]{1,64}$`)
)

// lireEtiquettes : élément (« m:pve1 ») → clé → valeur.
func (c *Console) lireEtiquettes(ctx context.Context) map[string]map[string]string {
	out := map[string]map[string]string{}
	var v string
	if c.db.QueryRowContext(ctx, `SELECT valeur FROM reglages WHERE cle = ?`, cleEtiquettes).Scan(&v) == nil {
		_ = json.Unmarshal([]byte(v), &out)
	}
	return out
}

// lireLignesEtiquettes : « clé=valeur » une par ligne (ou séparées par des virgules).
func lireLignesEtiquettes(texte string) (map[string]string, error) {
	out := map[string]string{}
	for _, l := range strings.FieldsFunc(texte, func(r rune) bool { return r == '\n' || r == ',' }) {
		l = strings.TrimSpace(l)
		if l == "" {
			continue
		}
		k, v, ok := strings.Cut(l, "=")
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if !ok || !reCleEtiquette.MatchString(k) || !reValeurEtiquette.MatchString(v) {
			return nil, fmt.Errorf("étiquette %q : clé=valeur attendu (clé : minuscules, chiffres, _ ; valeur : 64 caractères au plus)", l)
		}
		out[k] = v
	}
	if len(out) > maxEtiquettes {
		return nil, fmt.Errorf("%d étiquettes au plus par élément", maxEtiquettes)
	}
	return out, nil
}

// texteEtiquettesElement : « a=1, b=2 » trié (formulaire).
func texteEtiquettesElement(m map[string]string) string {
	var l []string
	for k, v := range m {
		l = append(l, k+"="+v)
	}
	sort.Strings(l)
	return strings.Join(l, ", ")
}

// enregistrerEtiquettes : étiquettes d'un élément (remplacées), audit.
func (c *Console) enregistrerEtiquettes(ctx context.Context, q *req, cle string, etq map[string]string, t time.Time) error {
	if cle == "" || len(cle) > 200 || !strings.Contains("mcsv", cle[:1]) || cle[1] != ':' {
		return errors.New("élément inconnu")
	}
	tout := c.lireEtiquettes(ctx)
	avant := texteEtiquettesElement(tout[cle])
	if len(etq) == 0 {
		delete(tout, cle)
	} else {
		tout[cle] = etq
	}
	if len(tout) > maxElementsEtiquetes {
		return errors.New("trop d'éléments étiquetés")
	}
	brut, err := json.Marshal(tout)
	if err != nil {
		return err
	}
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // sans effet après Commit
	if _, err := tx.ExecContext(ctx, `INSERT INTO reglages(cle, valeur, modifie_le, modifie_par) VALUES (?, ?, ?, ?)
		ON CONFLICT(cle) DO UPDATE SET valeur = excluded.valeur, modifie_le = excluded.modifie_le, modifie_par = excluded.modifie_par`,
		cleEtiquettes, string(brut), t.Unix(), q.qui()); err != nil {
		return err
	}
	if err := Auditer(ctx, tx, q.qui(), "etiquettes", cle, avant, texteEtiquettesElement(etq), q.ip.String(), t); err != nil {
		return err
	}
	return tx.Commit()
}

// postEtiquettes : formulaire des fiches et de la page Éléments.
func (c *Console) postEtiquettes(q *req) {
	retour := q.r.PostFormValue("retour")
	if !strings.HasPrefix(retour, "/") || strings.HasPrefix(retour, "//") {
		retour = "/elements"
	}
	etq, err := lireLignesEtiquettes(q.r.PostFormValue("etiquettes"))
	if err == nil {
		err = c.enregistrerEtiquettes(q.r.Context(), q, q.r.PostFormValue("element"), etq, q.t)
	}
	c.retour(q, retour, err, "étiquettes enregistrées")
}

// blocElementMachine : liens et étiquettes de la fiche d'une machine (même
// bloc que les autres fiches, SPEC § 56).
type blocElementMachine struct {
	Cle, Texte string
	Etiquettes map[string]string
	Liens      []lienElement
	Retour     string
}

func (c *Console) elementMachine(ctx context.Context, nom string) *blocElementMachine {
	b := &blocElementMachine{Cle: "m:" + nom, Retour: "/machines/" + nom}
	b.Etiquettes = c.lireEtiquettes(ctx)[b.Cle]
	b.Texte = texteEtiquettesElement(b.Etiquettes)
	if r, err := flotte.RelevesParCollecteur(ctx, c.db); err == nil {
		if col, ok := r[nom]; ok {
			b.Liens = append(b.Liens, lienElement{"relevé par", col, "/machines/" + col})
		}
	}
	if svcs, err := servicesmetier.Lire(ctx, c.db); err == nil {
		for _, s := range svcs {
			for _, x := range s.Composants {
				m, _, _ := strings.Cut(x.Nom, "/")
				if (x.Sorte == servicesmetier.Machine && x.Nom == nom) || (x.Sorte == servicesmetier.Conteneur && m == nom) {
					b.Liens = append(b.Liens, lienElement{"fait partie de", s.Nom, "/services/" + s.Nom})
					break
				}
			}
		}
	}
	return b
}
