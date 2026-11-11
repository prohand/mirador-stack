// SPDX-License-Identifier: GPL-3.0-or-later

package racine

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"mirador-stack/internal/agent/config"
)

// progFactices : sorties d'exemple des programmes (aucun n'est lancé).
type progFactices struct {
	ipt, nft, scan []byte
	errIpt, errNft error
	disques        map[string][]byte
	appelsSmart    int
}

func (p *progFactices) iptablesInput() ([]byte, error) { return p.ipt, p.errIpt }
func (p *progFactices) nftRuleset() ([]byte, error)    { return p.nft, p.errNft }
func (p *progFactices) smartScan() ([]byte, error)     { return p.scan, nil }
func (p *progFactices) smartDisque(d, _ string) ([]byte, error) {
	p.appelsSmart++
	return p.disques[d], nil
}

const iptablesDrop = `-P INPUT DROP
-A INPUT -i lo -j ACCEPT
-A INPUT -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT
-A INPUT -p tcp -m tcp --dport 22 -j ACCEPT
`

const iptablesAccept = "-P INPUT ACCEPT\n"

const nftDrop = `table inet filter {
	chain input {
		type filter hook input priority filter; policy drop;
		ct state established,related accept
		iif "lo" accept
		tcp dport 22 accept
	}
	chain forward {
		type filter hook forward priority filter; policy drop;
	}
}
`

func TestPareFeu(t *testing.T) {
	cas := []struct {
		nom       string
		prog      *progFactices
		outil     string
		politique string
		regles    int
	}{
		{"iptables DROP", &progFactices{ipt: []byte(iptablesDrop)}, "iptables", "DROP", 3},
		{"iptables ACCEPT puis nftables drop", &progFactices{ipt: []byte(iptablesAccept), nft: []byte(nftDrop)}, "nftables", "DROP", 3},
		{"iptables ACCEPT, pas de nft", &progFactices{ipt: []byte(iptablesAccept), errNft: errAbsent}, "iptables", "ACCEPT", 0},
		{"aucun outil", &progFactices{errIpt: errAbsent, errNft: errAbsent}, "aucun", "AUCUNE", 0},
	}
	for _, c := range cas {
		t.Run(c.nom, func(t *testing.T) {
			pf, err := lirePareFeu(c.prog)
			if err != nil {
				t.Fatal(err)
			}
			if pf.Outil != c.outil || pf.Politique != c.politique || pf.Regles != c.regles {
				t.Errorf("obtenu %+v", pf)
			}
		})
	}
}

const scanSmart = `{"devices":[
 {"name":"/dev/sda","type":"sat"},
 {"name":"/dev/nvme0","type":"nvme"},
 {"name":"/dev/sdb","type":"sat"},
 {"name":"/dev/sdz; rm -rf /","type":"sat"},
 {"name":"/dev/sdc","type":"sat -d autre"}
]}`

const smartATA = `{"model_name":"WDC WD40EFRX","smart_status":{"passed":true},
 "temperature":{"current":38},"power_on_time":{"hours":41000},
 "ata_smart_attributes":{"table":[
  {"id":5,"name":"Reallocated_Sector_Ct","raw":{"value":8}},
  {"id":9,"name":"Power_On_Hours","raw":{"value":41000}},
  {"id":197,"name":"Current_Pending_Sector","raw":{"value":0}}]}}`

const smartNVMe = `{"model_name":"Samsung SSD 980","smart_status":{"passed":false},
 "temperature":{"current":51},"power_on_time":{"hours":1200},
 "nvme_smart_health_information_log":{"available_spare":97}}`

const smartVeille = `{"smartctl":{"messages":[{"string":"Device is in STANDBY mode, exit(2)","severity":"information"}]}}`

func TestSmart(t *testing.T) {
	prog := &progFactices{scan: []byte(scanSmart), disques: map[string][]byte{
		"/dev/sda": []byte(smartATA), "/dev/nvme0": []byte(smartNVMe), "/dev/sdb": []byte(smartVeille),
	}}
	disques, err := lireSmart(prog)
	if err != nil {
		t.Fatal(err)
	}
	if prog.appelsSmart != 3 {
		t.Errorf("les noms ou types suspects doivent être ignorés (%d appels)", prog.appelsSmart)
	}
	if len(disques) != 3 {
		t.Fatalf("3 disques attendus : %+v", disques)
	}
	sda, nvme, sdb := disques[0], disques[1], disques[2]
	if sda.Nom != "sda" || !*sda.SanteOK || *sda.Temperature != 38 || sda.Attributs["Reallocated_Sector_Ct"] != 8 {
		t.Errorf("sda mal lu : %+v", sda)
	}
	if _, garde := sda.Attributs["Power_On_Hours"]; garde {
		t.Error("seuls les attributs utiles sont gardés")
	}
	if *nvme.SanteOK || *nvme.ReserveNVMe != 97 {
		t.Errorf("nvme mal lu : %+v", nvme)
	}
	if !sdb.EnVeille || sdb.SanteOK != nil {
		t.Errorf("disque en veille mal lu : %+v", sdb)
	}
}

