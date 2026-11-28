// SPDX-License-Identifier: GPL-3.0-or-later

package flotte

import (
	"context"
	"database/sql"
	"errors"
	"net/netip"
	"strings"
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

	// Alertes « adresse nouvelle » tenues en même temps (revue du 28/11,
	// M6) : 20 par machine (au-delà, c'est une attaque ou un agent abîmé :
	// les 20 premières suffisent à le voir), 200 pour tout le parc. Lignes
	// lues en base : 2 000 au plus (deux fois le plafond par machine, pour
	// les adresses « connues » écartées ensuite).
	maxNouvellesParMachine = 20
	maxNouvellesSSH        = 200
	maxLignesNouvelles     = 2000
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
	cs, err := ListerSSHConnues(ctx, db)
	if err != nil {
		return false, err
	}
	connue := Connue(cs, machine, a)
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
	cs, err := ListerSSHConnues(ctx, db)
	if err != nil {
		return nil, err
	}
	// Sélection équitable (revue du 28/11, M6) : avant, « LIMIT 500 » sans
	// ordre puis 200 pour tout le parc ; une machine compromise qui
	// signalait des centaines d'adresses évinçait la vraie alerte d'une
	// autre machine. Maintenant : rang de chaque adresse dans sa machine
	// (les plus anciennes d'abord : l'alerte ne saute pas d'une adresse à
	// l'autre), puis tri par rang : le 1er rang de chaque machine passe
	// avant le 2e de n'importe quelle autre.
	rows, err := db.QueryContext(ctx, `SELECT machine, adresse, utilisateur FROM (
		SELECT machine, adresse, utilisateur,
			ROW_NUMBER() OVER (PARTITION BY machine ORDER BY nouvelle_jusqua, adresse) AS rang
		FROM ssh_adresses WHERE nouvelle_jusqua > ?)
		WHERE rang <= ? ORDER BY rang, machine LIMIT ?`, t.Unix(), 2*maxNouvellesParMachine, maxLignesNouvelles)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ConnexionSSH
	parMachine := map[string]int{}
	for rows.Next() {
		var c ConnexionSSH
		if err := rows.Scan(&c.Machine, &c.Adresse, &c.Utilisateur); err != nil {
			return nil, err
		}
		a, err := netip.ParseAddr(c.Adresse)
		if err != nil || Connue(cs, c.Machine, a) {
			continue
		}
		if parMachine[c.Machine] >= maxNouvellesParMachine || len(out) >= maxNouvellesSSH {
			continue
		}
		parMachine[c.Machine]++
		out = append(out, c)
	}
	return out, rows.Err()
}

// SSHConnue : adresse ou plage connue (retour du 27/11), pour toutes les
// machines (Machine vide) ou une seule.
type SSHConnue struct {
	ID      int64
	Prefixe netip.Prefix
	Machine string
	Note    string
	Par     string
	Le      time.Time
}

// maxSSHConnues : lignes au plus (la liste est relue à chaque connexion).
const maxSSHConnues = 500

// ListerSSHConnues : liste complète (petite), triée par plage puis machine.
func ListerSSHConnues(ctx context.Context, db *sql.DB) ([]SSHConnue, error) {
	rows, err := db.QueryContext(ctx, `SELECT id, prefixe, machine, note, par, le FROM ssh_connues ORDER BY prefixe, machine LIMIT ?`, maxSSHConnues)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SSHConnue
	for rows.Next() {
		var c SSHConnue
		var p string
		var le int64
		if err := rows.Scan(&c.ID, &p, &c.Machine, &c.Note, &c.Par, &le); err != nil {
			return nil, err
		}
		pr, err := netip.ParsePrefix(p)
		if err != nil {
			continue // ligne abîmée : ignorée (jamais écrite par l'interface)
		}
		c.Prefixe, c.Le = pr, time.Unix(le, 0)
		out = append(out, c)
	}
	return out, rows.Err()
}

// Connue : a est dans une plage connue pour cette machine (ou toutes).
func Connue(cs []SSHConnue, machine string, a netip.Addr) bool {
	a = a.Unmap()
	for _, c := range cs {
		if (c.Machine == "" || c.Machine == machine) && c.Prefixe.Contains(a) {
			return true
		}
	}
	return false
}

// LirePlage : « 203.0.113.7 » → /32, « 2001:db8::1 » → /128, ou
// « 203.0.113.0/24 ». Refuse les plages trop larges (moins de /8 en IPv4,
// /16 en IPv6) : « tout Internet est connu » couperait l'alerte.
func LirePlage(s string) (netip.Prefix, error) {
	s = strings.TrimSpace(s)
	var p netip.Prefix
	if strings.Contains(s, "/") {
		x, err := netip.ParsePrefix(s)
		if err != nil {
			return p, errors.New("plage illisible (ex. 203.0.113.0/24)")
		}
		p = x.Masked()
	} else {
		a, err := netip.ParseAddr(s)
		if err != nil {
			return p, errors.New("adresse illisible")
		}
		a = a.Unmap()
		p = netip.PrefixFrom(a, a.BitLen())
	}
	if (p.Addr().Is4() && p.Bits() < 8) || (p.Addr().Is6() && p.Bits() < 16) {
		return p, errors.New("plage trop large (au moins /8 en IPv4, /16 en IPv6)")
	}
	return p, nil
}

// AjouterSSHConnue : adresse ou plage connue (machine vide : toutes) ; les
// alertes en cours qu'elle couvre se ferment au tour suivant.
func AjouterSSHConnue(ctx context.Context, db *sql.DB, plage, machine, note, qui string, t time.Time) (netip.Prefix, error) {
	p, err := LirePlage(plage)
	if err != nil {
		return p, err
	}
	note = strings.TrimSpace(note)
	if len([]rune(note)) > 100 {
		return p, errors.New("note : 100 caractères au plus")
	}
	var n int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM ssh_connues`).Scan(&n); err != nil {
		return p, err
	}
	if n >= maxSSHConnues {
		return p, errors.New("500 adresses connues au plus")
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO ssh_connues(prefixe, machine, note, par, le) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(prefixe, machine) DO UPDATE SET note = excluded.note, par = excluded.par, le = excluded.le`,
		p.String(), machine, note, qui, t.Unix()); err != nil {
		return p, err
	}
	return p, nil
}

// RetirerSSHConnue : par identifiant ; renvoie la ligne retirée (audit).
func RetirerSSHConnue(ctx context.Context, db *sql.DB, id int64) (string, error) {
	var p, m string
	if err := db.QueryRowContext(ctx, `SELECT prefixe, machine FROM ssh_connues WHERE id = ?`, id).Scan(&p, &m); err != nil {
		return "", errors.New("adresse connue introuvable")
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM ssh_connues WHERE id = ?`, id); err != nil {
		return "", err
	}
	if m == "" {
		m = "toutes les machines"
	}
	return p + " (" + m + ")", nil
}
