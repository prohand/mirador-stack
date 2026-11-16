// SPDX-License-Identifier: GPL-3.0-or-later

package flotte

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"strings"
	"testing"
	"time"

	"mirador-stack/internal/agent/majagent"
	"mirador-stack/internal/protocole"
	"mirador-stack/internal/server/certsyslog"
)

// Collecteur (SPEC § 23.13) : type choisi au jeton, équipements rattachés,
// liste posée dans sa configuration, clé « collecteur » réservée au binaire
// qui annonce la capacité.
func TestCollecteur(t *testing.T) {
	b := nouveauBanc(t)
	jeton, err := CreerJetonEtiquettes(ctx, b.db, time.Hour, Etiquettes{Nom: "collecteur-lan", TypeMachine: "collecteur"}, "admin", t0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := CreerJetonEtiquettes(ctx, b.db, time.Hour, Etiquettes{TypeMachine: "routeur"}, "admin", t0); err == nil {
		t.Error("type inconnu accepté")
	}
	b.enroler(t, jeton, idA, "nom-propose", "198.51.100.4")
	idC, err := IDMachine(ctx, b.db, "collecteur-lan")
	if err != nil {
		t.Fatal(err)
	}
	if _, typ, _ := typeDe(ctx, b.db, idC); typ != "collecteur" {
		t.Fatalf("type : %q", typ)
	}
	if err := Approuver(ctx, b.db, "collecteur-lan", false, "admin", t0); err != nil {
		t.Fatal(err)
	}
	// Équipement relevé (sans token) et machine avec agent.
	if err := CreerEquipement(ctx, b.db, "nas", idC, "admin", t0); err != nil {
		t.Fatal(err)
	}
	if err := CreerEquipement(ctx, b.db, "nas", idC, "admin", t0); err == nil {
		t.Error("équipement en double créé")
	}
	tokPC, _ := TokenManuel(ctx, b.db, "pc", "admin", t0)
	idPC, _ := IDMachine(ctx, b.db, "pc")
	idNas, _ := IDMachine(ctx, b.db, "nas")
	if err := RattacherEquipement(ctx, b.db, idPC, idNas, "admin", t0); err == nil {
		t.Error("rattaché à une machine qui n'est pas un collecteur")
	}
	if err := RattacherEquipement(ctx, b.db, idC, idC, "admin", t0); err == nil {
		t.Error("collecteur rattaché à lui-même")
	}
	if err := RattacherEquipement(ctx, b.db, idC, idPC, "admin", t0); err != nil {
		t.Fatal(err)
	}
	eqs, _ := EquipementsDe(ctx, b.db, idC)
	if len(eqs) != 2 || eqs[0].Nom != "nas" || eqs[1].Nom != "pc" {
		t.Fatalf("équipements : %+v", eqs)
	}
	if _, nom, _ := CollecteurDe(ctx, b.db, idNas); nom != "collecteur-lan" {
		t.Errorf("CollecteurDe : %q", nom)
	}
	// Configuration du collecteur : liste posée par le serveur, même si une
	// liste a été saisie à la main.
	if err := DefinirConfig(ctx, b.db, "machine:"+itoa(idC), []byte(`{"collecteur":{"equipements":["intrus"]},"intervalle":"60s"}`), "admin", t0); err != nil {
		t.Fatal(err)
	}
	doc, _, err := ConfigMachine(ctx, b.db, idC)
	if err != nil || !strings.Contains(string(doc), `"collecteur":{"equipements":["nas","pc"]}`) {
		t.Fatalf("configuration : %s %v", doc, err)
	}
	if d, err := ConfigEffective(ctx, b.db, idC); err != nil || d.Collecteur == nil || len(d.Collecteur.Equipements) != 2 {
		t.Fatalf("configuration lue par le collecteur : %+v %v", d, err)
	}
	// Détaché : retiré de la liste.
	if err := DetacherEquipement(ctx, b.db, idPC, "admin", t0); err != nil {
		t.Fatal(err)
	}
	if err := DetacherEquipement(ctx, b.db, idPC, "admin", t0); err == nil {
		t.Error("détaché deux fois")
	}
	if doc, _, _ = ConfigMachine(ctx, b.db, idC); !strings.Contains(string(doc), `"equipements":["nas"]`) {
		t.Errorf("après détachement : %s", doc)
	}
	// Une machine ordinaire ne reçoit jamais la clé « collecteur » ; un
	// agent ne la recevrait pas non plus sans la capacité.
	if err := DefinirConfig(ctx, b.db, "defaut", []byte(`{"collecteur":{}}`), "admin", t0); err != nil {
		t.Fatal(err)
	}
	w := b.requete(http.MethodGet, protocole.CheminConfig, tokPC, nil, "", protocole.EnteteCapacites, protocole.Capacites)
	if strings.Contains(w.Body.String(), "collecteur") {
		t.Errorf("clé collecteur envoyée à un agent : %s", w.Body)
	}
}

// Binaires par produit : un collecteur reçoit le manifeste et le binaire de
// mirador-collector, un agent ceux de mirador-agent.
func TestMisesAJourCollecteur(t *testing.T) {
	b := nouveauBanc(t)
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	ancienne := majagent.ClesPubliques
	majagent.ClesPubliques = base64.StdEncoding.EncodeToString(pub)
	defer func() { majagent.ClesPubliques = ancienne }()
	signerProduit := func(produit string, binaire []byte) *protocole.Manifeste {
		h := sha256.Sum256(binaire)
		m := &protocole.Manifeste{Produit: produit, Version: "1.1.0", Architecture: "amd64", SHA256: hex.EncodeToString(h[:]), Taille: int64(len(binaire))}
		m.Signature = base64.StdEncoding.EncodeToString(ed25519.Sign(priv, m.MessageSigne()))
		return m
	}
	binA, binC := []byte("binaire de mirador-agent 1.1.0"), []byte("binaire de mirador-collector 1.1.0")
	if err := Publier(ctx, b.db, b.f.opt.DossierVersions, signerProduit("", binA), binA, "admin", t0); err != nil {
		t.Fatal(err)
	}
	if err := Publier(ctx, b.db, b.f.opt.DossierVersions, signerProduit(protocole.ProduitCollecteur, binC), binC, "admin", t0); err != nil {
		t.Fatal(err)
	}
	if err := Publier(ctx, b.db, b.f.opt.DossierVersions, signerProduit("mirador-autre", binC), binC, "admin", t0); err == nil {
		t.Error("produit inconnu publié")
	}
	if err := DefinirCible(ctx, b.db, 0, "1.1.0", "admin", t0); err != nil {
		t.Fatal(err)
	}
	tokA, _ := TokenManuel(ctx, b.db, "pc", "admin", t0)
	tokC, _ := TokenManuel(ctx, b.db, "collecteur-lan", "admin", t0)
	if _, err := b.db.Exec(`UPDATE machines SET type = 'collecteur' WHERE nom = 'collecteur-lan'`); err != nil {
		t.Fatal(err)
	}
	for _, tok := range []string{tokA, tokC} {
		corps, _ := json.Marshal(protocole.Battement{Version: "1.0.0", Architecture: "amd64"})
		b.requete(http.MethodPost, protocole.CheminBattement, tok, corps, "")
	}
	_ = b.f.EcrireBattements(ctx)
	b.f.opt.Ingestion.Auth().Oublier()
	lire := func(tok string) (*protocole.Manifeste, []byte) {
		t.Helper()
		w := b.requete(http.MethodGet, protocole.CheminMaj, tok, nil, "", protocole.EnteteVersion, "1.0.0")
		var rep protocole.ReponseMaj
		_ = json.Unmarshal(w.Body.Bytes(), &rep)
		wb := b.requete(http.MethodGet, protocole.CheminBinaire+"?version=1.1.0&arch=amd64", tok, nil, "")
		return rep.Manifeste, wb.Body.Bytes()
	}
	mA, bA := lire(tokA)
	mC, bC := lire(tokC)
	if mA == nil || mA.Produit != "" || !bytes.Equal(bA, binA) {
		t.Fatalf("agent : %+v %q", mA, bA)
	}
	if mC == nil || mC.Produit != protocole.ProduitCollecteur || !bytes.Equal(bC, binC) {
		t.Fatalf("collecteur : %+v %q", mC, bC)
	}
	// Chacun ne peut installer que le sien (vérification du binaire réel).
	majagent.ProduitCourant = protocole.ProduitCollecteur
	defer func() { majagent.ProduitCourant = protocole.ProduitAgent }()
	if majagent.VerifierManifeste(mC, "1.0.0", majagent.ClesPubliques) != nil || majagent.VerifierManifeste(mA, "1.0.0", majagent.ClesPubliques) == nil {
		t.Error("le collecteur accepte le manifeste de l'agent, ou refuse le sien")
	}
	vs, _ := ListerVersions(ctx, b.db)
	if len(vs) != 2 {
		t.Errorf("versions : %+v", vs)
	}
}

// Demande de certificat du syslog jointe au battement : rangée pour un
// collecteur, ignorée pour un agent, refusée si illisible.
func TestBattementCSR(t *testing.T) {
	b := nouveauBanc(t)
	cle, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	der, _ := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{CommonName: "syslog.lan.exemple.org"},
		DNSNames: []string{"syslog.lan.exemple.org"}}, cle)
	csr := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der}))
	tokA, _ := TokenManuel(ctx, b.db, "pc", "admin", t0)
	tokC, _ := TokenManuel(ctx, b.db, "collecteur-lan", "admin", t0)
	_, _ = b.db.Exec(`UPDATE machines SET type = 'collecteur' WHERE nom = 'collecteur-lan'`)
	b.f.opt.Ingestion.Auth().Oublier()
	for _, tok := range []string{tokA, tokC} {
		corps, _ := json.Marshal(protocole.Battement{Version: "1.0.0", Architecture: "amd64", CSRSyslog: csr})
		if w := b.requete(http.MethodPost, protocole.CheminBattement, tok, corps, ""); w.Code != http.StatusNoContent {
			t.Fatalf("battement : %d", w.Code)
		}
	}
	idA, _ := IDMachine(ctx, b.db, "pc")
	idC, _ := IDMachine(ctx, b.db, "collecteur-lan")
	if e, _ := certsyslog.LireEtat(ctx, b.db, idC); !e.CSR {
		t.Error("demande du collecteur non rangée")
	}
	if e, _ := certsyslog.LireEtat(ctx, b.db, idA); e.CSR {
		t.Error("demande d'un agent rangée")
	}
}

