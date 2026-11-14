// SPDX-License-Identifier: GPL-3.0-or-later

package certsyslog

import (
	"context"
	"crypto/sha1" // #nosec G505 -- vérifie la signature imposée par l'API OVH
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"mirador-stack/internal/server/secrets"
)

// zoneFausse : enregistrements TXT d'une fausse zone (sous-nom → valeurs).
type zoneFausse struct {
	mu  sync.Mutex
	txt map[string][]string
}

func (z *zoneFausse) valeurs(sous string) []string {
	z.mu.Lock()
	defer z.mu.Unlock()
	return append([]string(nil), z.txt[sous]...)
}

func (z *zoneFausse) mettre(sous string, v []string) {
	z.mu.Lock()
	defer z.mu.Unlock()
	if len(v) == 0 {
		delete(z.txt, sous)
	} else {
		z.txt[sous] = v
	}
}

// fauxFournisseur : faux serveur d'API pour chaque fournisseur (le strict
// nécessaire, avec contrôle des identifiants).
func fauxFournisseur(t *testing.T, nom string, z *zoneFausse) *httptest.Server {
	t.Helper()
	var mu sync.Mutex
	prochain := 100
	ids := map[string]string{} // identifiant → sous-nom|valeur
	repondre := func(w http.ResponseWriter, v any) { _ = json.NewEncoder(w).Encode(v) }
	ajouter := func(sous, val string) string {
		mu.Lock()
		defer mu.Unlock()
		prochain++
		id := strconv.Itoa(prochain)
		ids[id] = sous + "|" + val
		z.mettre(sous, append(z.valeurs(sous), val))
		return id
	}
	retirer := func(id string) bool {
		mu.Lock()
		defer mu.Unlock()
		sv, ok := ids[id]
		if !ok {
			return false
		}
		delete(ids, id)
		sous, val, _ := strings.Cut(sv, "|")
		var garde []string
		for _, v := range z.valeurs(sous) {
			if v != val {
				garde = append(garde, v)
			}
		}
		z.mettre(sous, garde)
		return true
	}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		corps, _ := io.ReadAll(r.Body)
		var c map[string]any
		_ = json.Unmarshal(corps, &c)
		texte := func(k string) string { s, _ := c[k].(string); return s }
		switch nom {
		case "cloudflare":
			if r.Header.Get("Authorization") != "Bearer jeton-cf" {
				w.WriteHeader(403)
				return
			}
			switch {
			case r.Method == "GET" && r.URL.Path == "/zones" && r.URL.Query().Get("name") == "exemple.org":
				repondre(w, map[string]any{"result": []map[string]string{{"id": "z1", "name": "exemple.org"}}})
			case r.Method == "POST" && r.URL.Path == "/zones/z1/dns_records" && texte("type") == "TXT":
				id := ajouter(strings.TrimSuffix(texte("name"), ".exemple.org"), strings.Trim(texte("content"), `"`))
				repondre(w, map[string]any{"result": map[string]string{"id": id}})
			case r.Method == "GET" && r.URL.Path == "/zones/z1/dns_records":
				sous := strings.TrimSuffix(r.URL.Query().Get("name"), ".exemple.org")
				var res []map[string]string
				mu.Lock()
				for id, sv := range ids {
					if s, v, _ := strings.Cut(sv, "|"); s == sous {
						res = append(res, map[string]string{"id": id, "content": `"` + v + `"`})
					}
				}
				mu.Unlock()
				repondre(w, map[string]any{"result": res})
			case r.Method == "DELETE" && strings.HasPrefix(r.URL.Path, "/zones/z1/dns_records/"):
				if !retirer(strings.TrimPrefix(r.URL.Path, "/zones/z1/dns_records/")) {
					w.WriteHeader(404)
				}
			default:
				w.WriteHeader(404)
			}
		case "ovh":
			if r.URL.Path == "/auth/time" {
				repondre(w, 1700000000)
				return
			}
			// Signature recalculée comme le fait OVH.
			adresse := "https://" + r.Host + r.URL.RequestURI()
			h := sha1.Sum([]byte("secret-app+cle-conso+" + r.Method + "+" + adresse + "+" + string(corps) + "+1700000000")) // #nosec G401 -- API OVH
			if r.Header.Get("X-Ovh-Application") != "cle-app" || r.Header.Get("X-Ovh-Consumer") != "cle-conso" ||
				r.Header.Get("X-Ovh-Signature") != "$1$"+hex.EncodeToString(h[:]) {
				w.WriteHeader(403)
				return
			}
			switch {
			case r.Method == "POST" && r.URL.Path == "/domain/zone/exemple.org/record" && texte("fieldType") == "TXT":
				id, _ := strconv.Atoi(ajouter(texte("subDomain"), texte("target")))
				repondre(w, map[string]any{"id": id})
			case r.Method == "POST" && r.URL.Path == "/domain/zone/exemple.org/refresh":
			case r.Method == "DELETE" && strings.HasPrefix(r.URL.Path, "/domain/zone/exemple.org/record/"):
				if !retirer(strings.TrimPrefix(r.URL.Path, "/domain/zone/exemple.org/record/")) {
					w.WriteHeader(404)
				}
			default:
				w.WriteHeader(404)
			}
		case "gandi":
			if r.Header.Get("Authorization") != "Bearer jeton-gandi" {
				w.WriteHeader(403)
				return
			}
			p := strings.TrimPrefix(r.URL.Path, "/domains/exemple.org/records/")
			if p == r.URL.Path || !strings.HasSuffix(p, "/TXT") {
				w.WriteHeader(404)
				return
			}
			sous := strings.TrimSuffix(p, "/TXT")
			switch r.Method {
			case "PUT":
				var v []string
				for _, x := range c["rrset_values"].([]any) {
					v = append(v, strings.Trim(x.(string), `"`))
				}
				z.mettre(sous, v)
				w.WriteHeader(201)
			case "DELETE":
				z.mettre(sous, nil)
				w.WriteHeader(204)
			}
		case "infomaniak":
			if r.Header.Get("Authorization") != "Bearer jeton-ik" {
				w.WriteHeader(401)
				return
			}
			switch {
			case r.Method == "GET" && r.URL.Path == "/1/product" && r.URL.Query().Get("customer_name") == "exemple.org":
				repondre(w, map[string]any{"result": "success", "data": []map[string]any{{"id": 42, "customer_name": "exemple.org"}}})
			case r.Method == "POST" && r.URL.Path == "/1/domain/42/dns/record" && texte("type") == "TXT":
				repondre(w, map[string]any{"result": "success", "data": ajouter(texte("source"), texte("target"))})
			case r.Method == "DELETE" && strings.HasPrefix(r.URL.Path, "/1/domain/42/dns/record/"):
				if !retirer(strings.TrimPrefix(r.URL.Path, "/1/domain/42/dns/record/")) {
					w.WriteHeader(404)
				}
			default:
				w.WriteHeader(404)
			}
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// Chaque fournisseur pose puis retire le TXT du défi ; mauvais
// identifiants refusés.
func TestFournisseursDNS(t *testing.T) {
	ctx := context.Background()
	for _, cas := range []struct {
		nom, base string
		ids       map[string]string
	}{
		{"cloudflare", "cloudflare", map[string]string{"jeton": "jeton-cf"}},
		{"ovh", "ovh-eu", map[string]string{"point": "ovh-eu", "cle_application": "cle-app", "secret_application": "secret-app", "cle_consommateur": "cle-conso"}},
		{"gandi", "gandi", map[string]string{"jeton": "jeton-gandi"}},
		{"infomaniak", "infomaniak", map[string]string{"jeton": "jeton-ik"}},
	} {
		z := &zoneFausse{txt: map[string][]string{}}
		srv := fauxFournisseur(t, cas.nom, z)
		bases := map[string]string{cas.base: srv.URL}
		f, err := nouveauFournisseur(cas.nom, cas.ids, "exemple.org", bases, srv.Client())
		if err != nil {
			t.Fatalf("%s : %v", cas.nom, err)
		}
		sous := "_acme-challenge.syslog.lan"
		if err := f.Poser(ctx, sous, "valeur-du-defi"); err != nil {
			t.Fatalf("%s : pose : %v", cas.nom, err)
		}
		if v := z.valeurs(sous); len(v) != 1 || v[0] != "valeur-du-defi" {
			t.Fatalf("%s : TXT posé = %v", cas.nom, v)
		}
		if err := f.Retirer(ctx, sous, "valeur-du-defi"); err != nil {
			t.Fatalf("%s : retrait : %v", cas.nom, err)
		}
		if v := z.valeurs(sous); len(v) != 0 {
			t.Errorf("%s : TXT pas retiré : %v", cas.nom, v)
		}
		// Mauvais secret : refusé, avec le nom du fournisseur.
		faux := map[string]string{}
		for k, v := range cas.ids {
			faux[k] = v
		}
		for k := range faux {
			if k == "jeton" || k == "cle_consommateur" {
				faux[k] = "mauvais"
			}
		}
		f, _ = nouveauFournisseur(cas.nom, faux, "exemple.org", bases, srv.Client())
		if err := f.Poser(ctx, sous, "x"); err == nil {
			t.Errorf("%s : mauvais identifiants acceptés", cas.nom)
		}
	}
	if _, err := nouveauFournisseur("inconnu", nil, "exemple.org", nil, nil); err == nil {
		t.Error("fournisseur inconnu accepté")
	}
	if _, err := nouveauFournisseur("ovh", map[string]string{"point": "ailleurs"}, "exemple.org", nil, nil); err == nil {
		t.Error("point d'accès OVH inconnu accepté")
	}
}

// Réglages : champs obligatoires, secrets gardés si le fournisseur ne
// change pas, effacés sinon ; ancien jeton deSEC toujours lu.
func TestReglagesFournisseur(t *testing.T) {
	ctx := context.Background()
	db := baseTest(t)
	ovh := func(ids map[string]string) error {
		return DefinirReglages(ctx, db, cleServeur, Saisie{Fournisseur: "ovh", Domaine: "exemple.org", Identifiants: ids}, "admin", time.Now())
	}
	complet := map[string]string{"point": "ovh-eu", "cle_application": "cle-app", "secret_application": "secret-app", "cle_consommateur": "cle-conso"}
	if err := ovh(map[string]string{"point": "ovh-eu", "cle_application": "cle-app"}); err == nil {
		t.Error("OVH sans secret accepté")
	}
	if err := ovh(map[string]string{"point": "ovh-mars", "cle_application": "a", "secret_application": "b", "cle_consommateur": "c"}); err == nil {
		t.Error("point d'accès inconnu accepté")
	}
	if err := ovh(map[string]string{"point": "ovh-eu", "cle_application": "a b", "secret_application": "b", "cle_consommateur": "c"}); err == nil {
		t.Error("espace dans un identifiant accepté")
	}
	if err := DefinirReglages(ctx, db, cleServeur, Saisie{Fournisseur: "route53", Domaine: "exemple.org"}, "admin", time.Now()); err == nil {
		t.Error("fournisseur inconnu accepté")
	}
	if err := ovh(complet); err != nil {
		t.Fatal(err)
	}
	r, _ := LireReglages(ctx, db)
	if r.Parametres["cle_application"] != "cle-app" || strings.Contains(r.IdentifiantsChiffre, "secret-app") {
		t.Fatalf("rangement : %+v", r)
	}
	// Secrets vides : gardés (même fournisseur).
	if err := ovh(map[string]string{"point": "ovh-ca", "cle_application": "cle-app"}); err != nil {
		t.Fatal(err)
	}
	r, _ = LireReglages(ctx, db)
	ids, err := r.identifiants(cleServeur)
	if err != nil || ids["secret_application"] != "secret-app" || ids["point"] != "ovh-ca" {
		t.Fatalf("secrets gardés : %v %v", ids, err)
	}
	// Autre fournisseur sans jeton : refusé (les secrets OVH ne servent pas).
	if err := DefinirReglages(ctx, db, cleServeur, Saisie{Fournisseur: "gandi", Domaine: "exemple.org"}, "admin", time.Now()); err == nil {
		t.Error("changement de fournisseur sans jeton accepté")
	}
	var audit string
	_ = db.QueryRow(`SELECT group_concat(apres) FROM audit WHERE action = 'certsyslog_reglages'`).Scan(&audit)
	if strings.Contains(audit, "secret-app") || strings.Contains(audit, "cle-conso") {
		t.Fatal("secret dans l'audit")
	}
	// Réglages d'avant (jeton deSEC seul) : toujours lus.
	ancien, _ := secrets.Chiffrer(cleServeur, UsageJeton, "ancien-jeton")
	vieux := Reglages{Domaine: "exemple.org", JetonChiffre: ancien}
	if vieux.FournisseurEffectif() != "desec" || !vieux.IdentifiantsEnregistres() {
		t.Fatal("ancien réglage mal lu")
	}
	if ids, err := vieux.identifiants(cleServeur); err != nil || ids["jeton"] != "ancien-jeton" {
		t.Fatalf("ancien jeton : %v %v", ids, err)
	}
}
