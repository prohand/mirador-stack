// SPDX-License-Identifier: GPL-3.0-or-later

package sauvegarde

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"

	"mirador-stack/internal/server/secrets"
)

// Usages des secrets (liés au chiffré, voir le paquet secrets).
const (
	UsageCleSSH   = "sauvegarde.sftp.cle_ssh"
	UsageSecretS3 = "sauvegarde.s3.secret" // #nosec G101 -- nom d'usage d'un secret, pas un secret
)

// --- SFTP --------------------------------------------------------------------------

// SFTP : copie des archives sur un serveur SFTP, avec rétention.
type SFTP struct {
	Hote          string `json:"hote"` // nom:port
	Utilisateur   string `json:"utilisateur"`
	Dossier       string `json:"dossier"`        // chemin absolu sur le serveur
	EmpreinteHote string `json:"empreinte_hote"` // « SHA256:… » (ssh-keygen -lf) : OBLIGATOIRE
	CleSSH        string `json:"cle_ssh"`        // clé privée OpenSSH sans phrase, chiffrée en base
}

var reEmpreinte = regexp.MustCompile(`^SHA256:[A-Za-z0-9+/]{43}$`)

func (s *SFTP) valider() error {
	var errs []error
	if _, _, err := net.SplitHostPort(s.Hote); err != nil {
		errs = append(errs, errors.New("sftp.hote : nom:port attendu"))
	}
	if s.Utilisateur == "" || !strings.HasPrefix(s.Dossier, "/") {
		errs = append(errs, errors.New("sftp : utilisateur et dossier absolu obligatoires"))
	}
	if !reEmpreinte.MatchString(s.EmpreinteHote) {
		errs = append(errs, errors.New("sftp.empreinte_hote : « SHA256:… » obligatoire (ssh-keygen -lf), sinon un faux serveur passerait"))
	}
	if !secrets.EstChiffre(s.CleSSH) {
		errs = append(errs, errors.New("sftp.cle_ssh : doit être chiffrée avant enregistrement"))
	}
	return errors.Join(errs...)
}

type destinationSFTP struct {
	r   SFTP
	cle []byte
}

func (d *destinationSFTP) Nom() string { return "sftp " + d.r.Hote }

func (d *destinationSFTP) connecter(ctx context.Context) (*sftp.Client, *ssh.Client, error) {
	pem, err := secrets.Dechiffrer(d.cle, UsageCleSSH, d.r.CleSSH)
	if err != nil {
		return nil, nil, err
	}
	signeur, err := ssh.ParsePrivateKey([]byte(pem))
	if err != nil {
		return nil, nil, errors.New("clé SSH illisible (clé OpenSSH sans phrase de passe attendue)")
	}
	conf := &ssh.ClientConfig{
		User: d.r.Utilisateur,
		Auth: []ssh.AuthMethod{ssh.PublicKeys(signeur)},
		HostKeyCallback: func(_ string, _ net.Addr, k ssh.PublicKey) error {
			if ssh.FingerprintSHA256(k) != d.r.EmpreinteHote {
				return fmt.Errorf("empreinte du serveur %s différente de celle attendue : connexion refusée", ssh.FingerprintSHA256(k))
			}
			return nil
		},
		Timeout: 20 * time.Second,
	}
	var dial net.Dialer
	conn, err := dial.DialContext(ctx, "tcp", d.r.Hote)
	if err != nil {
		return nil, nil, err
	}
	c, chans, reqs, err := ssh.NewClientConn(conn, d.r.Hote, conf)
	if err != nil {
		_ = conn.Close()
		return nil, nil, err
	}
	cs := ssh.NewClient(c, chans, reqs)
	cl, err := sftp.NewClient(cs)
	if err != nil {
		_ = cs.Close()
		return nil, nil, err
	}
	return cl, cs, nil
}