// Mots de passe SNMP : chiffrés en base, envoyés en clair au seul collecteur
// (HTTPS), même ETag.
func TestSecretsSNMP(t *testing.T) {
	b := nouveauBanc(t)
	b.f.opt.Cle = []byte("0123456789abcdef0123456789abcdef")
	tokC, _ := TokenManuel(ctx, b.db, "collecteur-lan", "admin", t0)
	_, _ = b.db.Exec(`UPDATE machines SET type = 'collecteur' WHERE nom = 'collecteur-lan'`)
	b.f.opt.Ingestion.Auth().Oublier()
	idC, _ := IDMachine(ctx, b.db, "collecteur-lan")
	_ = CreerEquipement(ctx, b.db, "nas", idC, "admin", t0)
	mdp, _ := ChiffrerSecretSNMP(b.f.opt.Cle, "auth-tres-secret")
	cle, _ := ChiffrerSecretSNMP(b.f.opt.Cle, "priv-tres-secret")
	_ = CreerEquipement(ctx, b.db, "unifi", idC, "admin", t0)
	cleAPI, _ := ChiffrerSecretSNMP(b.f.opt.Cle, "cle-api-unifi-tres-secrete")
	doc := `{"collecteur":{"snmp":[{"machine":"nas","adresse":"192.168.1.20","prereglage":"synology","utilisateur":"mirador",` +
		`"auth":"SHA","mot_de_passe":"` + mdp + `","chiffrement":"AES","cle_chiffrement":"` + cle + `"}],` +
		`"unifi":{"machine":"unifi","adresse":"https://192.168.1.5","cle_api":"` + cleAPI + `"}}}`
	if err := DefinirConfig(ctx, b.db, "machine:"+itoa(idC), []byte(doc), "admin", t0); err != nil {
		t.Fatal(err)
	}
	enBase, etag, _ := ConfigMachine(ctx, b.db, idC)
	if strings.Contains(string(enBase), "tres-secret") {
		t.Fatal("secret en clair en base")
	}
	w := b.requete(http.MethodGet, protocole.CheminConfig, tokC, nil, "", protocole.EnteteCapacites, protocole.CapaciteCollecteur)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"mot_de_passe":"auth-tres-secret"`) || w.Header().Get("ETag") != etag ||
		!strings.Contains(w.Body.String(), `"cle_api":"cle-api-unifi-tres-secrete"`) {
		t.Fatalf("envoi au collecteur : %d %s", w.Code, w.Body)
	}
	// Mauvaise clé du serveur : rien n'est envoyé (le collecteur garde la sienne).
	b.f.opt.Cle = []byte("ffffffffffffffffffffffffffffffff")
	if w := b.requete(http.MethodGet, protocole.CheminConfig, tokC, nil, "", protocole.EnteteCapacites, protocole.CapaciteCollecteur); w.Code == http.StatusOK {
		t.Error("secret illisible : configuration envoyée quand même")
	}
}

// Noms réservés par l'interface (/machines/ajouter…) : refusés partout ;
// RelevesParCollecteur : équipement → collecteur en une requête.
func TestNomsReservesEtReleves(t *testing.T) {
	b := nouveauBanc(t)
	ctx := context.Background()
	t0 := time.Unix(1_700_000_000, 0)
	if _, err := TokenManuel(ctx, b.db, "Ajouter", "admin", t0); err == nil {
		t.Error("token manuel pour un nom réservé")
	}
	if _, err := CreerJetonEtiquettes(ctx, b.db, time.Hour, Etiquettes{Nom: "jeton"}, "admin", t0); err == nil {
		t.Error("jeton pour un nom réservé")
	}
	if _, err := TokenManuel(ctx, b.db, "collecteur-lan", "admin", t0); err != nil {
		t.Fatal(err)
	}
	if _, err := b.db.Exec(`UPDATE machines SET type = 'collecteur' WHERE nom = 'collecteur-lan'`); err != nil {
		t.Fatal(err)
	}
	idC, _ := IDMachine(ctx, b.db, "collecteur-lan")
	if err := CreerEquipement(ctx, b.db, "equipement", idC, "admin", t0); err == nil {
		t.Error("équipement au nom réservé")
	}
	if err := CreerEquipement(ctx, b.db, "synology", idC, "admin", t0); err != nil {
		t.Fatal(err)
	}
	r, err := RelevesParCollecteur(ctx, b.db)
	if err != nil || len(r) != 1 || r["synology"] != "collecteur-lan" {
		t.Fatalf("relevés : %v, %v", r, err)
	}
}
