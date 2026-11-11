// SPDX-License-Identifier: GPL-3.0-or-later

package flotte

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"errors"
	"net/http"
	"regexp"
	"time"

	"mirador-stack/internal/protocole"
	"mirador-stack/internal/server/ingestion"
	"mirador-stack/internal/server/web"
)

// Formats acceptés dans une demande d'enrôlement.
var (
	// FormatNom : même règle que l'agent et l'ancien script (SPEC § 4.5).
	FormatNom         = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$`)
	formatIdentifiant = regexp.MustCompile(`^[0-9a-f]{32}$`)
	formatVersion     = regexp.MustCompile(`^[0-9A-Za-z.+_-]{1,40}$`)
)

// Architectures acceptées.
var architectures = map[string]bool{"amd64": true, "arm64": true}

// Messages renvoyés à l'agent (courts, sans détail utile à un attaquant).
const (
	msgJetonInvalide = "jeton d'enrôlement invalide, déjà utilisé ou expiré"
	msgNomPris       = "nom déjà utilisé par une autre machine : choisissez-en un autre"
	msgRefusee       = "demande refusée dans l'interface"
)

// PrefixeToken : repérable par les outils de détection de secrets.
const PrefixeToken = "mirador_"

// NouveauToken : 256 bits aléatoires. Seule son empreinte est gardée en base.
func NouveauToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return PrefixeToken + base64.RawURLEncoding.EncodeToString(b), nil
}

func (f *Flotte) enroler(w http.ResponseWriter, r *http.Request) {
	maintenant := f.opt.Maintenant()
	ip := web.AdresseClient(r).String()
	cle := ingestion.CleLimite(web.AdresseClient(r)) // IPv6 : le /64 entier
	if !f.demandes.Prendre(cle, 1, maintenant) || !f.echecs.Prendre(cle, 0, maintenant) {
		w.Header().Set("Retry-After", "30")
		erreur(w, http.StatusTooManyRequests, "trop de demandes")
		return
	}
	var d protocole.DemandeEnrolement
	if err := lireJSON(r, &d); err != nil {
		erreur(w, http.StatusBadRequest, "demande illisible")
		return
	}
	if len(d.Jeton) < 20 || len(d.Jeton) > 256 || !formatIdentifiant.MatchString(d.Identifiant) ||
		!formatVersion.MatchString(d.Version) || !architectures[d.Architecture] {
		f.echecs.Prendre(cle, 1, maintenant)
		erreur(w, http.StatusBadRequest, "demande invalide")
		return
	}
	rep, echec, err := f.traiter(r.Context(), d, ip, maintenant)
	if err != nil {
		f.opt.Journal.Error("enrôlement", "erreur", err)
		erreur(w, http.StatusServiceUnavailable, "base indisponible")
		return
	}
	if echec {
		f.echecs.Prendre(cle, 1, maintenant)
		f.opt.Journal.Warn("enrôlement refusé", "ip", ip, "raison", rep.Message)
	}
	repondreJSON(w, http.StatusOK, rep)
}

// traiter : toute la décision dans une transaction (deux demandes simultanées
// avec le même jeton ne peuvent pas créer deux machines). echec = à compter
// comme tentative suspecte pour l'adresse.
func (f *Flotte) traiter(ctx context.Context, d protocole.DemandeEnrolement, ip string, maintenant time.Time) (rep protocole.ReponseEnrolement, echec bool, err error) {
	refus := func(msg string) (protocole.ReponseEnrolement, bool, error) {
		return protocole.ReponseEnrolement{Statut: protocole.Refuse, Message: msg}, true, nil
	}
	tx, err := f.opt.DB.BeginTx(ctx, nil)
	if err != nil {
		return rep, false, err
	}
	defer tx.Rollback() //nolint:errcheck // sans effet après Commit

	e := ingestion.Empreinte(d.Jeton)
	var (
		idJeton, expire, utilise int64
		machine                  sql.NullInt64
		identifiant              string
		etiq                     Etiquettes
	)
	err = tx.QueryRowContext(ctx, `SELECT id, expire_le, utilise_le, machine_id, identifiant, site, env, groupe, nom FROM jetons_enrolement WHERE empreinte = ?`,
		e[:]).Scan(&idJeton, &expire, &utilise, &machine, &identifiant, &etiq.Site, &etiq.Env, &etiq.Groupe, &etiq.Nom)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return refus(msgJetonInvalide)
	case err != nil:
		return rep, false, err
	case expire <= maintenant.Unix():
		return refus(msgJetonInvalide)
	}

	if utilise == 0 {
		// Première demande : la machine est créée « en attente ». Nom choisi
		// à la création du jeton : il s'impose à celui que propose la machine.
		if etiq.Nom != "" {
			d.NomPropose = etiq.Nom
		}
		if !FormatNom.MatchString(d.NomPropose) {
			return protocole.ReponseEnrolement{Statut: protocole.Refuse, Message: "nom de machine invalide"}, false, nil
		}
		if err := nomLibre(ctx, tx, d.NomPropose, 0); err != nil {
			// Le jeton n'est pas consommé : nouvel essai possible avec un autre nom.
			return protocole.ReponseEnrolement{Statut: protocole.Refuse, Message: msgNomPris}, false, nil
		}
		// Site, type et groupe : ceux choisis à la création du jeton ; à
		// défaut, site et type proposés par la machine (--site / --env, ou
		// ceux d'Alloy), à vérifier à l'approbation. Jamais le groupe : il
		// décide de la configuration de l'agent, l'admin seul le choisit.
		if etiq.Site == "" && formatEtiquette.MatchString(d.SitePropose) {
			etiq.Site = d.SitePropose
		}
		if etiq.Env == "" && formatEtiquette.MatchString(d.EnvPropose) {
			etiq.Env = d.EnvPropose
		}
		res, err := tx.ExecContext(ctx, `INSERT INTO machines(nom, type, statut, identifiant, version_agent, architecture, ip_enrolement, cree_le, site, env, groupe)
			VALUES (?, 'agent', 'en_attente', ?, ?, ?, ?, ?, ?, ?, ?)`, d.NomPropose, d.Identifiant, d.Version, d.Architecture, ip, maintenant.Unix(),
			etiq.Site, etiq.Env, etiq.Groupe)
		if err != nil {
			return rep, false, err
		}
		id, err := res.LastInsertId()
		if err != nil {
			return rep, false, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE jetons_enrolement SET utilise_le = ?, machine_id = ?, identifiant = ? WHERE id = ?`,
			maintenant.Unix(), id, d.Identifiant, idJeton); err != nil {
			return rep, false, err
		}
		if err := audit(ctx, tx, maintenant.Unix(), "agent", "enrolement_demande", d.NomPropose, "", "en_attente", ip); err != nil {
			return rep, false, err
		}
		if err := tx.Commit(); err != nil {
			return rep, false, err
		}
		f.opt.Journal.Info("nouvelle machine en attente d'approbation", "machine", d.NomPropose, "ip", ip)
		return protocole.ReponseEnrolement{Statut: protocole.EnAttente, Nom: d.NomPropose, Attente: protocole.DelaiEnrolementDef}, false, nil
	}

	// Demandes suivantes : seule l'installation qui a utilisé le jeton (même
	// identifiant aléatoire) peut suivre la demande.
	if subtle.ConstantTimeCompare([]byte(identifiant), []byte(d.Identifiant)) != 1 || !machine.Valid {
		return refus(msgJetonInvalide)
	}
	var nom, statut string
	if err := tx.QueryRowContext(ctx, `SELECT nom, statut FROM machines WHERE id = ?`, machine.Int64).Scan(&nom, &statut); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return refus(msgJetonInvalide)
		}
		return rep, false, err
	}
	switch statut {
	case "en_attente":
		return protocole.ReponseEnrolement{Statut: protocole.EnAttente, Nom: nom, Attente: protocole.DelaiEnrolementDef}, false, nil
	case "refusee":
		return protocole.ReponseEnrolement{Statut: protocole.Refuse, Message: msgRefusee}, false, nil
	case "approuvee":
	default:
		return refus(msgJetonInvalide)
	}
	// Approuvée : nouveau token. S'il en avait déjà reçu un par ce jeton
	// (réponse perdue en route), l'ancien est révoqué : un seul token valable.
	token, err := NouveauToken()
	if err != nil {
		return rep, false, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE tokens SET revoque_le = ? WHERE machine_id = ? AND revoque_le = 0`,
		maintenant.Unix(), machine.Int64); err != nil {
		return rep, false, err
	}
	et := ingestion.Empreinte(token)
	if _, err := tx.ExecContext(ctx, `INSERT INTO tokens(machine_id, empreinte, cree_le) VALUES (?, ?, ?)`,
		machine.Int64, et[:], maintenant.Unix()); err != nil {
		return rep, false, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE jetons_enrolement SET token_remis_le = ? WHERE id = ?`, maintenant.Unix(), idJeton); err != nil {
		return rep, false, err
	}
	if err := audit(ctx, tx, maintenant.Unix(), "agent", "token_remis", nom, "", "***", ip); err != nil {
		return rep, false, err
	}
	if err := tx.Commit(); err != nil {
		return rep, false, err
	}
	if f.opt.Ingestion != nil {
		f.opt.Ingestion.Auth().Oublier() // l'ancien token cesse tout de suite
	}
	f.opt.Journal.Info("token remis à une machine approuvée", "machine", nom, "ip", ip)
	return protocole.ReponseEnrolement{Statut: protocole.Approuve, Token: token, Nom: nom}, false, nil
}
