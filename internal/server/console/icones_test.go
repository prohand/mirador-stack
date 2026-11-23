// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

// Retour du 23/11 : l'icône de l'application n'apparaissait pas sur le
// raccourci du téléphone. Manifeste, icônes et adresses demandées d'office
// (/apple-touch-icon.png, /favicon.ico) : servis sans être connecté.
func TestIconesSansConnexion(t *testing.T) {
	b := nouveauBanc(t)
	cl := b.client() // pas de session
	for chemin, typ := range map[string]string{
		"/statique/mirador.webmanifest":        "application/manifest+json",
		"/statique/icones/icone-192.png":       "image/png",
		"/statique/icones/icone-512.png":       "image/png",
		"/statique/icones/icone-apple-180.png": "image/png",
		"/apple-touch-icon.png":                "image/png",
		"/apple-touch-icon-precomposed.png":    "image/png",
		"/favicon.ico":                         "image/x-icon",
	} {
		resp, err := cl.Get(b.srv.URL + chemin)
		if err != nil {
			t.Fatal(err)
		}
		corps, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), typ) || len(corps) < 100 {
			t.Errorf("%s : %d %s (%d octets)", chemin, resp.StatusCode, resp.Header.Get("Content-Type"), len(corps))
		}
	}
	// La page de connexion annonce le manifeste et les icônes.
	b.premierAdmin("admin", mdpAdmin)
	_, corps, _ := b.get(b.client(), "/connexion")
	for _, attendu := range []string{`rel="manifest"`, `rel="apple-touch-icon" sizes="180x180"`, `rel="icon" type="image/png" sizes="192x192"`} {
		if !strings.Contains(corps, attendu) {
			t.Errorf("connexion : %q absent", attendu)
		}
	}
}
