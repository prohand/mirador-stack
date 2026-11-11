// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"errors"
	"html/template"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"mirador-stack/internal/server/alertes"
	"mirador-stack/internal/server/secrets"
)

// --- Mon compte --------------------------------------------------------------------

type donneesCompte struct {
	U             Utilisateur
	Secret, Etape string
	QR            template.URL
}

func (c *Console) pageCompte(q *req) {
	d := donneesCompte{U: q.u}
	if q.s.SecretPropose != "" {
		d.QR, _ = ImageQR(URITOTP(q.u.Nom, q.s.SecretPropose))
		d.Secret = grouper(q.s.SecretPropose)
	}
	c.rendre(q, "compte", "Mon compte", "compte", d)
}

// postMotDePasse : ancien mot de passe + code 2FA + nouveau (deux fois).
// Les autres sessions du compte sont fermées.
func (c *Console) postMotDePasse(q *req) {
	err := func() error {
		// Verrou vérifié AVANT Argon2id (sinon la réponse dirait si le mot
		// de passe est juste, même verrouillé) ; même message pour un mot
		// de passe ou un code faux.
		if c.sensibleBloque(q) {
			return errTropEssais
		}
		finir, libre := c.debuterSensible(q)
		if !libre {
			return errTropEssais
		}
		bon := Comparer(q.u.MotDePasse, q.r.PostFormValue("actuel"))
		if !bon {
			err := c.echecSensible(q, "mot de passe faux (changement)")
			finir()
			if err != nil {
				return err
			}
			return errors.New("mot de passe actuel ou code 2FA faux")
		}
		finir() // avant verifierCode, qui réserve son propre essai
		if err := c.verifierCode(q); err != nil {
			if errors.Is(err, errTropEssais) || errors.Is(err, errSessionFermee) {
				return err
			}
			return errors.New("mot de passe actuel ou code 2FA faux")
		}
		n := q.r.PostFormValue("nouveau")
		if n != q.r.PostFormValue("nouveau2") {
			return errors.New("les deux nouveaux mots de passe sont différents")
		}
		if err := DefinirMotDePasse(q.r.Context(), c.db, q.u.Nom, n, q.qui(), q.ip.String(), q.t); err != nil {
			return err
		}
		return c.garderSession(q)
	}()
	c.retour(q, "/compte", err, "mot de passe changé ; les autres sessions sont fermées")
}

// garderSession : après un changement qui ferme les sessions du compte
// (generation + 1), la session courante continue avec un nouvel identifiant.
func (c *Console) garderSession(q *req) error {
	c.oublierComptes()
	u, err := LireUtilisateurID(q.r.Context(), c.db, q.u.ID)
	if err != nil {
		return err
	}
	c.sessions.fermerUtilisateur(u.ID, q.s.empreinte)
	id := c.sessions.renouveler(q.s.empreinte, q.t, func(s *Session) { s.Generation = u.Generation; s.SecretPropose = "" })
	q.s = c.sessions.lire(id)
	if q.s == nil {
		return errors.New("session perdue")
	}
	poserCookie(q.w, id)
	return nil
}

// post2FANouvelle : préparer une nouvelle 2FA (code actuel demandé). Elle
// ne remplace l'ancienne qu'après un code juste de la nouvelle : on ne peut
// pas s'enfermer dehors avec un téléphone mal réglé.
func (c *Console) post2FANouvelle(q *req) {
	err := c.verifierCode(q)
	if err == nil {
		var secret string
		if secret, err = NouveauSecretTOTP(); err == nil {
			c.sessions.modifier(q.s.empreinte, func(s *Session) { s.SecretPropose = secret })
		}
	}
	c.retour(q, "/compte", err, "scanner le nouveau QR code puis saisir un code de la nouvelle 2FA")
}

func (c *Console) post2FAConfirmer(q *req) {
	err := func() error {
		if q.s.SecretPropose == "" {
			return errors.New("aucune nouvelle 2FA en cours")
		}
		pas, ok := VerifierTOTP(q.s.SecretPropose, q.r.PostFormValue("code"), q.t, 0)
		if !ok {
			return errors.New("code faux : la 2FA actuelle est gardée")
		}
		ch, err := secrets.Chiffrer(c.opt.Cle, usageTOTP, q.s.SecretPropose)
		if err != nil {
			return err
		}
		if err := DefinirTOTP(q.r.Context(), c.db, q.u.Nom, ch, pas, q.qui(), q.ip.String(), q.t); err != nil {
			return err
		}
		return c.garderSession(q)
	}()
	c.retour(q, "/compte", err, "nouvelle 2FA enregistrée ; les autres sessions sont fermées")
}

