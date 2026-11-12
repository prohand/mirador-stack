// SPDX-License-Identifier: GPL-3.0-or-later

package server

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"fmt"
	"io"
	"mirador-stack/internal/server/auth"
	"text/tabwriter"
	"time"

	"mirador-stack/internal/server/console"
)

// AideConsole : commandes de secours de l'interface web. Elles servent
// quand on ne peut plus entrer (2FA perdue, liste d'adresses fausse) : il
// faut alors un accès au VPS (docker exec), ce qui vaut preuve d'identité.
const AideConsole = `

Interface web (secours) :
  utilisateurs                       comptes de l'interface
  utilisateur-secours NOM            nouveau mot de passe provisoire (affiché une fois) et 2FA
                                     effacée ; le compte est réactivé ; ses sessions sont fermées
                                     (effet en 10 s au plus)
  console-ips-vider                  vide la liste d'adresses autorisées de l'interface`

func commandeConsole(ctx context.Context, db *sql.DB, nom string, pos []string, sortie io.Writer, t time.Time) (traite bool, err error) {
	switch nom {
	case "utilisateurs":
		us, err := auth.ListerUtilisateurs(ctx, db)
		if err != nil {
			return true, err
		}
		tw := tabwriter.NewWriter(sortie, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "NOM\tRÔLE\t2FA\tÉTAT\tDERNIÈRE CONNEXION")
		for _, u := range us {
			deux, etat, vu := "oui", "actif", "jamais"
			if u.TOTP == "" {
				deux = "à créer"
			}
			if u.Desactive {
				etat = "désactivé"
			}
			if u.ConnecteLe > 0 {
				vu = time.Unix(u.ConnecteLe, 0).Format("02/01/2006 15:04")
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", u.Nom, u.Role, deux, etat, vu)
		}
		return true, tw.Flush()
	case "utilisateur-secours":
		if len(pos) != 1 {
			return true, fmt.Errorf("1 argument attendu ; voir « aide »")
		}
		b := make([]byte, 18)
		if _, err := rand.Read(b); err != nil {
			return true, err
		}
		mdp := base64.RawURLEncoding.EncodeToString(b)
		if err := auth.DefinirMotDePasse(ctx, db, pos[0], mdp, qui, "", t); err != nil {
			return true, err
		}
		if err := auth.ReinitialiserTOTP(ctx, db, pos[0], qui, "", t); err != nil {
			return true, err
		}
		if err := auth.DefinirDesactive(ctx, db, pos[0], false, qui, "", t); err != nil {
			return true, err
		}
		// Le serveur en marche lève ses verrous de connexion (5 s au plus).
		if err := console.DemanderLeveeVerrous(ctx, db, qui, t); err != nil {
			return true, err
		}
		fmt.Fprintf(sortie, "Mot de passe provisoire de %s (affiché une seule fois) :\n%s\n"+
			"Se connecter, créer la nouvelle 2FA, puis changer ce mot de passe (page « Mon compte »).\n", pos[0], mdp)
		return true, nil
	case "console-ips-vider":
		s, err := console.LireSecurite(ctx, db)
		if err != nil {
			s = console.SecuriteDefaut()
		}
		s.IPs = ""
		if _, err := console.EcrireSecurite(ctx, db, s, qui, "", t); err != nil {
			return true, err
		}
		fmt.Fprintln(sortie, "liste d'adresses de l'interface vidée (effet en 5 s au plus)")
		return true, nil
	}
	return false, nil
}
