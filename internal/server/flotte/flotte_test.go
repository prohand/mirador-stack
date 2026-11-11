// SPDX-License-Identifier: GPL-3.0-or-later

package flotte

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"mirador-stack/internal/agent/majagent"
	"mirador-stack/internal/protocole"
	"mirador-stack/internal/server/base"
	"mirador-stack/internal/server/ingestion"
	"mirador-stack/internal/server/stockage"
	"mirador-stack/internal/server/web"
)

var (
	t0  = time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)
	ctx = context.Background()
)

type banc struct {
	db  *sql.DB
	f   *Flotte
	h   http.Handler
	dir string
}

func nouveauBanc(t *testing.T) *banc {
	t.Helper()
	dir := t.TempDir()
	db, err := base.Ouvrir(filepath.Join(dir, "mirador.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := base.Migrer(ctx, db, dir); err != nil {
		t.Fatal(err)
	}
	mem := &stockage.Memoire{}
	maintenant := func() time.Time { return t0 }
	ing := ingestion.Nouvelle(ingestion.Options{DB: db, Metriques: mem, Logs: stockage.LogsMemoire{Memoire: mem}, Maintenant: maintenant})
	f := Nouvelle(Options{DB: db, Ingestion: ing, DossierVersions: filepath.Join(dir, "versions"), Maintenant: maintenant})
	m := http.NewServeMux()
	ing.Routes(m)
	f.Routes(m)
	return &banc{db: db, f: f, h: web.IPClient(nil)(m), dir: dir}
}

func (b *banc) requete(methode, chemin, token string, corps []byte, ip string, entetes ...string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(methode, chemin, bytes.NewReader(corps))
	if ip == "" {
		ip = "192.0.2.10"
	}
	r.RemoteAddr = net.JoinHostPort(ip, "5000")
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	for i := 0; i+1 < len(entetes); i += 2 {
		r.Header.Set(entetes[i], entetes[i+1])
	}
	w := httptest.NewRecorder()
	b.h.ServeHTTP(w, r)
	return w
}

func (b *banc) enroler(t *testing.T, jeton, identifiant, nom, ip string) protocole.ReponseEnrolement {
	t.Helper()
	corps, _ := json.Marshal(protocole.DemandeEnrolement{Jeton: jeton, NomPropose: nom, Version: "1.0.0",
		Architecture: "amd64", Identifiant: identifiant})
	w := b.requete(http.MethodPost, protocole.CheminEnrolement, "", corps, ip)
	if w.Code != http.StatusOK {
		t.Fatalf("enrôlement : HTTP %d %s", w.Code, w.Body)
	}
	var rep protocole.ReponseEnrolement
	if err := json.Unmarshal(w.Body.Bytes(), &rep); err != nil {
		t.Fatal(err)
	}
	return rep
}

const (
	idA = "0123456789abcdef0123456789abcdef"
	idB = "fedcba9876543210fedcba9876543210"
)

func TestEnrolementComplet(t *testing.T) {
	b := nouveauBanc(t)
	jeton, err := CreerJeton(ctx, b.db, time.Hour, "admin", t0)
	if err != nil {
		t.Fatal(err)
	}
	if rep := b.enroler(t, jeton, idA, "pc-bureau", "198.51.100.4"); rep.Statut != protocole.EnAttente || rep.Attente != 30 {
		t.Fatalf("1re demande : %+v", rep)
	}
	if rep := b.enroler(t, jeton, idA, "pc-bureau", "198.51.100.4"); rep.Statut != protocole.EnAttente {
		t.Fatalf("en attente : %+v", rep)
	}
	// Une autre installation qui aurait volé le jeton : refusée.
	if rep := b.enroler(t, jeton, idB, "pirate", "203.0.113.66"); rep.Statut != protocole.Refuse {
		t.Fatalf("autre identifiant : %+v", rep)
	}
	if err := Approuver(ctx, b.db, "pc-bureau", true, "admin", t0); err != nil {
		t.Fatal(err)
	}
	rep := b.enroler(t, jeton, idA, "pc-bureau", "198.51.100.4")
	if rep.Statut != protocole.Approuve || !strings.HasPrefix(rep.Token, PrefixeToken) || rep.Nom != "pc-bureau" {
		t.Fatalf("approuvée : %+v", rep)
	}
	// Le token marche, depuis l'adresse de la demande seulement.
	if w := b.requete(http.MethodGet, protocole.CheminConfig, rep.Token, nil, "198.51.100.4"); w.Code != http.StatusOK {
		t.Fatalf("config avec le token : %d", w.Code)
	}
	if w := b.requete(http.MethodGet, protocole.CheminConfig, rep.Token, nil, "198.51.100.5"); w.Code != http.StatusForbidden {
		t.Fatalf("autre adresse : %d", w.Code)
	}
	// Réponse perdue : nouvelle demande → nouveau token, l'ancien ne marche plus.
	rep2 := b.enroler(t, jeton, idA, "pc-bureau", "198.51.100.4")
	if rep2.Token == rep.Token {
		t.Fatal("même token remis deux fois")
	}
	if w := b.requete(http.MethodGet, protocole.CheminConfig, rep.Token, nil, "198.51.100.4"); w.Code != http.StatusUnauthorized {
		t.Fatalf("ancien token : %d", w.Code)
	}
	var actifs int
	_ = b.db.QueryRow(`SELECT count(*) FROM tokens WHERE revoque_le = 0`).Scan(&actifs)
	if actifs != 1 {
		t.Errorf("%d tokens actifs", actifs)
	}
	// Jamais de secret dans l'audit.
	var n int
	_ = b.db.QueryRow(`SELECT count(*) FROM audit WHERE apres LIKE ? OR avant LIKE ?`, "%"+rep2.Token+"%", "%"+jeton+"%").Scan(&n)
	if n != 0 {
		t.Error("secret écrit dans l'audit")
	}
}

func TestEnrolementRefus(t *testing.T) {
	b := nouveauBanc(t)
	if rep := b.enroler(t, "mirador_enrol_inconnu-0123456789", idA, "pc", ""); rep.Statut != protocole.Refuse {
		t.Fatalf("jeton inconnu : %+v", rep)
	}
	expire, _ := CreerJeton(ctx, b.db, time.Minute, "admin", t0.Add(-time.Hour))
	if rep := b.enroler(t, expire, idA, "pc", ""); rep.Statut != protocole.Refuse {
		t.Fatalf("jeton expiré : %+v", rep)
	}
	// Nom déjà pris : refusé, mais le jeton reste utilisable avec un autre nom.
	if _, err := TokenManuel(ctx, b.db, "pris", "admin", t0); err != nil {
		t.Fatal(err)
	}
	jeton, _ := CreerJeton(ctx, b.db, time.Hour, "admin", t0)
	if rep := b.enroler(t, jeton, idA, "pris", ""); rep.Statut != protocole.Refuse || rep.Message != msgNomPris {
		t.Fatalf("nom pris : %+v", rep)
	}
	if rep := b.enroler(t, jeton, idA, "libre", ""); rep.Statut != protocole.EnAttente {
		t.Fatalf("autre nom : %+v", rep)
	}
	if err := Refuser(ctx, b.db, "libre", "admin", t0); err != nil {
		t.Fatal(err)
	}
	if rep := b.enroler(t, jeton, idA, "libre", ""); rep.Statut != protocole.Refuse || rep.Message != msgRefusee {
		t.Fatalf("refusée : %+v", rep)
	}
	// Demandes mal formées : 400.
	for _, corps := range []string{`{"jeton":"x"}`, `{"inconnu":1}`, `pas du json`,
		`{"jeton":"` + jeton + `","nom_propose":"a","version":"1","architecture":"mips","identifiant":"` + idA + `"}`} {
		if w := b.requete(http.MethodPost, protocole.CheminEnrolement, "", []byte(corps), "198.51.100.200"); w.Code != http.StatusBadRequest {
			t.Errorf("%s : %d", corps, w.Code)
		}
	}
}

func TestEnrolementForceBrute(t *testing.T) {
	b := nouveauBanc(t)
	for i := 0; i < 10; i++ {
		b.enroler(t, "mirador_enrol_faux-0123456789-"+string(rune('a'+i)), idA, "pc", "203.0.113.9")
	}
	corps, _ := json.Marshal(protocole.DemandeEnrolement{Jeton: "mirador_enrol_faux-0123456789-z", NomPropose: "pc",
		Version: "1.0.0", Architecture: "amd64", Identifiant: idA})
	if w := b.requete(http.MethodPost, protocole.CheminEnrolement, "", corps, "203.0.113.9"); w.Code != http.StatusTooManyRequests {
		t.Fatalf("après 10 jetons faux : %d", w.Code)
	}
}

func TestConfigDistante(t *testing.T) {
	b := nouveauBanc(t)
	token, _ := TokenManuel(ctx, b.db, "pc", "admin", t0)
	if err := DefinirEtiquettes(ctx, b.db, "pc", "maison", "prod", "serveurs", "admin", t0); err != nil {
		t.Fatal(err)
	}
	id, _ := IDMachine(ctx, b.db, "pc")
	for portee, doc := range map[string]string{
		"defaut":              `{"intervalle":"60s","journal":true}`,
		"groupe:serveurs":     `{"intervalle":"30s"}`,
		"machine:" + itoa(id): `{"logs_debit":50}`,
	} {
		if err := DefinirConfig(ctx, b.db, portee, []byte(doc), "admin", t0); err != nil {
			t.Fatalf("%s : %v", portee, err)
		}
	}
	w := b.requete(http.MethodGet, protocole.CheminConfig, token, nil, "")
	if w.Code != http.StatusOK || w.Body.String() != `{"intervalle":"30s","journal":true,"logs_debit":50}` {
		t.Fatalf("config fusionnée : %d %s", w.Code, w.Body)
	}
	etag := w.Header().Get("ETag")
	if w := b.requete(http.MethodGet, protocole.CheminConfig, token, nil, "", "If-None-Match", etag); w.Code != http.StatusNotModified || w.Body.Len() != 0 {
		t.Fatalf("ETag : %d", w.Code)
	}
	// Documents refusés dès l'enregistrement.
	for _, doc := range []string{`{"commande":"rm -rf /"}`, `{"intervalle":"1s"}`, `{"url_metriques":"https://ailleurs"}`, `[1]`} {
		if err := DefinirConfig(ctx, b.db, "defaut", []byte(doc), "admin", t0); err == nil {
			t.Errorf("document accepté : %s", doc)
		}
	}
	if err := DefinirConfig(ctx, b.db, "partout", []byte(`{}`), "admin", t0); err == nil {
		t.Error("portée invalide acceptée")
	}
	// Retirer une portée.
	_ = DefinirConfig(ctx, b.db, "machine:"+itoa(id), nil, "admin", t0)
	w = b.requete(http.MethodGet, protocole.CheminConfig, token, nil, "", "If-None-Match", etag)
	if w.Code != http.StatusOK || strings.Contains(w.Body.String(), "logs_debit") {
		t.Fatalf("après retrait : %d %s", w.Code, w.Body)
	}
	// Sans token : refusé.
	if w := b.requete(http.MethodGet, protocole.CheminConfig, "", nil, "198.51.100.77"); w.Code != http.StatusUnauthorized {
		t.Fatalf("sans token : %d", w.Code)
	}
}

func itoa(i int64) string { return strconv.FormatInt(i, 10) }

func TestBattement(t *testing.T) {
	b := nouveauBanc(t)
	token, _ := TokenManuel(ctx, b.db, "pc", "admin", t0)
	corps, _ := json.Marshal(protocole.Battement{Version: "1.2.3", Architecture: "arm64", Demarrage: t0.Add(-time.Hour),
		MajEchec: "self-test du nouveau binaire en échec"})
	if w := b.requete(http.MethodPost, protocole.CheminBattement, token, corps, ""); w.Code != http.StatusNoContent {
		t.Fatalf("battement : %d %s", w.Code, w.Body)
	}
	if err := b.f.EcrireBattements(ctx); err != nil {
		t.Fatal(err)
	}
	var version, arch, echec string
	var le, demarrage int64
	_ = b.db.QueryRow(`SELECT version_agent, architecture, maj_echec, battement_le, demarrage_le FROM machines WHERE nom = 'pc'`).
		Scan(&version, &arch, &echec, &le, &demarrage)
	if version != "1.2.3" || arch != "arm64" || echec == "" || le != t0.Unix() || demarrage != t0.Add(-time.Hour).Unix() {
		t.Fatalf("en base : %s %s %q %d %d", version, arch, echec, le, demarrage)
	}
	for _, mauvais := range []string{`{"version":"1.0; rm -rf /"}`, `{"version":"1.0","architecture":"sparc"}`, `{"version":"1.0","pirate":1}`} {
		if w := b.requete(http.MethodPost, protocole.CheminBattement, token, []byte(mauvais), ""); w.Code != http.StatusBadRequest {
			t.Errorf("%s : %d", mauvais, w.Code)
		}
	}
}

// signer : ce que fera mirador-release, avec une clé de test.
func signer(t *testing.T, priv ed25519.PrivateKey, version, arch string, binaire []byte) *protocole.Manifeste {
	t.Helper()
	h := sha256.Sum256(binaire)
	m := &protocole.Manifeste{Version: version, Architecture: arch, SHA256: hex.EncodeToString(h[:]), Taille: int64(len(binaire))}
	m.Signature = base64.StdEncoding.EncodeToString(ed25519.Sign(priv, m.MessageSigne()))
	return m
}

func TestMisesAJour(t *testing.T) {
	b := nouveauBanc(t)
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	ancienne := majagent.ClesPubliques
	majagent.ClesPubliques = base64.StdEncoding.EncodeToString(pub)
	defer func() { majagent.ClesPubliques = ancienne }()

	token, _ := TokenManuel(ctx, b.db, "pc", "admin", t0)
	corps, _ := json.Marshal(protocole.Battement{Version: "1.0.0", Architecture: "amd64"})
	b.requete(http.MethodPost, protocole.CheminBattement, token, corps, "")
	_ = b.f.EcrireBattements(ctx)

	// Rien d'assigné : pas de manifeste.
	lireMaj := func(version string) *protocole.Manifeste {
		t.Helper()
		w := b.requete(http.MethodGet, protocole.CheminMaj, token, nil, "", protocole.EnteteVersion, version)
		if w.Code != http.StatusOK {
			t.Fatalf("maj : %d", w.Code)
		}
		var rep protocole.ReponseMaj
		_ = json.Unmarshal(w.Body.Bytes(), &rep)
		return rep.Manifeste
	}
	if lireMaj("1.0.0") != nil {
		t.Fatal("manifeste sans version assignée")
	}
	binaire := []byte("binaire de test de mirador-agent 1.1.0")
	m := signer(t, priv, "1.1.0", "amd64", binaire)
	// Signature d'une autre clé, ou binaire modifié : publication refusée.
	_, autre, _ := ed25519.GenerateKey(rand.Reader)
	if err := Publier(ctx, b.db, b.f.opt.DossierVersions, signer(t, autre, "1.1.0", "amd64", binaire), binaire, "admin", t0); err == nil {
		t.Fatal("signature d'une clé inconnue acceptée")
	}
	if err := Publier(ctx, b.db, b.f.opt.DossierVersions, m, append([]byte("x"), binaire...), "admin", t0); err == nil {
		t.Fatal("binaire modifié accepté")
	}
	if err := DefinirCible(ctx, b.db, 0, "1.1.0", "admin", t0); err == nil {
		t.Fatal("version non publiée assignée")
	}
	if err := Publier(ctx, b.db, b.f.opt.DossierVersions, m, binaire, "admin", t0); err != nil {
		t.Fatal(err)
	}
	if err := DefinirCible(ctx, b.db, 0, "1.1.0", "admin", t0); err != nil {
		t.Fatal(err)
	}
	// Une version pour l'autre architecture se publie aussi (le serveur n'a
	// pas à être de la même architecture que les agents).
	if err := Publier(ctx, b.db, b.f.opt.DossierVersions, signer(t, priv, "1.1.0", "arm64", binaire), binaire, "admin", t0); err != nil {
		t.Fatalf("publication arm64 : %v", err)
	}
	got := lireMaj("1.0.0")
	if got == nil || got.Version != "1.1.0" || got.Signature != m.Signature {
		t.Fatalf("manifeste : %+v", got)
	}
	// L'agent vérifie lui-même : la signature passe avec la clé intégrée.
	if err := majagent.VerifierManifeste(got, "1.0.0", majagent.ClesPubliques); err != nil {
		t.Fatal(err)
	}
	if lireMaj("1.1.0") != nil {
		t.Error("manifeste renvoyé à un agent déjà à jour")
	}
	// Binaire : seulement la version assignée, pour la bonne architecture.
	w := b.requete(http.MethodGet, protocole.CheminBinaire+"?version=1.1.0&arch=amd64", token, nil, "")
	if w.Code != http.StatusOK || !bytes.Equal(w.Body.Bytes(), binaire) {
		t.Fatalf("binaire : %d", w.Code)
	}
	for _, q := range []string{"?version=1.0.0&arch=amd64", "?version=1.1.0&arch=arm64", "?version=../../etc/passwd&arch=amd64"} {
		if w := b.requete(http.MethodGet, protocole.CheminBinaire+q, token, nil, ""); w.Code != http.StatusNotFound {
			t.Errorf("%s : %d", q, w.Code)
		}
	}
	// Assignation propre à une machine : prioritaire sur la globale.
	m2 := signer(t, priv, "1.2.0", "amd64", []byte("binaire 1.2.0"))
	if err := Publier(ctx, b.db, b.f.opt.DossierVersions, m2, []byte("binaire 1.2.0"), "admin", t0); err != nil {
		t.Fatal(err)
	}
	id, _ := IDMachine(ctx, b.db, "pc")
	if err := DefinirCible(ctx, b.db, id, "1.2.0", "admin", t0); err != nil {
		t.Fatal(err)
	}
	if got := lireMaj("1.0.0"); got == nil || got.Version != "1.2.0" {
		t.Fatalf("cible de la machine : %+v", got)
	}
}

// Jeton avec site, type et groupe : la machine créée les reçoit.
func TestJetonEtiquettes(t *testing.T) {
	b := nouveauBanc(t)
	if _, err := CreerJetonEtiquettes(ctx, b.db, time.Hour, Etiquettes{Site: "pas bon"}, "admin", t0); err == nil {
		t.Fatal("site invalide accepté")
	}
	jeton, err := CreerJetonEtiquettes(ctx, b.db, time.Hour, Etiquettes{Site: "maison", Env: "vm", Groupe: "lan"}, "admin", t0)
	if err != nil {
		t.Fatal(err)
	}
	b.enroler(t, jeton, idA, "pc-salon", "198.51.100.4")
	var site, env, groupe string
	if err := b.db.QueryRow(`SELECT site, env, groupe FROM machines WHERE nom = 'pc-salon'`).Scan(&site, &env, &groupe); err != nil ||
		site != "maison" || env != "vm" || groupe != "lan" {
		t.Fatalf("étiquettes : %q %q %q %v", site, env, groupe, err)
	}
}

// Nom choisi au jeton, renommage et anciens noms (SPEC § 18.23).
func TestNomJetonEtAnciensNoms(t *testing.T) {
	b := nouveauBanc(t)
	if _, err := CreerJetonEtiquettes(ctx, b.db, time.Hour, Etiquettes{Nom: "pas bon"}, "admin", t0); err == nil {
		t.Fatal("nom invalide accepté")
	}
	jeton, err := CreerJetonEtiquettes(ctx, b.db, time.Hour, Etiquettes{Nom: "dns02", Groupe: "dns"}, "admin", t0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := CreerJetonEtiquettes(ctx, b.db, time.Hour, Etiquettes{Nom: "dns02"}, "admin", t0); err == nil {
		t.Error("deux jetons valables pour le même nom")
	}
	// Le nom du jeton s'impose au nom d'hôte proposé.
	b.enroler(t, jeton, idA, "debian", "198.51.100.4")
	ms, _ := ListerMachines(ctx, b.db)
	if len(ms) != 1 || ms[0].Nom != "dns02" || !ms[0].NomDuJeton || ms[0].Groupe != "dns" {
		t.Fatalf("machine : %+v", ms)
	}
	// Renommée avant approbation : pas d'ancien nom (aucune donnée).
	if err := Renommer(ctx, b.db, "dns02", "srv-test", "admin", t0); err != nil {
		t.Fatal(err)
	}
	if a, _ := NomsAnciens(ctx, b.db); len(a) != 0 {
		t.Errorf("ancien nom gardé pour une machine en attente : %v", a)
	}
	if err := Approuver(ctx, b.db, "srv-test", false, "admin", t0); err != nil {
		t.Fatal(err)
	}
	// Renommée après approbation : l'ancien nom est gardé.
	if err := Renommer(ctx, b.db, "srv-test", "dns-b", "admin", t0); err != nil {
		t.Fatal(err)
	}
	a, _ := NomsAnciens(ctx, b.db)
	if a["srv-test"] != "dns-b" {
		t.Fatalf("anciens noms : %v", a)
	}
	// L'ancien nom est réservé : ni jeton, ni autre machine.
	if _, err := CreerJetonEtiquettes(ctx, b.db, time.Hour, Etiquettes{Nom: "srv-test"}, "admin", t0); err == nil {
		t.Error("jeton accepté sur l'ancien nom d'une machine")
	}
	// Retour à l'ancien nom : il n'est plus « ancien ».
	if err := Renommer(ctx, b.db, "dns-b", "srv-test", "admin", t0); err != nil {
		t.Fatal(err)
	}
	a, _ = NomsAnciens(ctx, b.db)
	if _, ok := a["srv-test"]; ok || a["dns-b"] != "srv-test" {
		t.Errorf("anciens noms après retour : %v", a)
	}
	id, _ := IDMachine(ctx, b.db, "srv-test")
	if l, _ := AnciensNoms(ctx, b.db, id); len(l) != 1 || l[0] != "dns-b" {
		t.Errorf("anciens noms de la machine : %v", l)
	}
}

// Site et type proposés par la machine : gardés si le jeton n'en a pas ;
// ceux du jeton ont la priorité ; jamais de groupe.
func TestSiteTypeProposes(t *testing.T) {
	b := nouveauBanc(t)
	envoyer := func(jeton, id, nom string) {
		t.Helper()
		corps, _ := json.Marshal(protocole.DemandeEnrolement{Jeton: jeton, NomPropose: nom, Version: "1.0.0",
			Architecture: "amd64", Identifiant: id, SitePropose: "bureau", EnvPropose: "vm"})
		if w := b.requete(http.MethodPost, protocole.CheminEnrolement, "", corps, "198.51.100.4"); w.Code != http.StatusOK {
			t.Fatalf("HTTP %d", w.Code)
		}
	}
	j1, _ := CreerJetonEtiquettes(ctx, b.db, time.Hour, Etiquettes{}, "admin", t0)
	envoyer(j1, idA, "pc1")
	j2, _ := CreerJetonEtiquettes(ctx, b.db, time.Hour, Etiquettes{Site: "maison"}, "admin", t0)
	envoyer(j2, idB, "pc2")
	for nom, att := range map[string][2]string{"pc1": {"bureau", "vm"}, "pc2": {"maison", "vm"}} {
		var site, env, groupe string
		_ = b.db.QueryRow(`SELECT site, env, groupe FROM machines WHERE nom = ?`, nom).Scan(&site, &env, &groupe)
		if site != att[0] || env != att[1] || groupe != "" {
			t.Errorf("%s : %q %q %q", nom, site, env, groupe)
		}
	}
}

// Clé publique réglée dans l'interface : servie au script d'installation et
// utilisée pour vérifier les versions publiées (SPEC § 18.25).
func TestClesPubliques(t *testing.T) {
	b := nouveauBanc(t)
	ancienne := majagent.ClesPubliques
	majagent.ClesPubliques = ""
	defer func() { majagent.ClesPubliques = ancienne }()

	// Aucune clé : 404, et publication refusée.
	if w := b.requete(http.MethodGet, protocole.CheminCles, "", nil, ""); w.Code != http.StatusNotFound {
		t.Fatalf("sans clé : %d", w.Code)
	}
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	binaire := []byte("binaire de test")
	m := signer(t, priv, "1.1.0", "amd64", binaire)
	if err := Publier(ctx, b.db, t.TempDir(), m, binaire, "admin", t0); err == nil || !strings.Contains(err.Error(), "aucune clé") {
		t.Fatalf("publication sans clé : %v", err)
	}
	// Clé invalide refusée ; clé valide enregistrée (avec audit).
	if err := DefinirClesPubliques(ctx, b.db, "pas une clé", "admin", t0); err == nil {
		t.Error("clé invalide acceptée")
	}
	cle := base64.StdEncoding.EncodeToString(pub)
	if err := DefinirClesPubliques(ctx, b.db, cle+"\n", "admin", t0); err != nil {
		t.Fatal(err)
	}
	var n int
	_ = b.db.QueryRow(`SELECT COUNT(*) FROM audit WHERE action = 'cles_publiques'`).Scan(&n)
	if n != 1 {
		t.Errorf("audit : %d ligne(s)", n)
	}
	// Servie en clair, une par ligne.
	w := b.requete(http.MethodGet, protocole.CheminCles, "", nil, "")
	if w.Code != http.StatusOK || strings.TrimSpace(w.Body.String()) != cle {
		t.Fatalf("clé servie : %d %q", w.Code, w.Body.String())
	}
	// Publication acceptée avec cette clé, refusée avec une autre.
	if err := Publier(ctx, b.db, t.TempDir(), m, binaire, "admin", t0); err != nil {
		t.Fatalf("publication : %v", err)
	}
	_, autre, _ := ed25519.GenerateKey(rand.Reader)
	if err := Publier(ctx, b.db, t.TempDir(), signer(t, autre, "1.2.0", "amd64", binaire), binaire, "admin", t0); err == nil {
		t.Error("version signée par une autre clé acceptée")
	}
}