// ecrireCertificat crée un certificat autosigné dans dir/nom.
func ecrireCertificat(t *testing.T, chemin, cn string, fin time.Time) {
	t.Helper()
	cle, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	modele := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: cn},
		Issuer:       pkix.Name{CommonName: cn},
		NotBefore:    fin.Add(-90 * 24 * time.Hour),
		NotAfter:     fin,
	}
	der, err := x509.CreateCertificate(rand.Reader, modele, modele, &cle.PublicKey, cle)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(chemin), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(chemin, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestCertificats(t *testing.T) {
	dir := t.TempDir()
	fin := time.Date(2027, 1, 15, 12, 0, 0, 0, time.UTC)
	ecrireCertificat(t, filepath.Join(dir, "live/exemple.org/cert.pem"), "exemple.org", fin)
	ecrireCertificat(t, filepath.Join(dir, "local/interne.crt"), "interne.lan", fin.AddDate(0, 6, 0))
	// Une clé privée rangée par erreur dans un dossier scanné : erreur comptée.
	if err := os.WriteFile(filepath.Join(dir, "local/cle.pem"),
		[]byte("-----BEGIN PRIVATE KEY-----\nAAAA\n-----END PRIVATE KEY-----\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Lien symbolique vers le même fichier (cas /etc/pve/local) : compté une fois.
	if err := os.Symlink(filepath.Join(dir, "local/interne.crt"), filepath.Join(dir, "local/lien.crt")); err != nil {
		t.Fatal(err)
	}
	c := lireCertificats([]string{
		filepath.Join(dir, "live/*/cert.pem"),
		filepath.Join(dir, "local/*.crt"),
		filepath.Join(dir, "local/*.pem"),
		filepath.Join(dir, "absent/*.pem"),
	}, time.Now())
	if c.Trouves != 2 || c.Erreurs != 1 || len(c.Liste) != 2 {
		t.Fatalf("obtenu %+v", c)
	}
	if c.Liste[0].CN != "exemple.org" || !c.Liste[0].Fin.Equal(fin) {
		t.Errorf("certificat mal lu : %+v", c.Liste[0])
	}
}

func TestEcrireLireEtReprise(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Defaut()
	cfg.PareFeu, cfg.Smart = true, true
	cfg.Certificats = nil
	cfg.FichierRoot = filepath.Join(dir, "root.json")
	prog := &progFactices{ipt: []byte(iptablesDrop), scan: []byte(scanSmart),
		disques: map[string][]byte{"/dev/sda": []byte(smartATA)}}

	t0 := time.Date(2026, 11, 8, 10, 0, 0, 0, time.UTC)
	r := Mesurer(cfg, t0, nil, prog)
	if err := Ecrire(cfg.FichierRoot, r); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(cfg.FichierRoot)
	if err != nil || info.Mode().Perm() != 0o640 {
		t.Fatalf("fichier absent ou droits incorrects : %v %v", info, err)
	}
	relu, err := Lire(cfg.FichierRoot)
	if err != nil || relu.PareFeu.Politique != "DROP" || len(relu.Smart) != 1 {
		t.Fatalf("relecture incorrecte : %+v %v", relu, err)
	}
	// 15 min plus tard : SMART n'est pas relu (moins de 30 min), il est repris.
	appels := prog.appelsSmart
	r2 := Mesurer(cfg, t0.Add(15*time.Minute), relu, prog)
	if prog.appelsSmart != appels || len(r2.Smart) != 1 || !r2.SmartLu.Equal(relu.SmartLu) {
		t.Error("SMART aurait dû être repris de la mesure précédente")
	}
	// 31 min plus tard : relu.
	Mesurer(cfg, t0.Add(31*time.Minute), relu, prog)
	if prog.appelsSmart == appels {
		t.Error("SMART aurait dû être relu après 30 min")
	}
}

func TestLireRefuse(t *testing.T) {
	dir := t.TempDir()
	chemin := filepath.Join(dir, "root.json")
	if err := os.WriteFile(chemin, []byte(`{"version":"x","inconnu":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Lire(chemin); err == nil {
		t.Error("un champ inconnu doit être refusé")
	}
}

func TestVerifierProgramme(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "outil")
	if err := os.WriteFile(f, nil, 0o777); err != nil { // #nosec G306 -- test : fichier volontairement trop ouvert
		t.Fatal(err)
	}
	if err := os.Chmod(f, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := verifierProgramme(f); err == nil {
		t.Error("un programme modifiable par tous doit être refusé")
	}
	if err := verifierProgramme(filepath.Join(dir, "absent")); err != errAbsent {
		t.Errorf("programme absent : %v", err)
	}
}

// TestIptablesReel : si on est root et qu'iptables existe (machine de test),
// la vraie commande doit marcher.
func TestIptablesReel(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("nécessite root")
	}
	if _, err := os.Stat(cheminIptables); err != nil {
		t.Skip("iptables absent")
	}
	pf, err := lirePareFeu(programmesReels())
	if err != nil {
		t.Skipf("iptables inutilisable ici : %v", err)
	}
	t.Logf("machine de test : %+v", pf)
}

func FuzzAnalyses(f *testing.F) {
	f.Add([]byte(iptablesDrop))
	f.Add([]byte(nftDrop))
	f.Add([]byte(smartATA))
	f.Fuzz(func(t *testing.T, b []byte) {
		analyserIptables(b)
		analyserNft(b)
		_, _ = analyserSmart("/dev/sda", b)
	})
}
