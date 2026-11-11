// SPDX-License-Identifier: GPL-3.0-or-later

package sauvegarde

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/pem"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"

	"mirador-stack/internal/server/secrets"
)

// serveurSFTP : vrai serveur SSH + SFTP en mémoire du test (fichiers dans
// un dossier temporaire). Renvoie son adresse et l'empreinte de sa clé.
func serveurSFTP(t *testing.T, cleClient ssh.PublicKey) (string, string) {
	t.Helper()
	_, privHote, _ := ed25519.GenerateKey(rand.Reader)
	hote, _ := ssh.NewSignerFromKey(privHote)
	conf := &ssh.ServerConfig{PublicKeyCallback: func(_ ssh.ConnMetadata, k ssh.PublicKey) (*ssh.Permissions, error) {
		if !bytes.Equal(k.Marshal(), cleClient.Marshal()) {
			return nil, io.EOF
		}
		return nil, nil
	}}
	conf.AddHostKey(hote)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	t.Cleanup(func() { _ = l.Close(); wg.Wait() })
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, chans, reqs, err := ssh.NewServerConn(c, conf)
				if err != nil {
					return
				}
				go ssh.DiscardRequests(reqs)
				for nc := range chans {
					ch, demandes, err := nc.Accept()
					if err != nil {
						continue
					}
					go func() {
						for d := range demandes {
							ok := d.Type == "subsystem" && string(d.Payload[4:]) == "sftp"
							_ = d.Reply(ok, nil)
							if ok {
								srv, _ := sftp.NewServer(ch)
								_ = srv.Serve()
								_ = ch.Close()
							}
						}
					}()
				}
			}()
		}
	}()
	return l.Addr().String(), ssh.FingerprintSHA256(hote.PublicKey())
}

func cleClientSSH(t *testing.T) (string, ssh.PublicKey) {
	t.Helper()
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	bloc, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatal(err)
	}
	sp, _ := ssh.NewPublicKey(pub)
	return string(pem.EncodeToMemory(bloc)), sp
}

func TestDestinationSFTP(t *testing.T) {
	pemCle, pub := cleClientSSH(t)
	adresse, empreinte := serveurSFTP(t, pub)
	distant := t.TempDir()
	r, err := PreparerSecrets(Reglages{SFTP: &SFTP{Hote: adresse, Utilisateur: "mirador", Dossier: distant,
		EmpreinteHote: empreinte, CleSSH: pemCle}}, Reglages{}, cle)
	if err != nil {
		t.Fatal(err)
	}
	if !secrets.EstChiffre(r.SFTP.CleSSH) || r.SFTP.valider() != nil {
		t.Fatalf("réglages : %v", r.SFTP.valider())
	}
	local := t.TempDir()
	for i := 0; i < 4; i++ {
		nom := filepath.Join(local, "mirador-2026111"+string(rune('0'+i))+"-031700.sauvegarde")
		_ = os.WriteFile(nom, []byte("archive "+nom), 0o600)
		d := &destinationSFTP{r: *r.SFTP, cle: cle}
		if err := d.Envoyer(ctx, nom, 2); err != nil {
			t.Fatalf("envoi %d : %v", i, err)
		}
	}
	entrees, _ := os.ReadDir(distant)
	if len(entrees) != 2 || entrees[1].Name() != "mirador-20261113-031700.sauvegarde" {
		t.Fatalf("rétention distante : %v", entrees)
	}
	// Mauvaise empreinte : refus (faux serveur).
	faux := *r.SFTP
	faux.EmpreinteHote = "SHA256:" + strings.Repeat("A", 43)
	d := &destinationSFTP{r: faux, cle: cle}
	if err := d.Envoyer(ctx, filepath.Join(local, "mirador-20261110-031700.sauvegarde"), 2); err == nil || !strings.Contains(err.Error(), "empreinte") {
		t.Fatalf("empreinte fausse : %v", err)
	}
	// Réglages renvoyés masqués : le secret est gardé.
	r2, err := PreparerSecrets(r.Masques(), r, cle)
	if err != nil || r2.SFTP.CleSSH != r.SFTP.CleSSH {
		t.Fatal("secret perdu en renvoyant les réglages masqués")
	}
	if strings.Contains(r.Masques().SFTP.CleSSH, "chiffre") {
		t.Error("secret affiché")
	}
}

func TestDestinationS3(t *testing.T) {
	var recu []byte
	var entetes http.Header
	var chemin string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		recu, _ = io.ReadAll(r.Body)
		entetes, chemin = r.Header.Clone(), r.URL.Path
		if r.Method != http.MethodPut {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	r, err := PreparerSecrets(Reglages{S3: &S3{Adresse: "https://s3.exemple.org", Region: "fr-par", Seau: "sauvegardes",
		Prefixe: "mirador/", CleAcces: "CLEDETEST", Secret: "secret-de-test"}}, Reglages{}, cle)
	if err != nil || r.S3.valider() != nil {
		t.Fatal(err, r.S3.valider())
	}
	local := filepath.Join(t.TempDir(), "mirador-20261110-031700.sauvegarde")
	contenu := bytes.Repeat([]byte("archive "), 1000)
	_ = os.WriteFile(local, contenu, 0o600)
	d := &destinationS3{r: *r.S3, cle: cle}
	d.r.Adresse = srv.URL // faux S3 local (http)
	if err := d.Envoyer(ctx, local, 0); err != nil {
		t.Fatal(err)
	}
	h := sha256.Sum256(contenu)
	if !bytes.Equal(recu, contenu) || chemin != "/sauvegardes/mirador/mirador-20261110-031700.sauvegarde" ||
		entetes.Get("X-Amz-Content-Sha256") != hex.EncodeToString(h[:]) ||
		!strings.HasPrefix(entetes.Get("Authorization"), "AWS4-HMAC-SHA256 Credential=CLEDETEST/") ||
		!strings.Contains(entetes.Get("Authorization"), "/fr-par/s3/aws4_request") {
		t.Fatalf("requête S3 : %s %v", chemin, entetes)
	}
	if strings.Contains(entetes.Get("Authorization"), "secret-de-test") {
		t.Fatal("secret envoyé en clair")
	}
	// Erreur du service : remontée.
	srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, "AccessDenied")
	})
	if err := d.Envoyer(ctx, local, 0); err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("refus S3 : %v", err)
	}
}

func TestValiderReglages(t *testing.T) {
	if err := ReglagesDefaut().Valider(); err != nil {
		t.Fatal(err)
	}
	mauvais := []Reglages{
		{Heure: "25:00", Conserver: 3},
		{Heure: "02:43", Conserver: 0},
		{Heure: "02:43", Conserver: 3, SFTP: &SFTP{Hote: "x:22", Utilisateur: "u", Dossier: "/s", CleSSH: "en clair"}},
		{Heure: "02:43", Conserver: 3, S3: &S3{Adresse: "http://s3", Region: "r", Seau: "s3", CleAcces: "a", Secret: "x"}},
	}
	for i, r := range mauvais {
		if r.Valider() == nil {
			t.Errorf("réglages %d acceptés", i)
		}
	}
}
