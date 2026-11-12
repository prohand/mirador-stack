// SPDX-License-Identifier: GPL-3.0-or-later

package auth

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mirador-stack/internal/server/auth/fauxcle"
	"mirador-stack/internal/server/base"
)

// dbTest : base du test en cours (posée par baseTest).
var dbTest *sql.DB

const (
	domaineTest = "mirador.example.org"
	origineTest = "https://mirador.example.org"
)

// Création puis connexion, pour les trois algorithmes acceptés.
func TestPasskeyCreationEtConnexion(t *testing.T) {
	for nom, alg := range map[string]int{"ES256": algES256, "EdDSA": algEdDSA, "RS256": algRS256} {
		t.Run(nom, func(t *testing.T) {
			cle := fauxcle.Nouvelle(alg, domaineTest, origineTest)
			defi, _ := NouveauDefiPasskey()
			client, att := cle.Creer(defi)
			p, err := VerifierCreation(client, att, defi, domaineTest, origineTest)
			if err != nil {
				t.Fatal(err)
			}
			if p.Algorithme != alg || string(p.Identifiant) != string(cle.Identifiant) || p.Domaine != domaineTest {
				t.Fatalf("clé enregistrée : %+v", p)
			}
			defi2, _ := NouveauDefiPasskey()
			c, a, s := cle.Signer(defi2)
			if _, err := VerifierConnexion(p, c, a, s, defi2, domaineTest, origineTest); err != nil {
				t.Fatalf("connexion refusée : %v", err)
			}
			// Tout ce qui doit être refusé à la connexion.
			autre := fauxcle.Nouvelle(alg, domaineTest, origineTest)
			_, _, sigAutre := autre.Signer(defi2)
			ailleurs := *cle
			ailleurs.Origine = "https://mirador.example.org.pirate.example"
			cA, aA, sA := ailleurs.Signer(defi2)
			refuses := map[string][3]string{
				"défi rejoué (un autre défi est attendu)": {c, a, s},
				"signature d'une autre clé":               {c, a, sigAutre},
				"origine d'hameçonnage":                   {cA, aA, sA},
				"signature tronquée":                      {c, a, s[:len(s)-4]},
			}
			for cas, r := range refuses {
				attendu := defi2
				if strings.HasPrefix(cas, "défi") {
					attendu, _ = NouveauDefiPasskey()
				}
				if _, err := VerifierConnexion(p, r[0], r[1], r[2], attendu, domaineTest, origineTest); !errors.Is(err, ErrPasskey) {
					t.Errorf("%s : accepté (%v)", cas, err)
				}
			}
			// Clé créée pour un autre domaine : inutilisable ici.
			if _, err := VerifierConnexion(p, c, a, s, defi2, "autre.example.org", "https://autre.example.org"); !errors.Is(err, ErrPasskey) {
				t.Error("clé acceptée sous un autre domaine")
			}
		})
	}
}

func TestPasskeyCreationRefusee(t *testing.T) {
	cle := fauxcle.Nouvelle(algES256, domaineTest, origineTest)
	defi, _ := NouveauDefiPasskey()
	client, att := cle.Creer(defi)
	autreDefi, _ := NouveauDefiPasskey()
	// Une réponse de connexion ne vaut pas création (type webauthn.get).
	clientGet, _, _ := cle.Signer(defi)
	cas := map[string][4]string{
		"défi faux":          {client, att, autreDefi, origineTest},
		"défi vide":          {client, att, "", origineTest},
		"autre origine":      {client, att, defi, "https://pirate.example"},
		"mauvais type":       {clientGet, att, defi, origineTest},
		"attestation abîmée": {client, att[:len(att)/2], defi, origineTest},
		"attestation vide":   {client, "", defi, origineTest},
		"pas du base64":      {"***", att, defi, origineTest},
		"attestation énorme": {client, strings.Repeat("A", 200_000), defi, origineTest},
	}
	for nom, c := range cas {
		if _, err := VerifierCreation(c[0], c[1], c[2], domaineTest, c[3]); err == nil {
			t.Errorf("%s : accepté", nom)
		}
	}
	if _, err := VerifierCreation(client, att, defi, "autre.example.org", origineTest); !errors.Is(err, ErrPasskey) {
		t.Error("clé acceptée pour un autre domaine")
	}
}

