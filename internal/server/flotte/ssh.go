// SPDX-License-Identifier: GPL-3.0-or-later

package flotte

import (
	"context"
	"database/sql"
	"errors"
	"net/netip"
	"time"

	"mirador-stack/internal/protocole"
)

// Connexions SSH depuis une adresse nouvelle (demande du 27/11, maquette
// validée). L'agent signale chaque connexion réussie (une fois par adresse
// et utilisateur toutes les 10 min) ; le serveur ne garde que les adresses
// publiques. Une adresse jamais vue sur la machine depuis 90 jours, et pas
// « connue », ouvre l'alerte SshAdresseNouvelle (métrique interne tenue 24 h).

const (
	gardeSSH         = 90 * 24 * time.Hour
	dureeAlerteSSH   = 24 * time.Hour
	maxAdressesSSH   = 2000 // par machine : au-delà, les plus anciennes partent
	MetriqueSSHNeuve = "mirador_ssh_adresse_nouvelle"
)

// cgnat : 100.64.0.0/10 (adresses de l'opérateur, pas Internet).
var cgnat = netip.MustParsePrefix("100.64.0.0/10")

// AdresseLocale : LAN, boucle, lien local, CGNAT : jamais signalée.
func AdresseLocale(a netip.Addr) bool {
	a = a.Unmap()
	return a.IsPrivate() || a.IsLoopback() || a.IsLinkLocalUnicast() || a.IsUnspecified() || cgnat.Contains(a)
}

// NoterConnexionSSH : enregistre la connexion ; nouvelle dit si elle ouvre
// une alerte (et elle est alors notée dans le journal des événements).
func NoterConnexionSSH(ctx context.Context, db *sql.DB, machineID int64, machine string, e protocole.EvenementAgent, t time.Time) (nouvelle bool, err error) {
	a, err := netip.ParseAddr(e.Objet)
	if err != nil {
		return false, err
	}
	if AdresseLocale(a) {
		return false, nil
	}
	adr := a.Unmap().String()
	var x int
	connue := db.QueryRowContext(ctx, `SELECT 1 FROM ssh_connues WHERE adresse = ?`, adr).Scan(&x) == nil
	var derniere int64
	err = db.QueryRowContext(ctx, `SELECT derniere FROM ssh_adresses WHERE machine = ? AND adresse = ?`, machine, adr).Scan(&derniere)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		nouvelle, err = !connue, nil
	case err != nil:
		return false, err
	default:
		nouvelle = !connue && derniere < t.Add(-gardeSSH).Unix()
	}
	jusqua := int64(0)
	if nouvelle {
		jusqua = t.Add(dureeAlerteSSH).Unix()
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO ssh_adresses(machine, adresse, utilisateur, premiere, derniere, nouvelle_jusqua)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(machine, adresse) DO UPDATE SET utilisateur = excluded.utilisateur, derniere = excluded.derniere,
			nouvelle_jusqua = MAX(nouvelle_jusqua, excluded.nouvelle_jusqua)`,
		machine, adr, e.Texte, t.Unix(), t.Unix(), jusqua); err != nil {
		return false, err
	}
	// Ménage : 90 jours, et un plafond par machine.
	if _, err := db.ExecContext(ctx, `DELETE FROM ssh_adresses WHERE derniere < ? OR (machine = ? AND adresse IN (
		SELECT adresse FROM ssh_adresses WHERE machine = ? ORDER BY derniere DESC LIMIT -1 OFFSET ?))`,
		t.Add(-gardeSSH).Unix(), machine, machine, maxAdressesSSH); err != nil {
		return nouvelle, err
	}
	if nouvelle {
		err = NoterPanne(ctx, db, machineID, t, e)
	}
	return nouvelle, err
}

// ConnexionSSH : adresse nouvelle dont l'alerte est tenue.
type ConnexionSSH struct {
	Machine, Adresse, Utilisateur string
}

// SSHNouvelles : adresses nouvelles à signaler maintenant (métrique interne).
func SSHNouvelles(ctx context.Context, db *sql.DB, t time.Time) ([]ConnexionSSH, error) {
	rows, err := db.QueryContext(ctx, `SELECT machine, adresse, utilisateur FROM ssh_adresses
		WHERE nouvelle_jusqua > ? AND adresse NOT IN (SELECT adresse FROM ssh_connues) LIMIT 200`, t.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ConnexionSSH
	for rows.Next() {
		var c ConnexionSSH
		if err := rows.Scan(&c.Machine, &c.Adresse, &c.Utilisateur); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// MarquerSSHConnue : « C'est moi » : l'adresse ne déclenche plus rien, sur
// aucune machine ; les alertes en cours se ferment au tour suivant.
func MarquerSSHConnue(ctx context.Context, db *sql.DB, adresse, qui string, t time.Time) error {
	a, err := netip.ParseAddr(adresse)
	if err != nil {
		return errors.New("adresse invalide")
	}
	adr := a.Unmap().String()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // sans effet après Commit
	if _, err := tx.ExecContext(ctx, `INSERT INTO ssh_connues(adresse, par, le) VALUES (?, ?, ?)
		ON CONFLICT(adresse) DO UPDATE SET par = excluded.par, le = excluded.le`, adr, qui, t.Unix()); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE ssh_adresses SET nouvelle_jusqua = 0 WHERE adresse = ?`, adr); err != nil {
		return err
	}
	return tx.Commit()
}
