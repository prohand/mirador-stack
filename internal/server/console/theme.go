// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"net/http"
	"net/url"
	"strings"

	"mirador-stack/internal/server/auth"
)

// Thème par utilisateur (SPEC § 25) : sélecteur Jour / Nuit / Auto en bas
// du menu. Le navigateur change le thème tout de suite (mirador.js) et
// l'enregistre ici ; sans JavaScript, le formulaire recharge la page.

func (c *Console) routesTheme() {
	c.mux.HandleFunc("POST /compte/theme", c.lecture(c.postTheme))
	c.mux.HandleFunc("POST /compte/aides", c.lecture(c.postAides))
}

// postAides : bouton « Avec aides / Sans aides » du menu (SPEC § 26).
func (c *Console) postAides(q *req) {
	v := q.r.PostFormValue("aides")
	if v != "avec" && v != "sans" {
		http.Error(q.w, "choix inconnu", http.StatusBadRequest)
		return
	}
	if err := auth.DefinirAides(q.r.Context(), c.db, q.u.ID, v == "sans"); err != nil {
		http.Error(q.w, err.Error(), http.StatusInternalServerError)
		return
	}
	c.oublierComptes()
	if q.r.Header.Get("X-Requested-With") == "mirador" {
		q.w.WriteHeader(http.StatusNoContent)
		return
	}
	http.Redirect(q.w, q.r, retourLocal(q.r.PostFormValue("retour")), http.StatusSeeOther)
}

func (c *Console) postTheme(q *req) {
	theme := q.r.PostFormValue("theme")
	if err := auth.DefinirTheme(q.r.Context(), c.db, q.u.ID, theme); err != nil {
		http.Error(q.w, err.Error(), http.StatusBadRequest)
		return
	}
	c.oublierComptes()
	if q.r.Header.Get("X-Requested-With") == "mirador" {
		q.w.WriteHeader(http.StatusNoContent)
		return
	}
	http.Redirect(q.w, q.r, retourLocal(q.r.PostFormValue("retour")), http.StatusSeeOther)
}

// retourLocal : chemin de retour sur ce site seulement (« / » sinon).
func retourLocal(v string) string {
	u, err := url.Parse(v)
	if err != nil || v == "" || u.IsAbs() || u.Host != "" || !strings.HasPrefix(u.Path, "/") || strings.HasPrefix(v, "//") || strings.ContainsAny(v, "\\\r\n") {
		return "/"
	}
	return u.RequestURI()
}