func (c *Console) post2FAAbandonner(q *req) {
	c.sessions.modifier(q.s.empreinte, func(s *Session) { s.SecretPropose = "" })
	c.retour(q, "/compte", nil, "nouvelle 2FA abandonnée, l'actuelle est gardée")
}

// --- Utilisateurs (admin) ----------------------------------------------------------

func (c *Console) pageUtilisateurs(q *req) {
	us, err := ListerUtilisateurs(q.r.Context(), c.db)
	if err != nil {
		c.flash(q, "", err.Error())
	}
	c.rendre(q, "utilisateurs", "Utilisateurs", "utilisateurs", us)
}

// postUtilisateur : toutes les actions sur les comptes. Sensibles (brief :
// « changement de sécurité ») : code 2FA de l'admin demandé à chaque fois.
func (c *Console) postUtilisateur(q *req) {
	ctx := q.r.Context()
	action := q.r.PostFormValue("action")
	nom := strings.TrimSpace(q.r.PostFormValue("nom"))
	ip := q.ip.String()
	ok := "fait"
	// Lu AVANT l'action : après une suppression, le compte n'existe plus
	// et ses sessions doivent quand même être fermées (revue du 11/11).
	cible, _ := LireUtilisateur(ctx, c.db, nom)
	err := c.verifierCode(q)
	if err == nil {
		switch action {
		case "creer":
			err = CreerUtilisateur(ctx, c.db, nom, q.r.PostFormValue("role"), q.r.PostFormValue("mot_de_passe"), q.qui(), ip, q.t)
			ok = "compte créé : la personne crée sa 2FA à sa première connexion"
		case "role":
			role := q.r.PostFormValue("role")
			if nom == q.u.Nom && role != q.u.Role {
				// Changer son propre rôle peut enfermer dehors : à confirmer.
				if err = DefinirRole(ctx, c.db, nom, role, q.qui(), ip, q.t); err == nil {
					_, err = DemanderConfirmationRole(ctx, c.db, nom, q.u.Role, q.qui(), q.t)
					ok = "rôle changé : confirmer dans les 2 minutes (bandeau en haut), sinon il revient"
				}
			} else {
				err = DefinirRole(ctx, c.db, nom, role, q.qui(), ip, q.t)
			}
		case "desactiver", "activer":
			if nom == q.u.Nom {
				err = errors.New("on ne peut pas désactiver son propre compte")
				break
			}
			err = DefinirDesactive(ctx, c.db, nom, action == "desactiver", q.qui(), ip, q.t)
		case "reinitialiser_2fa":
			err = ReinitialiserTOTP(ctx, c.db, nom, q.qui(), ip, q.t)
			ok = "2FA effacée : la personne la recrée à sa prochaine connexion"
		case "mot_de_passe":
			err = DefinirMotDePasse(ctx, c.db, nom, q.r.PostFormValue("mot_de_passe"), q.qui(), ip, q.t)
			ok = "mot de passe changé, sessions de ce compte fermées"
		case "supprimer":
			if nom == q.u.Nom {
				err = errors.New("on ne peut pas supprimer son propre compte")
				break
			}
			err = SupprimerUtilisateur(ctx, c.db, nom, q.qui(), ip, q.t)
		default:
			err = errors.New("action inconnue")
		}
	}
	if err == nil {
		if cible.ID != 0 && action != "role" {
			c.sessions.fermerUtilisateur(cible.ID, q.s.empreinte)
		}
		c.oublierComptes()
	}
	c.retour(q, "/utilisateurs", err, ok)
}

// --- Sécurité (admin) --------------------------------------------------------------

type donneesSecurite struct {
	S        Securite
	Defaut   Securite
	Sessions []Session
	MonIP    string
}

func (c *Console) pageSecurite(q *req) {
	s, err := LireSecurite(q.r.Context(), c.db)
	if err != nil {
		c.flash(q, "", err.Error())
	}
	ss := c.sessions.lister()
	sort.Slice(ss, func(i, j int) bool { return ss[i].Vue.After(ss[j].Vue) })
	c.rendre(q, "securite", "Sécurité", "securite", donneesSecurite{S: s, Defaut: SecuriteDefaut(), Sessions: ss, MonIP: q.ip.String()})
}

