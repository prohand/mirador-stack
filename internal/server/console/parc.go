// SPDX-License-Identifier: GPL-3.0-or-later

package console

// Taille du parc (SPEC § 141, retour du 27/11 : « gérable dans l'interface,
// rien à régler pour un petit parc »). Le serveur compte les machines
// approuvées et prend 20 % de marge ; l'administrateur peut prévoir plus
// (grosse arrivée de machines). Les plafonds en découlent et sont appliqués
// à chaud par le serveur (séries, mémoire, places, tampons de logs).

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"strconv"
	"strings"
	"time"

	"mirador-stack/internal/server/config"
)

const cleTailleParc = "parc.taille" // machines prévues ; absent = automatique

// disqueParMachine : estimation avec les durées par défaut (logs 30 j,
// métriques brutes 7 j, agrégées 30 j), docs/administration-serveur.md.
const disqueParMachine = 73 << 20

// TailleParc : machines approuvées, prévues (0 = automatique) et taille
// retenue pour les plafonds.
type TailleParc struct {
	Approuvees, Prevue, Retenue int
}

// LireTailleParc : état actuel (une requête et une lecture de réglage).
func LireTailleParc(ctx context.Context, db *sql.DB) TailleParc {
	var t TailleParc
	_ = db.QueryRowContext(ctx, `SELECT count(*) FROM machines WHERE statut = 'approuvee'`).Scan(&t.Approuvees)
	var v string
	if err := db.QueryRowContext(ctx, `SELECT valeur FROM reglages WHERE cle = ?`, cleTailleParc).Scan(&v); err == nil {
		t.Prevue, _ = strconv.Atoi(v)
	}
	t.Retenue = max(t.Prevue, t.Approuvees+t.Approuvees/5, 1)
	return t
}

// vueParc : bloc « Taille du parc » de la page Conservation des données.
type vueParc struct {
	T                  TailleParc
	D                  config.Dimensions
	MemoireFixee       bool  // GOMEMLIMIT donné au conteneur : le serveur n'y touche pas
	Conteneur          int64 // limite mémoire du conteneur (cgroup), 0 = inconnue
	ConteneurConseille int64
	DisqueConseille    int64
	ConteneurTropPetit bool
}

func (c *Console) vueTailleParc(ctx context.Context) vueParc {
	t := LireTailleParc(ctx, c.db)
	v := vueParc{T: t, D: config.DimensionsPour(t.Retenue), MemoireFixee: os.Getenv("GOMEMLIMIT") != ""}
	v.ConteneurConseille = v.D.Memoire * 3 / 2
	v.DisqueConseille = int64(t.Retenue) * disqueParMachine
	v.Conteneur = limiteMemoireConteneur()
	v.ConteneurTropPetit = v.Conteneur > 0 && v.Conteneur < v.D.Memoire*5/4
	return v
}

// limiteMemoireConteneur : limite dure du conteneur (cgroup v2 puis v1),
// 0 si aucune ou illisible.
func limiteMemoireConteneur() int64 {
	for _, f := range []string{"/sys/fs/cgroup/memory.max", "/sys/fs/cgroup/memory/memory.limit_in_bytes"} {
		b, err := os.ReadFile(f) // #nosec G304 -- chemins fixes du noyau
		if err != nil {
			continue
		}
		n, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
		if err != nil || n <= 0 || n >= 1<<60 { // « max » ou valeur géante : pas de limite
			return 0
		}
		return n
	}
	return 0
}

// EcrireTailleParc : machines prévues (0 = automatique), audité.
func EcrireTailleParc(ctx context.Context, db *sql.DB, n int, qui string, t time.Time) error {
	if n < 0 || n > config.TailleParcMax {
		return errors.New("taille du parc : entre 0 (automatique) et " + strconv.Itoa(config.TailleParcMax) + " machines")
	}
	avant := strconv.Itoa(LireTailleParc(ctx, db).Prevue)
	val := ""
	if n > 0 {
		val = strconv.Itoa(n)
	}
	return ecrireReglageAudit(ctx, db, cleTailleParc, val, qui, "taille_parc", avant, t)
}