// Compteur de signatures : il doit avancer quand la clé s'en sert.
func TestPasskeyCompteur(t *testing.T) {
	cle := fauxcle.Nouvelle(algES256, domaineTest, origineTest)
	cle.Compteur = 5
	defi, _ := NouveauDefiPasskey()
	client, att := cle.Creer(defi)
	p, err := VerifierCreation(client, att, defi, domaineTest, origineTest)
	if err != nil || p.Compteur != 5 {
		t.Fatalf("%+v %v", p, err)
	}
	c, a, s := cle.Signer(defi)
	n, err := VerifierConnexion(p, c, a, s, defi, domaineTest, origineTest)
	if err != nil || n != 6 {
		t.Fatalf("compteur %d %v", n, err)
	}
	p.Compteur = 9 // la base a vu plus loin : la réponse vient d'une copie
	c, a, s = cle.Signer(defi)
	if _, err := VerifierConnexion(p, c, a, s, defi, domaineTest, origineTest); !errors.Is(err, ErrPasskey) {
		t.Error("compteur en recul accepté")
	}
}

func TestDomaineEtOrigine(t *testing.T) {
	for hote, attendu := range map[string][2]string{
		"mirador.example.org":     {"mirador.example.org", "https://mirador.example.org"},
		"Mirador.Example.org:444": {"mirador.example.org", "https://Mirador.Example.org:444"},
		"localhost:9441":          {"localhost", "http://localhost:9441"},
	} {
		d, o, err := DomaineEtOrigine(hote)
		if err != nil || d != attendu[0] || o != attendu[1] {
			t.Errorf("%s : %s %s %v", hote, d, o, err)
		}
	}
	for _, mauvais := range []string{"", "a/b", "a b", "moi@pirate.example"} {
		if _, _, err := DomaineEtOrigine(mauvais); err == nil {
			t.Errorf("%q accepté", mauvais)
		}
	}
	if PasskeyPossible("192.0.2.1") || PasskeyPossible("::1") || !PasskeyPossible("mirador.example.org") {
		t.Error("PasskeyPossible : une adresse IP n'est pas un domaine")
	}
}

