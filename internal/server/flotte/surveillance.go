// SPDX-License-Identifier: GPL-3.0-or-later

package flotte

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"regexp"
	"strings"
	"time"

	"mirador-stack/internal/agent/distant"
	"mirador-stack/internal/protocole"
)

// Surveillance choisie dans l'interface (SPEC.md § 19) : ce que chaque
// machine envoie ou non. Les choix sont des clés de la configuration distante
// (valeurs typées, jamais de commande), le plus souvent à la portée de la
// machine.

// clesSurveillance : clés apparues avec la surveillance. Un agent plus ancien
// refuserait un document qui en contient une (champ inconnu) : elles ne sont
// envoyées qu'aux agents qui annoncent la capacité « surveillance ».
var clesSurveillance = map[string]bool{
	"temperatures": true, "docker_metriques": true, "ventilateurs": true, "zfs": true, "mdraid": true,
	"systemd_validation": true, "systemd_surveiller": true,
	"envoyer_pare_feu": true, "envoyer_smart": true, "envoyer_certificats": true,
}

// CleSurveillance dit si une clé demande un agent qui connaît la surveillance.
func CleSurveillance(cle string) bool { return clesSurveillance[cle] }

var formatCapacites = regexp.MustCompile(`^[a-z0-9,_-]{0,100}$`)

// capacitesDe lit l'en-tête X-Mirador-Capacites (vide pour un ancien agent).
func capacitesDe(r *http.Request) string {
	c := strings.ToLower(strings.TrimSpace(r.Header.Get(protocole.EnteteCapacites)))
	if !formatCapacites.MatchString(c) {
		return ""
	}
	return c
}

// ACapacite cherche une capacité dans la liste « a,b,c ».
func ACapacite(liste, capacite string) bool {
	for _, c := range strings.Split(liste, ",") {
		if c == capacite {
			return true
		}
	}
	return false
}

// sansSurveillance retire les clés de surveillance d'une configuration
// fusionnée et recalcule son ETag (agent trop ancien pour les comprendre).
func sansSurveillance(doc []byte) ([]byte, string, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(doc, &m); err != nil {
		return nil, "", err
	}
	for k := range m {
		if clesSurveillance[k] {
			delete(m, k)
		}
	}
	out, err := json.Marshal(m)
	if err != nil {
		return nil, "", err
	}
	h := sha256.Sum256(out)
	return out, `"` + hex.EncodeToString(h[:16]) + `"`, nil
}

// ConfigEffective : configuration distante d'une machine, toutes portées
// fusionnées (nil = valeur par défaut de l'agent).
func ConfigEffective(ctx context.Context, db *sql.DB, machine int64) (*distant.ConfigDistante, error) {
	doc, _, err := ConfigMachine(ctx, db, machine)
	if err != nil {
		return nil, err
	}
	return distant.Decoder(bytes.NewReader(doc))
}

// ModifierConfig lit la configuration d'une portée, laisse « changer » la
// modifier clé par clé (nil = retirer la clé), puis l'enregistre avec les
// mêmes contrôles et la même ligne d'audit que DefinirConfig.
func ModifierConfig(ctx context.Context, db *sql.DB, portee string, changer func(m map[string]any), qui string, maintenant time.Time) error {
	if err := ValiderPortee(portee); err != nil {
		return err
	}
	var valeur string
	err := db.QueryRowContext(ctx, `SELECT valeur FROM reglages WHERE cle = ?`, prefixeConfig+portee).Scan(&valeur)
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	m := map[string]any{}
	if valeur != "" {
		dec := json.NewDecoder(strings.NewReader(valeur))
		dec.UseNumber() // les nombres sont réécrits tels quels
		if err := dec.Decode(&m); err != nil {
			return err
		}
	}
	changer(m)
	for k, v := range m {
		if v == nil {
			delete(m, k)
		}
	}
	doc, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return DefinirConfig(ctx, db, portee, doc, qui, maintenant)
}
