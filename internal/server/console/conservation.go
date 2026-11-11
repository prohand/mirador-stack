// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"syscall"
	"time"

	"mirador-stack/internal/server/alertes"
	"mirador-stack/internal/server/stockage/journaux"
	"mirador-stack/internal/server/stockage/metriques"
)

// Conservation des données (brief, étape 5) : durées et plafonds de taille,
// appliqués à chaud par les moteurs de stockage (suppression de blocs ou de
// fichiers entiers, jamais ligne par ligne). Une réduction qui supprimerait
// des données n'est appliquée qu'après un délai (5 min), annulable.
//
// Limite des moteurs (signalée dans SPEC § 18.6) : la TSDB et le stockage des
// logs n'ont qu'UNE rétention par base. Une durée par groupe, par machine ou
// par type de log demanderait une base par catégorie (plus de RAM et de
// fichiers) : non fait.
type Conservation struct {
	Brut      alertes.Duree `json:"brut"`       // métriques brutes (30 s)
	Agrege    alertes.Duree `json:"agrege"`     // agrégats 5 min
	Logs      alertes.Duree `json:"logs"`       // logs
	MaxBrut   int64         `json:"max_brut"`   // octets ; 0 = sans plafond
	MaxAgrege int64         `json:"max_agrege"` // octets
	MaxLogs   int64         `json:"max_logs"`   // octets
	// Base SQLite (petits volumes, purge une fois par jour, ligne par ligne) :
	Audit      alertes.Duree `json:"audit,omitempty"`      // journal d'audit
	Historique alertes.Duree `json:"historique,omitempty"` // alertes résolues et envois
}

// Durées par défaut des historiques de la base (avant le 10/11 : historique
// figé à 180 j dans le code, audit jamais purgé).
const (
	auditDefaut      = alertes.Duree(365 * 24 * time.Hour)
	historiqueDefaut = alertes.Duree(180 * 24 * time.Hour)
)

// completer : réglage enregistré avant l'ajout de l'audit et de
// l'historique → leurs valeurs par défaut.
func (c *Conservation) completer() {
	if c.Audit == 0 {
		c.Audit = auditDefaut
	}
	if c.Historique == 0 {
		c.Historique = historiqueDefaut
	}
}

// ConservationDefaut : SPEC § 7.2 ; plafond total 10 Gio (réponse du
// 10/11) : 5 (brut) + 1 (agrégé) + 4 (logs).
func ConservationDefaut() Conservation {
	j := alertes.Duree(24 * time.Hour)
	return Conservation{Brut: 7 * j, Agrege: 30 * j, Logs: 30 * j, MaxBrut: 5 << 30, MaxAgrege: 1 << 30, MaxLogs: 4 << 30,
		Audit: auditDefaut, Historique: historiqueDefaut}
}

const (
	cleConservation    = "conservation"
	cleConservationAtt = "conservation.en_attente"
	// DelaiReduction : une réduction attend ce délai (annulable) avant d'être appliquée.
	DelaiReduction = 5 * time.Minute
)

// Valider : bornes (garde-fous du brief).
func (c Conservation) Valider() error {
	var errs []error
	j := 24 * time.Hour
	borne := func(nom string, d alertes.Duree, min, max time.Duration) {
		if x := time.Duration(d); x < min || x > max {
			errs = append(errs, fmt.Errorf("%s : entre %s et %s", nom, dureeLisible(min), dureeLisible(max)))
		}
	}
	borne("métriques brutes", c.Brut, j, 90*j)
	borne("métriques agrégées", c.Agrege, 7*j, 730*j)
	borne("logs", c.Logs, j, 365*j)
	// Audit : 90 j au moins (on ne peut pas effacer vite ses propres traces).
	borne("journal d'audit", c.Audit, 90*j, 3650*j)
	borne("historique des alertes et des envois", c.Historique, 30*j, 730*j)
	if c.Agrege < c.Brut {
		errs = append(errs, errors.New("les agrégats doivent durer au moins autant que les métriques brutes"))
	}
	for nom, m := range map[string]int64{"plafond brut": c.MaxBrut, "plafond agrégé": c.MaxAgrege, "plafond logs": c.MaxLogs} {
		if m != 0 && (m < 64<<20 || m > 4<<40) {
			errs = append(errs, fmt.Errorf("%s : 0 (sans plafond) ou entre 64 Mio et 4 Tio", nom))
		}
	}
	return errors.Join(errs...)
}

// reduit dit si n supprimerait des données par rapport à c.
func (c Conservation) reduit(n Conservation) bool {
	moins := func(a, b int64) bool { return b != 0 && (a == 0 || b < a) }
	return n.Brut < c.Brut || n.Agrege < c.Agrege || n.Logs < c.Logs || n.Audit < c.Audit || n.Historique < c.Historique ||
		moins(c.MaxBrut, n.MaxBrut) || moins(c.MaxAgrege, n.MaxAgrege) || moins(c.MaxLogs, n.MaxLogs)
}

