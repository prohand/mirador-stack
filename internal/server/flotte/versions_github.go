// SPDX-License-Identifier: GPL-3.0-or-later

package flotte

import (
	"context"
	"encoding/xml"
	"errors"
	"net/url"
	"strings"
	"time"
)

// Liste des dernières versions publiées sur GitHub (SPEC § 37) : proposée
// dans « Importer une version signée » au lieu de taper le numéro. Lue dans
// le flux Atom des Releases (https://github.com/DEPOT/releases.atom) : même
// hôte que les téléchargements, pas de clé d'API, pas de limite stricte.
// Ce n'est qu'une liste de noms : la signature est toujours vérifiée à
// l'import, quel que soit ce que dit le flux.

// VersionGitHub : une Release du dépôt.
type VersionGitHub struct {
	Tag  string    // « v0.3.2 »
	Date time.Time // dernière mise à jour de la Release
}

// tailleMaxFlux : le flux fait quelques dizaines de Ko ; au-delà, refusé.
const tailleMaxFlux = 1 << 20

// delaiFlux : la page des mises à jour n'attend pas GitHub plus longtemps.
const delaiFlux = 4 * time.Second

// fluxAtom : seuls les champs utiles du flux.
type fluxAtom struct {
	Entrees []struct {
		Titre string `xml:"title"`
		MAJ   string `xml:"updated"`
		Liens []struct {
			Href string `xml:"href,attr"`
		} `xml:"link"`
	} `xml:"entry"`
}

// DernieresVersionsGitHub : les n dernières versions « vX.Y.Z » du dépôt,
// de la plus récente à la plus ancienne (ordre du flux).
func DernieresVersionsGitHub(ctx context.Context, src SourceGitHub, depot string, n int) ([]VersionGitHub, error) {
	if !formatDepot.MatchString(depot) {
		return nil, errors.New("dépôt invalide")
	}
	base := src.Base
	if base == "" {
		base = "https://github.com"
	}
	client := src.Client
	if client == nil {
		client = clientGitHub()
	}
	ctx, fin := context.WithTimeout(ctx, delaiFlux)
	defer fin()
	b, err := telechargerGitHub(ctx, client, base+"/"+depot+"/releases.atom", tailleMaxFlux)
	if err != nil {
		return nil, err
	}
	var f fluxAtom
	if err := xml.Unmarshal(b, &f); err != nil {
		return nil, errors.New("flux des versions illisible")
	}
	var out []VersionGitHub
	vu := map[string]bool{}
	for _, e := range f.Entrees {
		if len(out) >= n {
			break
		}
		tag := tagDeEntree(e.Liens, e.Titre)
		if tag == "" || vu[tag] {
			continue
		}
		vu[tag] = true
		v := VersionGitHub{Tag: tag}
		v.Date, _ = time.Parse(time.RFC3339, strings.TrimSpace(e.MAJ))
		out = append(out, v)
	}
	return out, nil
}

// tagDeEntree : le tag est la fin du lien « …/releases/tag/v0.3.2 » ; à
// défaut, le titre s'il a la forme d'une version. Tout autre nom est ignoré.
func tagDeEntree(liens []struct {
	Href string `xml:"href,attr"`
}, titre string) string {
	for _, l := range liens {
		u, err := url.Parse(l.Href)
		if err != nil {
			continue
		}
		if i := strings.LastIndex(u.Path, "/tag/"); i >= 0 {
			if t, err := url.PathUnescape(u.Path[i+len("/tag/"):]); err == nil && formatTag.MatchString(t) {
				return normaliserTag(t)
			}
		}
	}
	if t := strings.TrimSpace(titre); formatTag.MatchString(t) {
		return normaliserTag(t)
	}
	return ""
}

// normaliserTag : toujours avec le « v » devant.
func normaliserTag(t string) string {
	if !strings.HasPrefix(t, "v") {
		return "v" + t
	}
	return t
}
