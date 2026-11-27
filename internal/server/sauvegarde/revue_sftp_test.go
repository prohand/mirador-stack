// SPDX-License-Identifier: GPL-3.0-or-later

package sauvegarde

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"mirador-stack/internal/server/secrets"
)

// Revue du 27/11 (corrigé) : serveur SFTP qui accepte la connexion TCP puis ne répond plus :
// ssh.NewClientConn n'a aucun délai (conf.Timeout ne sert qu'à ssh.Dial) et
// ctx n'est plus regardé : Envoyer ne rend jamais la main.
func TestRevueSFTPBloque(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			defer c.Close() // muet
		}
	}()
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	blk, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatal(err)
	}
	cle := make([]byte, 32)
	chiffre, err := secrets.Chiffrer(cle, UsageCleSSH, string(pem.EncodeToMemory(blk)))
	if err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(t.TempDir(), "mirador-20261110-020000.sauvegarde")
	_ = os.WriteFile(archive, []byte("x"), 0o600)
	d := &destinationSFTP{r: SFTP{Hote: l.Addr().String(), Utilisateur: "u", Dossier: "/s", EmpreinteHote: "SHA256:" + string(make([]byte, 0)), CleSSH: chiffre}, cle: cle}
	ctx, annuler := context.WithTimeout(context.Background(), time.Second)
	defer annuler()
	fini := make(chan error, 1)
	go func() { fini <- d.Envoyer(ctx, archive, 3) }()
	select {
	case <-fini:
	case <-time.After(25 * time.Second):
		t.Fatal("Envoyer bloqué malgré ctx expiré (et Timeout de 20 s) : planificateur et arrêt du serveur bloqués")
	}
}