// Envoyer copie l'archive (nom provisoire puis renommage : jamais d'archive
// à moitié copiée sous le nom final), puis applique la rétention distante.
func (d *destinationSFTP) Envoyer(ctx context.Context, chemin string, conserver int) error {
	cl, cs, err := d.connecter(ctx)
	if err != nil {
		return err
	}
	defer cs.Close()
	defer cl.Close()
	src, err := os.Open(chemin) // #nosec G304 -- archive écrite par le serveur
	if err != nil {
		return err
	}
	defer src.Close()
	nom := filepath.Base(chemin)
	final := path.Join(d.r.Dossier, nom)
	tmp := final + ".tmp"
	dst, err := cl.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC)
	if err != nil {
		return err
	}
	if _, err := io.Copy(dst, src); err != nil {
		_ = dst.Close()
		return err
	}
	if err := dst.Close(); err != nil {
		return err
	}
	_ = cl.Remove(final)
	if err := cl.Rename(tmp, final); err != nil {
		return err
	}
	// Rétention : seulement les fichiers nommés par mirador.
	entrees, err := cl.ReadDir(d.r.Dossier)
	if err != nil {
		return err
	}
	var noms []string
	for _, e := range entrees {
		if reNom.MatchString(e.Name()) {
			noms = append(noms, e.Name())
		}
	}
	sort.Slice(noms, func(i, j int) bool { // le nom contient la date : ordre = âge
		di, dj := reNom.FindStringSubmatch(noms[i])[1], reNom.FindStringSubmatch(noms[j])[1]
		if di != dj {
			return di > dj
		}
		return numero(noms[i]) > numero(noms[j])
	})
	var errs []error
	for _, n := range noms[min(conserver, len(noms)):] {
		errs = append(errs, cl.Remove(path.Join(d.r.Dossier, n)))
	}
	return errors.Join(errs...)
}

// --- S3 ----------------------------------------------------------------------------

// S3 : envoi des archives dans un compartiment compatible S3. Mirador
// n'efface rien : la rétention se règle par une règle de cycle de vie du
// compartiment, et la clé peut n'avoir que le droit d'écrire (une machine
// compromise ne peut pas détruire les sauvegardes).
type S3 struct {
	Adresse   string `json:"adresse"`    // https://s3.fr-par.scw.cloud
	Region    string `json:"region"`     // fr-par
	Seau      string `json:"seau"`       // compartiment
	Prefixe   string `json:"prefixe"`    // ex. mirador/
	CleAcces  string `json:"cle_acces"`  // identifiant (pas secret)
	Secret    string `json:"secret"`     // chiffré en base
	StyleHote bool   `json:"style_hote"` // https://seau.adresse/… au lieu de https://adresse/seau/…
}

// TailleMaxS3 : un seul envoi S3 est limité à 5 Gio.
const TailleMaxS3 = 5 << 30

var reSeau = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)

func (s *S3) valider() error {
	var errs []error
	u, err := url.Parse(s.Adresse)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.Path != "" && u.Path != "/" {
		errs = append(errs, errors.New("s3.adresse : https://hôte attendu"))
	}
	if s.Region == "" || !reSeau.MatchString(s.Seau) || s.CleAcces == "" {
		errs = append(errs, errors.New("s3 : région, compartiment et clé d'accès obligatoires"))
	}
	if strings.Contains(s.Prefixe, "..") || strings.HasPrefix(s.Prefixe, "/") {
		errs = append(errs, errors.New("s3.prefixe invalide"))
	}
	if !secrets.EstChiffre(s.Secret) {
		errs = append(errs, errors.New("s3.secret : doit être chiffré avant enregistrement"))
	}
	return errors.Join(errs...)
}

type destinationS3 struct {
	r      S3
	cle    []byte
	client *http.Client // nil : client par défaut (tests : remplacé)
}

func (d *destinationS3) Nom() string { return "s3 " + d.r.Seau }

func (d *destinationS3) Envoyer(ctx context.Context, chemin string, _ int) error {
	secret, err := secrets.Dechiffrer(d.cle, UsageSecretS3, d.r.Secret)
	if err != nil {
		return err
	}
	f, err := os.Open(chemin) // #nosec G304 -- archive écrite par le serveur
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if info.Size() > TailleMaxS3 {
		return errors.New("archive de plus de 5 Gio : trop grosse pour un envoi S3 (utiliser SFTP, ou ne pas inclure métriques et logs)")
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	empreinte := hex.EncodeToString(h.Sum(nil))
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	base, _ := url.Parse(d.r.Adresse)
	cle := strings.TrimPrefix(d.r.Prefixe+filepath.Base(chemin), "/")
	u := *base
	if d.r.StyleHote {
		u.Host = d.r.Seau + "." + base.Host
		u.Path = "/" + cle
	} else {
		u.Path = "/" + d.r.Seau + "/" + cle
	}
	ctx, annuler := context.WithTimeout(ctx, 2*time.Hour)
	defer annuler()
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, u.String(), f)
	if err != nil {
		return err
	}
	req.ContentLength = info.Size()
	req.Header.Set("x-amz-content-sha256", empreinte)
	req.Header.Set("Content-Type", "application/octet-stream")
	identite := aws.Credentials{AccessKeyID: d.r.CleAcces, SecretAccessKey: secret}
	if err := v4.NewSigner().SignHTTP(ctx, identite, req, empreinte, "s3", d.r.Region, time.Now()); err != nil {
		return err
	}
	client := d.client
	if client == nil {
		client = &http.Client{}
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	extrait, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d : %s", resp.StatusCode, strings.TrimSpace(string(extrait)))
	}
	return nil
}