func lireDureeForm(q *req, champ string) (alertes.Duree, error) {
	d, err := alertes.LireDuree(strings.TrimSpace(q.r.PostFormValue(champ)))
	if err != nil {
		return 0, errors.New(champ + " : durée illisible (ex. 30m, 12h)")
	}
	return d, nil
}

func (c *Console) postSecurite(q *req) {
	err := func() error {
		if err := c.verifierCode(q); err != nil {
			return err
		}
		var s Securite
		if q.r.PostFormValue("defaut") != "" {
			s = SecuriteDefaut()
		} else {
			var e1, e2, e3, e4 error
			s.Inactivite, e1 = lireDureeForm(q, "inactivite")
			s.DureeMax, e2 = lireDureeForm(q, "duree_max")
			s.Verrou, e3 = lireDureeForm(q, "verrou")
			s.Echecs, e4 = strconv.Atoi(q.r.PostFormValue("echecs"))
			if e4 != nil {
				e4 = errors.New("échecs : nombre attendu")
			}
			if err := errors.Join(e1, e2, e3, e4); err != nil {
				return err
			}
			s.IPs = strings.TrimSpace(q.r.PostFormValue("ips"))
		}
		if err := s.Valider(); err != nil {
			return err
		}
		if !s.AdresseAutorisee(q.ip) {
			return errors.New("votre adresse actuelle (" + q.ip.String() + ") n'est pas dans la liste : refusé")
		}
		id, err := EcrireSecurite(q.r.Context(), c.db, s, q.qui(), q.ip.String(), q.t)
		if err != nil {
			return err
		}
		c.securite.Store(&s)
		if id != 0 {
			c.flash(q, "liste d'adresses changée : la confirmer dans les 2 minutes (bandeau en haut), sinon l'ancienne revient", "")
			http.Redirect(q.w, q.r, "/securite", http.StatusSeeOther)
			return errRedirige
		}
		return nil
	}()
	if errors.Is(err, errRedirige) {
		return
	}
	c.retour(q, "/securite", err, "réglages de sécurité enregistrés")
}

// errRedirige : la réponse est déjà faite.
var errRedirige = errors.New("redirigé")

// postConfirmer : confirmation d'un changement « à confirmer ». Le fait
// même que cette requête arrive prouve que la nouvelle configuration laisse
// entrer.
// Seuls l'auteur du changement ou un admin peuvent le confirmer : un compte
// en lecture seule ne doit pas empêcher l'annulation automatique qui protège
// l'admin qui s'est enfermé dehors (revue du 11/11).
func (c *Console) postConfirmer(q *req) {
	id, _ := strconv.ParseInt(q.r.PostFormValue("id"), 10, 64)
	var par string
	_ = c.db.QueryRowContext(q.r.Context(), `SELECT par FROM a_confirmer WHERE id = ?`, id).Scan(&par)
	if !q.admin() && par != q.qui() {
		c.retour(q, refererLocal(q.r), errors.New("seul l'auteur du changement ou un administrateur peut le confirmer"), "")
		return
	}
	err := Confirmer(q.r.Context(), c.db, id, q.qui(), q.ip.String(), q.t)
	c.retour(q, refererLocal(q.r), err, "changement confirmé")
}

// postFermerSession : un admin ferme une session ouverte (vol de cookie
// soupçonné, poste oublié).
func (c *Console) postFermerSession(q *req) {
	e := q.r.PostFormValue("session")
	for _, s := range c.sessions.lister() {
		if s.Ref() == e {
			c.sessions.supprimer(s.empreinte)
			c.retour(q, "/securite", nil, "session fermée")
			return
		}
	}
	c.retour(q, "/securite", errors.New("session introuvable"), "")
}

// refererLocal : revenir sur la page d'où vient le formulaire (chemin
// local seulement : pas de redirection ouverte vers un autre site).
func refererLocal(r *http.Request) string {
	ref := r.Header.Get("Referer")
	if i := strings.Index(ref, "://"); i >= 0 {
		ref = ref[i+3:]
		if j := strings.Index(ref, "/"); j >= 0 {
			ref = ref[j:]
		} else {
			ref = "/"
		}
	}
	if !strings.HasPrefix(ref, "/") || strings.HasPrefix(ref, "//") || strings.ContainsAny(ref, "\\\r\n") {
		return "/"
	}
	return ref
}