func lireJSONReglage(ctx context.Context, db *sql.DB, cle string, v any) (bool, error) {
	var s string
	err := db.QueryRowContext(ctx, `SELECT valeur FROM reglages WHERE cle = ?`, cle).Scan(&s)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	dec := json.NewDecoder(strings.NewReader(s))
	dec.DisallowUnknownFields()
	return true, dec.Decode(v)
}

// LireConservation : réglage en base, ou défauts.
func LireConservation(ctx context.Context, db *sql.DB) (Conservation, error) {
	c := ConservationDefaut()
	c.Audit, c.Historique = 0, 0
	_, err := lireJSONReglage(ctx, db, cleConservation, &c)
	c.completer()
	if err != nil {
		return ConservationDefaut(), err
	}
	return c, c.Valider()
}

// ReductionEnAttente : réglage qui sera appliqué à Le, sauf annulation.
type ReductionEnAttente struct {
	Valeur Conservation `json:"valeur"`
	Le     int64        `json:"le"`
	Par    string       `json:"par"`
}

// LireAttente : réduction en attente (nil s'il n'y en a pas).
func LireAttente(ctx context.Context, db *sql.DB) (*ReductionEnAttente, error) {
	var a ReductionEnAttente
	ok, err := lireJSONReglage(ctx, db, cleConservationAtt, &a)
	if !ok || err != nil {
		return nil, err
	}
	a.Valeur.completer()
	return &a, nil
}

// EcrireConservation : une hausse est enregistrée tout de suite ; une
// réduction est mise en attente DelaiReduction. Renvoie vrai si c'est une
// mise en attente.
func EcrireConservation(ctx context.Context, db *sql.DB, n Conservation, qui, adresse string, t time.Time) (bool, error) {
	if err := n.Valider(); err != nil {
		return false, err
	}
	avant, err := LireConservation(ctx, db)
	if err != nil {
		return false, err
	}
	ba, _ := json.Marshal(avant)
	bn, _ := json.Marshal(n)
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback() //nolint:errcheck // sans effet après Commit
	attente := avant.reduit(n)
	if attente {
		ba2, _ := json.Marshal(ReductionEnAttente{Valeur: n, Le: t.Add(DelaiReduction).Unix(), Par: qui})
		err = ecrireReglage(ctx, tx, cleConservationAtt, string(ba2), qui, t)
	} else {
		if err = ecrireReglage(ctx, tx, cleConservation, string(bn), qui, t); err == nil {
			_, err = tx.ExecContext(ctx, `DELETE FROM reglages WHERE cle = ?`, cleConservationAtt)
		}
	}
	if err != nil {
		return false, err
	}
	action := "conservation"
	if attente {
		action = "conservation_reduction_demandee"
	}
	if err := Auditer(ctx, tx, qui, action, cleConservation, string(ba), string(bn), adresse, t); err != nil {
		return false, err
	}
	return attente, tx.Commit()
}

// AnnulerReduction retire la réduction en attente.
func AnnulerReduction(ctx context.Context, db *sql.DB, qui, adresse string, t time.Time) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // sans effet après Commit
	res, err := tx.ExecContext(ctx, `DELETE FROM reglages WHERE cle = ?`, cleConservationAtt)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errors.New("aucune réduction en attente (déjà appliquée ?)")
	}
	if err := Auditer(ctx, tx, qui, "conservation_reduction_annulee", cleConservation, "", "", adresse, t); err != nil {
		return err
	}
	return tx.Commit()
}

// appliquerConservation : réduction échue → enregistrée ; puis réglages
// donnés aux moteurs (à chaud). Appelé au démarrage et chaque tour.
func (c *Console) appliquerConservation(ctx context.Context, t time.Time) {
	if a, err := LireAttente(ctx, c.db); err == nil && a != nil && t.Unix() >= a.Le {
		bn, _ := json.Marshal(a.Valeur)
		tx, err := c.db.BeginTx(ctx, nil)
		if err == nil {
			err = ecrireReglage(ctx, tx, cleConservation, string(bn), a.Par, t)
			if err == nil {
				_, err = tx.ExecContext(ctx, `DELETE FROM reglages WHERE cle = ?`, cleConservationAtt)
			}
			if err == nil {
				err = Auditer(ctx, tx, "tâche de fond", "conservation_reduction_appliquee", cleConservation, "", string(bn), "", t)
			}
			if err == nil {
				err = tx.Commit()
			} else {
				_ = tx.Rollback()
			}
		}
		if err != nil {
			c.journal.Error("console : réduction de la conservation", "erreur", err)
		}
	}
	cons, err := LireConservation(ctx, c.db)
	if err != nil {
		c.journal.Error("console : réglage de conservation illisible, défauts appliqués", "erreur", err)
	}
	if cons == c.conservationAppliquee {
		return
	}
	if c.opt.Metriques != nil {
		if err := c.opt.Metriques.Regler(metriques.Reglages{RetentionBrut: time.Duration(cons.Brut), RetentionAgrege: time.Duration(cons.Agrege),
			MaxOctetsBrut: cons.MaxBrut, MaxOctetsAgrege: cons.MaxAgrege}); err != nil {
			c.journal.Error("console : conservation des métriques", "erreur", err)
			return
		}
	}
	if c.opt.Logs != nil {
		if err := c.opt.Logs.Regler(journaux.Reglages{Retention: time.Duration(cons.Logs), MaxOctets: cons.MaxLogs}); err != nil {
			c.journal.Error("console : conservation des logs", "erreur", err)
			return
		}
	}
	c.conservationAppliquee = cons
	c.journal.Info("conservation des données appliquée", "brut", cons.Brut.String(), "agrege", cons.Agrege.String(),
		"logs", cons.Logs.String())
}

