// SPDX-License-Identifier: GPL-3.0-or-later

package auth

import (
	"context"
	"database/sql"
	"net/http"
	"strings"
	"time"
)

// Appareils de confiance (SPEC § 78) : « Rester connecté sur cet appareil ».
//   - le navigateur garde un cookie daté (il survit à la fermeture de la PWA,
//     qu'iOS fait souvent en arrière-plan) ;
//   - le serveur garde l'empreinte SHA-256 de l'identifiant sur disque (la
//     session survit à un redémarrage, par exemple une mise à jour) ;
//   - durées propres : 30 j au plus, 7 j sans usage (réglables) ;
//   - mot de passe changé (génération), compte désactivé : refusé ;
//   - les actions sensibles demandent toujours le second facteur.

// Confiance : un appareil de confiance (ligne de la base).
type Confiance struct {
	Empreinte     string
	UtilisateurID int64
	Nom           string // nom du compte (liste de la page Sécurité)
	Generation    int64
	CreeLe, VuLe  time.Time
	Adresse       string
	Appareil      string
}

// Ref : début de l'empreinte, pour la désigner dans un formulaire.
func (a Confiance) Ref() string { return a.Empreinte[:16] }

// EmpreinteDe : empreinte de l'identifiant d'un cookie.
func EmpreinteDe(id string) string { return empreinteID(id) }

// EnregistrerConfiance : appareil devenu de confiance (connexion complète).
func EnregistrerConfiance(ctx context.Context, db *sql.DB, a Confiance) error {
	_, err := db.ExecContext(ctx, `INSERT INTO appareils_confiance(empreinte, utilisateur_id, generation, cree_le, vu_le, adresse, appareil)
		VALUES (?, ?, ?, ?, ?, ?, ?)`, a.Empreinte, a.UtilisateurID, a.Generation, a.CreeLe.Unix(), a.VuLe.Unix(), a.Adresse, a.Appareil)
	return err
}

// LireConfiance : l'appareil d'un identifiant de cookie (ok = false si
// inconnu). La validité (durées, compte) est vérifiée par l'appelant.
func LireConfiance(ctx context.Context, db *sql.DB, id string) (Confiance, bool) {
	if id == "" || len(id) > 64 {
		return Confiance{}, false
	}
	a := Confiance{Empreinte: empreinteID(id)}
	var cree, vu int64
	err := db.QueryRowContext(ctx, `SELECT utilisateur_id, generation, cree_le, vu_le, adresse, appareil FROM appareils_confiance WHERE empreinte = ?`,
		a.Empreinte).Scan(&a.UtilisateurID, &a.Generation, &cree, &vu, &a.Adresse, &a.Appareil)
	if err != nil {
		return Confiance{}, false
	}
	a.CreeLe, a.VuLe = time.Unix(cree, 0), time.Unix(vu, 0)
	return a, true
}

// ConfianceExpiree : plus de max depuis la connexion, ou plus d'inactivite
// sans usage.
func ConfianceExpiree(a Confiance, t time.Time, inactivite, max time.Duration) bool {
	return t.Sub(a.CreeLe) > max || t.Sub(a.VuLe) > inactivite
}

// ToucherConfiance : dernier usage (appelé toutes les 10 min au plus).
func ToucherConfiance(ctx context.Context, db *sql.DB, empreinte string, t time.Time) error {
	_, err := db.ExecContext(ctx, `UPDATE appareils_confiance SET vu_le = ? WHERE empreinte = ?`, t.Unix(), empreinte)
	return err
}

// OublierConfiance : déconnexion, ou « Oublier cet appareil ».
func OublierConfiance(ctx context.Context, db *sql.DB, empreinte string) error {
	_, err := db.ExecContext(ctx, `DELETE FROM appareils_confiance WHERE empreinte = ?`, empreinte)
	return err
}

// OublierConfianceRef : par le début de l'empreinte (formulaire de la page
// Sécurité) ; 16 caractères hexadécimaux exactement.
func OublierConfianceRef(ctx context.Context, db *sql.DB, ref string) (int64, error) {
	if len(ref) != 16 || strings.Trim(ref, "0123456789abcdef") != "" {
		return 0, nil
	}
	r, err := db.ExecContext(ctx, `DELETE FROM appareils_confiance WHERE substr(empreinte, 1, 16) = ?`, ref)
	if err != nil {
		return 0, err
	}
	return r.RowsAffected()
}

