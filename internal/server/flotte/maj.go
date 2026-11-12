// SPDX-License-Identifier: GPL-3.0-or-later

package flotte

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"time"
	"unicode/utf8"

	"mirador-stack/internal/agent/majagent"
	"mirador-stack/internal/protocole"
)

// formatVersionPubliee : versions « 1.2.3 » seulement (comme l'agent).
var formatVersionPubliee = regexp.MustCompile(`^[0-9]{1,5}\.[0-9]{1,5}\.[0-9]{1,5}$`)

// cleCible : réglage global de la version assignée à toutes les machines.
const cleCible = "version_agent_cible"

func nomBinaire(version, arch string) string { return "mirador-agent-" + version + "-linux-" + arch }

// Publier enregistre une version signée de l'agent. La signature est vérifiée
// ici aussi avec les clés publiques réglées dans l'interface (celles que
// reçoivent les agents à l'installation) : une erreur est vue tout de suite,
// pas sur 25 machines.
func Publier(ctx context.Context, db *sql.DB, dossier string, m *protocole.Manifeste, binaire []byte, qui string, maintenant time.Time) error {
	if !formatVersionPubliee.MatchString(m.Version) || !architectures[m.Architecture] {
		return fmt.Errorf("version %q ou architecture %q invalide", m.Version, m.Architecture)
	}
	cles, err := clesVerification(ctx, db)
	if err != nil {
		return err
	}
	if cles == "" {
		return errors.New("aucune clé publique réglée : la saisir d'abord (Mises à jour des agents › Clé publique)")
	}
	if err := majagent.VerifierSignature(m, cles); err != nil {
		return fmt.Errorf("manifeste : %w", err)
	}
	if err := majagent.VerifierBinaire(m, binaire); err != nil {
		return fmt.Errorf("binaire : %w", err)
	}
	if err := os.MkdirAll(dossier, 0o700); err != nil {
		return err
	}
	chemin := filepath.Join(dossier, nomBinaire(m.Version, m.Architecture))
	tmp := chemin + ".tmp"
	if err := os.WriteFile(tmp, binaire, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, chemin); err != nil {
		return err
	}
	retour := 0
	if m.RetourAutorise {
		retour = 1
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // sans effet après Commit
	if _, err := tx.ExecContext(ctx, `INSERT INTO versions_agent(version, architecture, sha256, taille, retour_autorise, signature, publiee_le, publiee_par)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT(version, architecture) DO UPDATE SET sha256 = excluded.sha256, taille = excluded.taille,
		retour_autorise = excluded.retour_autorise, signature = excluded.signature, publiee_le = excluded.publiee_le, publiee_par = excluded.publiee_par`,
		m.Version, m.Architecture, m.SHA256, m.Taille, retour, m.Signature, maintenant.Unix(), qui); err != nil {
		return err
	}
	if err := audit(ctx, tx, maintenant.Unix(), qui, "version_publiee", m.Version+"/"+m.Architecture, "", m.SHA256, ""); err != nil {
		return err
	}
	return tx.Commit()
}

// DefinirCible assigne une version (machine = 0 : toutes les machines sans
// version propre). version vide = retirer l'assignation.
func DefinirCible(ctx context.Context, db *sql.DB, machine int64, version, qui string, maintenant time.Time) error {
	if version != "" {
		if !formatVersionPubliee.MatchString(version) {
			return fmt.Errorf("version %q invalide", version)
		}
		var n int
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM versions_agent WHERE version = ?`, version).Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			return fmt.Errorf("version %s non publiée", version)
		}
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // sans effet après Commit
	objet := "toutes"
	if machine == 0 {
		_, err = tx.ExecContext(ctx, `INSERT INTO reglages(cle, valeur, modifie_le, modifie_par) VALUES (?, ?, ?, ?)
			ON CONFLICT(cle) DO UPDATE SET valeur = excluded.valeur, modifie_le = excluded.modifie_le, modifie_par = excluded.modifie_par`,
			cleCible, `"`+version+`"`, maintenant.Unix(), qui)
	} else {
		objet = fmt.Sprint("machine:", machine)
		var res sql.Result
		res, err = tx.ExecContext(ctx, `UPDATE machines SET version_cible = ? WHERE id = ?`, version, machine)
		if err == nil {
			if n, _ := res.RowsAffected(); n == 0 {
				err = errors.New("machine inconnue")
			}
		}
	}
	if err != nil {
		return err
	}
	if err := audit(ctx, tx, maintenant.Unix(), qui, "version_cible", objet, "", version, ""); err != nil {
		return err
	}
	return tx.Commit()
}

// cible renvoie la version assignée à une machine et son architecture.
func (f *Flotte) cible(ctx context.Context, machine int64) (version, arch string, err error) {
	if err = f.opt.DB.QueryRowContext(ctx, `SELECT version_cible, architecture FROM machines WHERE id = ?`, machine).Scan(&version, &arch); err != nil {
		return
	}
	if version == "" {
		var v string
		e := f.opt.DB.QueryRowContext(ctx, `SELECT valeur FROM reglages WHERE cle = ?`, cleCible).Scan(&v)
		if e != nil && !errors.Is(e, sql.ErrNoRows) {
			return "", "", e
		}
		if len(v) >= 2 {
			version = v[1 : len(v)-1]
		}
	}
	return version, arch, nil
}

func (f *Flotte) manifeste(ctx context.Context, version, arch string) (*protocole.Manifeste, error) {
	m := &protocole.Manifeste{Version: version, Architecture: arch}
	var retour int
	err := f.opt.DB.QueryRowContext(ctx, `SELECT sha256, taille, retour_autorise, signature FROM versions_agent WHERE version = ? AND architecture = ?`,
		version, arch).Scan(&m.SHA256, &m.Taille, &retour, &m.Signature)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	m.RetourAutorise = retour == 1
	return m, err
}

// maj : version assignée. Rien (manifeste nul) si aucune version n'est
// assignée, si l'agent l'a déjà, ou si elle n'existe pas pour son architecture.
func (f *Flotte) maj(w http.ResponseWriter, r *http.Request) {
	machine, liberer, ok := f.opt.Ingestion.Accueillir(w, r)
	if !ok {
		return
	}
	defer liberer()
	version, arch, err := f.cible(r.Context(), machine.ID)
	if err != nil {
		erreur(w, http.StatusServiceUnavailable, "base indisponible")
		return
	}
	var rep protocole.ReponseMaj
	fen, _ := LireFenetre(r.Context(), f.opt.DB)
	if version != "" && version != r.Header.Get(protocole.EnteteVersion) && fen.Contient(f.opt.Maintenant()) {
		if rep.Manifeste, err = f.manifeste(r.Context(), version, arch); err != nil {
			erreur(w, http.StatusServiceUnavailable, "base indisponible")
			return
		}
	}
	repondreJSON(w, http.StatusOK, rep)
}

// binaire : seulement la version assignée à cette machine, pour son
// architecture (une machine ne peut pas télécharger n'importe quoi).
func (f *Flotte) binaire(w http.ResponseWriter, r *http.Request) {
	machine, liberer, ok := f.opt.Ingestion.Accueillir(w, r)
	if !ok {
		return
	}
	defer liberer()
	version, arch, err := f.cible(r.Context(), machine.ID)
	if err != nil {
		erreur(w, http.StatusServiceUnavailable, "base indisponible")
		return
	}
	q := r.URL.Query()
	if version == "" || q.Get(protocole.ParametreVersion) != version || q.Get(protocole.ParametreArchitecture) != arch {
		erreur(w, http.StatusNotFound, "version non assignée à cette machine")
		return
	}
	m, err := f.manifeste(r.Context(), version, arch)
	if err != nil || m == nil {
		erreur(w, http.StatusNotFound, "version non publiée")
		return
	}
	fd, err := os.Open(filepath.Join(f.opt.DossierVersions, nomBinaire(version, arch))) // #nosec G304 -- nom fabriqué à partir de valeurs vérifiées
	if err != nil {
		erreur(w, http.StatusNotFound, "binaire absent")
		return
	}
	defer fd.Close()
	// Un binaire de ~8 Mo sur une liaison lente : délai d'écriture allongé.
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(10 * time.Minute))
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", fmt.Sprint(m.Taille))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	h := sha256.New()
	if _, err := io.Copy(w, io.TeeReader(io.LimitReader(fd, m.Taille), h)); err != nil {
		f.opt.Journal.Warn("envoi d'un binaire interrompu", "machine", machine.Nom, "erreur", err)
		return
	}
	if hex.EncodeToString(h.Sum(nil)) != m.SHA256 {
		// Fichier modifié sur le serveur : l'agent le refusera (empreinte), on le signale ici.
		f.opt.Journal.Error("binaire publié modifié sur le disque du serveur", "version", version, "arch", arch)
	}
}

// --- Battement ---------------------------------------------------------------------

var formatTexteCourt = regexp.MustCompile(`^[\p{L}\p{N} .,:;/()'+_-]{0,200}$`)

func (f *Flotte) battre(w http.ResponseWriter, r *http.Request) {
	m, liberer, ok := f.opt.Ingestion.Accueillir(w, r)
	if !ok {
		return
	}
	defer liberer()
	var b protocole.Battement
	if err := lireJSON(r, &b); err != nil {
		erreur(w, http.StatusBadRequest, "battement illisible")
		return
	}
	if !formatVersion.MatchString(b.Version) || (b.Architecture != "" && !architectures[b.Architecture]) ||
		!utf8.ValidString(b.MajEssai+b.MajEchec) || !formatTexteCourt.MatchString(b.MajEssai) || !formatTexteCourt.MatchString(b.MajEchec) {
		erreur(w, http.StatusBadRequest, "battement invalide")
		return
	}
	f.mu.Lock()
	f.battements[m.ID] = battement{le: f.opt.Maintenant().Unix(), b: b, capacites: capacitesDe(r)}
	f.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

// EcrireBattements : une transaction par minute pour toutes les machines.
func (f *Flotte) EcrireBattements(ctx context.Context) error {
	f.mu.Lock()
	lot := f.battements
	f.battements = make(map[int64]battement, len(lot))
	f.mu.Unlock()
	if len(lot) == 0 {
		return nil
	}
	tx, err := f.opt.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // sans effet après Commit
	for id, x := range lot {
		demarrage := int64(0)
		if !x.b.Demarrage.IsZero() {
			demarrage = x.b.Demarrage.Unix()
		}
		if _, err := tx.ExecContext(ctx, `UPDATE machines SET battement_le = ?, version_agent = ?,
			architecture = CASE WHEN ? = '' THEN architecture ELSE ? END,
			demarrage_le = ?, maj_essai = ?, maj_echec = ?, capacites = ? WHERE id = ?`,
			x.le, x.b.Version, x.b.Architecture, x.b.Architecture, demarrage, x.b.MajEssai, x.b.MajEchec, x.capacites, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// --- Fenêtre horaire des mises à jour ------------------------------------------------

// Fenetre : les agents ne reçoivent une nouvelle version que pendant cette
// plage horaire (brief : « fenêtre horaire autorisée »). Hors de la plage,
// la réponse dit « rien à faire » : l'agent redemandera plus tard.
type Fenetre struct {
	Active bool   `json:"active"`
	Debut  string `json:"debut"` // HH:MM
	Fin    string `json:"fin"`   // HH:MM (peut passer minuit : 22:00 → 05:00)
	Fuseau string `json:"fuseau"`
}

const cleFenetre = "maj_fenetre"

// FenetreDefaut : désactivée (toute heure).
func FenetreDefaut() Fenetre { return Fenetre{Debut: "02:00", Fin: "05:00", Fuseau: "Europe/Paris"} }

func minutes(hhmm string) (int, error) {
	t, err := time.Parse("15:04", hhmm)
	if err != nil {
		return 0, fmt.Errorf("heure %q : HH:MM attendu", hhmm)
	}
	return t.Hour()*60 + t.Minute(), nil
}

// Valider : heures lisibles, fuseau connu.
func (f Fenetre) Valider() error {
	_, e1 := minutes(f.Debut)
	_, e2 := minutes(f.Fin)
	_, e3 := time.LoadLocation(f.Fuseau)
	if f.Fuseau == "" {
		e3 = errors.New("fuseau vide")
	}
	if err := errors.Join(e1, e2, e3); err != nil {
		return err
	}
	if f.Debut == f.Fin {
		return errors.New("début et fin identiques")
	}
	return nil
}

// Contient dit si t est dans la fenêtre (toujours vrai si elle est inactive).
func (f Fenetre) Contient(t time.Time) bool {
	if !f.Active {
		return true
	}
	loc, err := time.LoadLocation(f.Fuseau)
	if err != nil {
		return true
	}
	d, e1 := minutes(f.Debut)
	fi, e2 := minutes(f.Fin)
	if e1 != nil || e2 != nil {
		return true
	}
	l := t.In(loc)
	m := l.Hour()*60 + l.Minute()
	if d < fi {
		return m >= d && m < fi
	}
	return m >= d || m < fi // passe minuit
}

// LireFenetre : réglage en base ou défaut.
func LireFenetre(ctx context.Context, db *sql.DB) (Fenetre, error) {
	f := FenetreDefaut()
	var v string
	err := db.QueryRowContext(ctx, `SELECT valeur FROM reglages WHERE cle = ?`, cleFenetre).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return f, nil
	}
	if err != nil {
		return f, err
	}
	if err := json.Unmarshal([]byte(v), &f); err != nil {
		return FenetreDefaut(), err
	}
	return f, nil
}

// EcrireFenetre : enregistre la fenêtre (audit).
func EcrireFenetre(ctx context.Context, db *sql.DB, f Fenetre, qui string, maintenant time.Time) error {
	if err := f.Valider(); err != nil {
		return err
	}
	b, _ := json.Marshal(f)
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // sans effet après Commit
	if _, err := tx.ExecContext(ctx, `INSERT INTO reglages(cle, valeur, modifie_le, modifie_par) VALUES (?, ?, ?, ?)
		ON CONFLICT(cle) DO UPDATE SET valeur = excluded.valeur, modifie_le = excluded.modifie_le, modifie_par = excluded.modifie_par`,
		cleFenetre, string(b), maintenant.Unix(), qui); err != nil {
		return err
	}
	if err := audit(ctx, tx, maintenant.Unix(), qui, "maj_fenetre", "", "", string(b), ""); err != nil {
		return err
	}
	return tx.Commit()
}

// CibleGlobale : version assignée à toutes les machines sans version propre.
func CibleGlobale(ctx context.Context, db *sql.DB) (string, error) {
	var v string
	err := db.QueryRowContext(ctx, `SELECT valeur FROM reglages WHERE cle = ?`, cleCible).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if len(v) >= 2 {
		v = v[1 : len(v)-1]
	}
	return v, err
}

// VersionPubliee : une version de l'agent publiée sur le serveur.
type VersionPubliee struct {
	Version, Architecture, SHA256, PublieePar string
	Taille, PublieeLe                         int64
	RetourAutorise                            bool
}

// ListerVersions : versions publiées, plus récentes d'abord.
func ListerVersions(ctx context.Context, db *sql.DB) ([]VersionPubliee, error) {
	rows, err := db.QueryContext(ctx, `SELECT version, architecture, sha256, taille, retour_autorise, publiee_le, publiee_par
		FROM versions_agent ORDER BY publiee_le DESC LIMIT 100`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []VersionPubliee
	for rows.Next() {
		var v VersionPubliee
		var r int
		if err := rows.Scan(&v.Version, &v.Architecture, &v.SHA256, &v.Taille, &r, &v.PublieeLe, &v.PublieePar); err != nil {
			return nil, err
		}
		v.RetourAutorise = r == 1
		out = append(out, v)
	}
	return out, rows.Err()
}