func baseTest(t *testing.T) (context.Context, Utilisateur, func() time.Time) {
	t.Helper()
	ctx := context.Background()
	db, err := base.Ouvrir(filepath.Join(t.TempDir(), "mirador.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := base.Migrer(ctx, db, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	t0 := time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC)
	if err := CreerUtilisateur(ctx, db, "admin", RoleAdmin, "une longue phrase de passe", "test", "", t0); err != nil {
		t.Fatal(err)
	}
	u, _ := LireUtilisateur(ctx, db, "admin")
	dbTest = db
	return ctx, u, func() time.Time { return t0 }
}

func TestPasskeysEnBase(t *testing.T) {
	ctx, u, maintenant := baseTest(t)
	db := dbTest
	cle := fauxcle.Nouvelle(algES256, domaineTest, origineTest)
	defi, _ := NouveauDefiPasskey()
	client, att := cle.Creer(defi)
	p, _ := VerifierCreation(client, att, defi, domaineTest, origineTest)
	if err := AjouterPasskey(ctx, db, u, p, "  Téléphone  ", "192.0.2.1", maintenant()); err != nil {
		t.Fatal(err)
	}
	if err := AjouterPasskey(ctx, db, u, p, "encore", "192.0.2.1", maintenant()); err == nil {
		t.Error("même passkey enregistrée deux fois")
	}
	liste, err := ListerPasskeys(ctx, db, u.ID)
	if err != nil || len(liste) != 1 || liste[0].Nom != "Téléphone" || liste[0].IdentifiantB64() != base64.RawURLEncoding.EncodeToString(cle.Identifiant) {
		t.Fatalf("%+v %v", liste, err)
	}
	if err := NoterPasskeyUtilisee(ctx, db, liste[0].ID, 7, maintenant()); err != nil {
		t.Fatal(err)
	}
	// La passkey d'un autre compte ne se supprime pas d'ici.
	_ = CreerUtilisateur(ctx, db, "autre", RoleLecture, "une autre phrase de passe", "test", "", maintenant())
	autre, _ := LireUtilisateur(ctx, db, "autre")
	if err := SupprimerPasskey(ctx, db, autre, liste[0].ID, "", maintenant()); err == nil {
		t.Error("passkey supprimée par un autre compte")
	}
	if err := SupprimerPasskey(ctx, db, u, liste[0].ID, "", maintenant()); err != nil {
		t.Fatal(err)
	}
	if liste, _ := ListerPasskeys(ctx, db, u.ID); len(liste) != 0 {
		t.Error("passkey encore là")
	}
	var n int
	_ = db.QueryRow(`SELECT count(*) FROM audit WHERE action LIKE 'utilisateur_passkey%'`).Scan(&n)
	if n != 2 {
		t.Errorf("audit : %d lignes", n)
	}
}

func TestCodesSecours(t *testing.T) {
	ctx, u, maintenant := baseTest(t)
	db := dbTest
	codes, err := CreerCodesSecours(ctx, db, u, "web:admin", "192.0.2.1", maintenant())
	if err != nil || len(codes) != NombreCodesSecours {
		t.Fatal(err)
	}
	vus := map[string]bool{}
	for _, c := range codes {
		if len(c) != 14 || !EstCodeSecours(c) || vus[c] {
			t.Fatalf("code %q mal formé ou en double", c)
		}
		vus[c] = true
	}
	// En base : jamais le code, seulement son empreinte.
	var enClair int
	_ = db.QueryRow(`SELECT count(*) FROM codes_secours WHERE empreinte = ?`, []byte(codes[0])).Scan(&enClair)
	if enClair != 0 {
		t.Error("code gardé en clair")
	}
	// Saisie tolérante (minuscules, espaces), mais une seule fois.
	saisie := strings.ToLower(strings.ReplaceAll(codes[0], "-", " "))
	if ok, err := UtiliserCodeSecours(ctx, db, u, saisie, "", maintenant()); !ok || err != nil {
		t.Fatalf("code refusé : %v", err)
	}
	if ok, _ := UtiliserCodeSecours(ctx, db, u, codes[0], "", maintenant()); ok {
		t.Error("code utilisé deux fois")
	}
	for _, faux := range []string{"", "123456", "AAAA-AAAA-AAAA", codes[1] + "X", "' OR 1=1 --"} {
		if ok, _ := UtiliserCodeSecours(ctx, db, u, faux, "", maintenant()); ok {
			t.Errorf("code faux accepté : %q", faux)
		}
	}
	if n, _ := CodesSecoursRestants(ctx, db, u.ID); n != 9 {
		t.Errorf("restants : %d", n)
	}
	// Le code d'un compte ne sert pas à un autre.
	_ = CreerUtilisateur(ctx, db, "autre", RoleLecture, "une autre phrase de passe", "test", "", maintenant())
	autre, _ := LireUtilisateur(ctx, db, "autre")
	if ok, _ := UtiliserCodeSecours(ctx, db, autre, codes[1], "", maintenant()); ok {
		t.Error("code accepté pour un autre compte")
	}
	// De nouveaux codes annulent les anciens.
	nouveaux, _ := CreerCodesSecours(ctx, db, u, "web:admin", "", maintenant())
	if ok, _ := UtiliserCodeSecours(ctx, db, u, codes[1], "", maintenant()); ok {
		t.Error("ancien code encore valable")
	}
	if ok, _ := UtiliserCodeSecours(ctx, db, u, nouveaux[0], "", maintenant()); !ok {
		t.Error("nouveau code refusé")
	}
	// Remise à zéro : plus rien.
	if err := EffacerSecoursEtPasskeys(ctx, db, "admin"); err != nil {
		t.Fatal(err)
	}
	if n, _ := CodesSecoursRestants(ctx, db, u.ID); n != 0 {
		t.Errorf("après effacement : %d", n)
	}
}

// Le lecteur CBOR et la vérification ne doivent jamais planter, quelle que
// soit l'entrée (elle vient du navigateur, donc d'Internet).
func FuzzPasskey(f *testing.F) {
	cle := fauxcle.Nouvelle(algES256, domaineTest, origineTest)
	client, att := cle.Creer("defi")
	brut, _ := base64.RawURLEncoding.DecodeString(att)
	f.Add(client, brut)
	f.Add("", []byte{0xbf, 0xff})
	f.Add("e30", []byte{0x9b, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff})
	f.Fuzz(func(t *testing.T, client string, att []byte) {
		_, _, _ = lireCBOR(att, 0)
		_, _ = VerifierCreation(client, base64.RawURLEncoding.EncodeToString(att), "defi", domaineTest, origineTest)
		p := &Passkey{Domaine: domaineTest, Algorithme: algES256, ClePublique: att}
		_, _ = VerifierConnexion(p, client, base64.RawURLEncoding.EncodeToString(att), client, "defi", domaineTest, origineTest)
	})
}

// Coût d'une connexion par passkey (vérification d'une signature ES256).
func BenchmarkVerifierConnexion(b *testing.B) {
	cle := fauxcle.Nouvelle(algES256, domaineTest, origineTest)
	client, att := cle.Creer("defi")
	p, _ := VerifierCreation(client, att, "defi", domaineTest, origineTest)
	c, a, s := cle.Signer("defi")
	b.ReportAllocs()
	for b.Loop() {
		if _, err := VerifierConnexion(p, c, a, s, "defi", domaineTest, origineTest); err != nil {
			b.Fatal(err)
		}
	}
}