// OublierConfianceUtilisateur : tous les appareils d'un compte (sauf garder,
// empreinte de l'appareil en cours) ; mot de passe changé, compte désactivé.
func OublierConfianceUtilisateur(ctx context.Context, db *sql.DB, utilisateur int64, garder string) error {
	_, err := db.ExecContext(ctx, `DELETE FROM appareils_confiance WHERE utilisateur_id = ? AND empreinte != ?`, utilisateur, garder)
	return err
}

// NettoyerConfiance : appareils expirés.
func NettoyerConfiance(ctx context.Context, db *sql.DB, t time.Time, inactivite, max time.Duration) error {
	_, err := db.ExecContext(ctx, `DELETE FROM appareils_confiance WHERE cree_le < ? OR vu_le < ?`, t.Add(-max).Unix(), t.Add(-inactivite).Unix())
	return err
}

// ListerConfiance : appareils de confiance de tous les comptes (page Sécurité).
func ListerConfiance(ctx context.Context, db *sql.DB) ([]Confiance, error) {
	rows, err := db.QueryContext(ctx, `SELECT a.empreinte, a.utilisateur_id, u.nom, a.generation, a.cree_le, a.vu_le, a.adresse, a.appareil
		FROM appareils_confiance a JOIN utilisateurs u ON u.id = a.utilisateur_id ORDER BY a.vu_le DESC`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }() // lecture seule
	var out []Confiance
	for rows.Next() {
		var a Confiance
		var cree, vu int64
		if rows.Scan(&a.Empreinte, &a.UtilisateurID, &a.Nom, &a.Generation, &cree, &vu, &a.Adresse, &a.Appareil) == nil {
			a.CreeLe, a.VuLe = time.Unix(cree, 0), time.Unix(vu, 0)
			out = append(out, a)
		}
	}
	return out, rows.Err()
}

// Restaurer : remet en mémoire la session d'un appareil de confiance (après
// la fermeture de la PWA ou un redémarrage du serveur), sous le même
// identifiant de cookie ; même jeton CSRF qu'avant (tiré de l'identifiant :
// un formulaire déjà affiché reste valable). Renvoie une copie.
func (m *Magasin) Restaurer(id string, s *Session, t time.Time) *Session {
	s.Empreinte = empreinteID(id)
	s.CSRF = csrfDe(id)
	s.Etape, s.Confiance, s.Vue = EtapeComplet, true, t
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.tout) >= sessionsMax {
		m.evincer()
	}
	m.tout[s.Empreinte] = s
	c := *s
	return &c
}

// PoserCookieConfiance : comme PoserCookie, avec une date d'expiration
// (le cookie survit à la fermeture de l'application).
func PoserCookieConfiance(w http.ResponseWriter, id string, duree time.Duration) {
	http.SetCookie(w, &http.Cookie{Name: NomCookie, Value: id, Path: "/", HttpOnly: true, Secure: true,
		SameSite: http.SameSiteStrictMode, MaxAge: int(duree / time.Second)})
}

// AppareilDe : « iPhone · Safari », « Windows · Firefox »… d'après
// l'en-tête User-Agent (indicatif : liste de la page Sécurité).
func AppareilDe(ua string) string {
	sys := ""
	for _, x := range [][2]string{{"iPhone", "iPhone"}, {"iPad", "iPad"}, {"Android", "Android"}, {"Windows", "Windows"}, {"Mac OS", "Mac"}, {"Linux", "Linux"}} {
		if strings.Contains(ua, x[0]) {
			sys = x[1]
			break
		}
	}
	nav := ""
	for _, x := range [][2]string{{"Edg/", "Edge"}, {"Firefox/", "Firefox"}, {"CriOS", "Chrome"}, {"Chrome/", "Chrome"}, {"Safari/", "Safari"}} {
		if strings.Contains(ua, x[0]) {
			nav = x[1]
			break
		}
	}
	switch {
	case sys != "" && nav != "":
		return sys + " · " + nav
	case sys != "":
		return sys
	case nav != "":
		return nav
	}
	return "Appareil inconnu"
}