// EtatStockage : ce qui est réellement occupé, et le rythme mesuré.
type EtatStockage struct {
	Brut, Agrege, Logs                               int64 // octets
	Series                                           uint64
	PlusAncienBrut, PlusAncienAgrege, PlusAncienLogs time.Time
	BrutParJour, LogsParJour                         float64 // octets par jour (mesurés)
	DisqueLibre, DisqueTotal                         int64
	AuditLignes, AuditOctets                         int64
	PlusAncienAudit                                  time.Time
	HistoriqueLignes                                 int64 // alertes résolues + envois
}

func (c *Console) etatStockage(t time.Time) EtatStockage {
	var e EtatStockage
	parJour := func(taille int64, depuis time.Time) float64 {
		if depuis.IsZero() {
			return 0
		}
		j := t.Sub(depuis).Hours() / 24
		if j < 1.0/24 {
			return 0
		}
		return float64(taille) / j
	}
	if c.opt.Metriques != nil {
		e.Brut, e.Agrege = c.opt.Metriques.Taille()
		var b, a int64
		e.Series, b, a = c.opt.Metriques.Etat()
		if b > 0 {
			e.PlusAncienBrut = time.UnixMilli(b)
		}
		if a > 0 {
			e.PlusAncienAgrege = time.UnixMilli(a)
		}
		e.BrutParJour = parJour(e.Brut, e.PlusAncienBrut)
	}
	if c.opt.Logs != nil {
		e.Logs = c.opt.Logs.Taille()
		e.PlusAncienLogs = c.opt.Logs.PlusAncien()
		e.LogsParJour = parJour(e.Logs, e.PlusAncienLogs)
	}
	e.DisqueLibre, e.DisqueTotal = espaceDisque(c.opt.Donnees)
	ctx := context.Background()
	var plusAncien int64
	_ = c.db.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(LENGTH(qui) + LENGTH(action) + LENGTH(objet) + LENGTH(avant) + LENGTH(apres) + LENGTH(adresse) + 40), 0),
		COALESCE(MIN(le), 0) FROM audit`).Scan(&e.AuditLignes, &e.AuditOctets, &plusAncien)
	if plusAncien > 0 {
		e.PlusAncienAudit = time.Unix(plusAncien, 0)
	}
	var a, n int64
	_ = c.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM alertes WHERE etat = 'resolue'`).Scan(&a)
	_ = c.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM envois`).Scan(&n)
	e.HistoriqueLignes = a + n
	return e
}

// PurgerAudit efface les lignes d'audit plus anciennes que avant (une fois
// par jour ; la ligne de la purge elle-même reste).
func PurgerAudit(ctx context.Context, db *sql.DB, avant time.Time) (int64, error) {
	res, err := db.ExecContext(ctx, `DELETE FROM audit WHERE le < ?`, avant.Unix())
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// Estimation : place prévue avec ces réglages, d'après le volume mesuré
// (les agrégats pèsent ~1/10 du brut par jour : un point par 5 min au lieu
// d'un toutes les 30 s).
func (e EtatStockage) Estimation(c Conservation) (metriques, logs int64) {
	jours := func(d alertes.Duree) float64 { return time.Duration(d).Hours() / 24 }
	plafond := func(v float64, max int64) int64 {
		if max > 0 && int64(v) > max {
			return max
		}
		return int64(v)
	}
	metriques = plafond(e.BrutParJour*jours(c.Brut), c.MaxBrut) + plafond(e.BrutParJour/10*jours(c.Agrege), c.MaxAgrege)
	logs = plafond(e.LogsParJour*jours(c.Logs), c.MaxLogs)
	return
}

// espaceDisque : place libre et totale du disque des données.
func espaceDisque(dossier string) (libre, total int64) {
	var st syscall.Statfs_t
	if dossier == "" || syscall.Statfs(dossier, &st) != nil {
		return 0, 0
	}
	return int64(st.Bavail) * st.Bsize, int64(st.Blocks) * st.Bsize // #nosec G115 -- tailles de disque
}
