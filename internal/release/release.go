// SPDX-License-Identifier: GPL-3.0-or-later

// Package release : signature des versions de mirador-agent, sur le poste de
// l'utilisateur (Windows), jamais sur le serveur (SPEC.md § 6.8 et § 16.2).
//
// La clé privée Ed25519 n'existe en clair qu'en mémoire, le temps d'une
// signature. Sur le disque, elle est chiffrée par une phrase de passe :
//
//	clé de chiffrement = Argon2id(phrase, sel aléatoire)   (lent et gourmand en mémoire :
//	                                                        une phrase volée se devine mal)
//	fichier            = AES-256-GCM(clé, graine Ed25519)  (toute modification est détectée)
package release

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"debug/elf"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"

	"golang.org/x/crypto/argon2"

	"mirador-stack/internal/protocole"
)

// FormatCle : version du format du fichier de clé.
const FormatCle = "mirador-cle-privee-v1"

// Paramètres Argon2id (recommandation RFC 9106, 2e choix : 3 passes, 256 Mio).
// ~1 s sur un PC de bureau, une fois par signature. Variables pour que les
// tests puissent aller plus vite ; jamais modifiées par le programme.
var (
	argonTemps   uint32 = 3
	argonMemoire uint32 = 256 * 1024 // Kio
	argonFils    uint8  = 4
)

// LongueurMinPhrase : en dessous, la phrase de passe est refusée.
const LongueurMinPhrase = 12

// FichierCle : contenu du fichier (JSON). Rien de secret en clair.
type FichierCle struct {
	Format     string `json:"format"`
	KDF        string `json:"kdf"`
	Temps      uint32 `json:"temps"`
	MemoireKio uint32 `json:"memoire_kio"`
	Fils       uint8  `json:"fils"`
	Sel        string `json:"sel"`
	Nonce      string `json:"nonce"`
	Chiffre    string `json:"chiffre"`
	Publique   string `json:"publique"` // base64, à coller dans l'interface (Mises à jour des agents)
}

// ErrPhrase : phrase de passe fausse ou fichier modifié (impossible de
// distinguer les deux, c'est voulu).
var ErrPhrase = errors.New("phrase de passe incorrecte ou fichier de clé modifié")

// NouvelleCle crée une paire de clés et renvoie le fichier chiffré.
func NouvelleCle(phrase []byte) (*FichierCle, error) {
	if len([]rune(string(phrase))) < LongueurMinPhrase {
		return nil, fmt.Errorf("phrase de passe trop courte (%d caractères minimum)", LongueurMinPhrase)
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	graine := priv.Seed()
	defer effacer(graine)
	defer effacer(priv)
	f := &FichierCle{Format: FormatCle, KDF: "argon2id", Temps: argonTemps, MemoireKio: argonMemoire, Fils: argonFils,
		Publique: base64.StdEncoding.EncodeToString(pub)}
	sel := make([]byte, 16)
	nonce := make([]byte, 12)
	if _, err := rand.Read(sel); err != nil {
		return nil, err
	}
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	aead, err := chiffreur(phrase, sel, f.Temps, f.MemoireKio, f.Fils)
	if err != nil {
		return nil, err
	}
	f.Sel = base64.StdEncoding.EncodeToString(sel)
	f.Nonce = base64.StdEncoding.EncodeToString(nonce)
	f.Chiffre = base64.StdEncoding.EncodeToString(aead.Seal(nil, nonce, graine, f.donneesAssociees()))
	return f, nil
}

// donneesAssociees : la clé publique et les paramètres sont liés au chiffré :
// les changer dans le fichier fait échouer le déchiffrement.
func (f *FichierCle) donneesAssociees() []byte {
	return fmt.Appendf(nil, "%s|%s|%d|%d|%d|%s", f.Format, f.KDF, f.Temps, f.MemoireKio, f.Fils, f.Publique)
}

func chiffreur(phrase, sel []byte, temps, memoire uint32, fils uint8) (cipher.AEAD, error) {
	cle := argon2.IDKey(phrase, sel, temps, memoire, fils, 32)
	defer effacer(cle)
	bloc, err := aes.NewCipher(cle)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(bloc)
}

// LireCle déchiffre le fichier. La clé privée renvoyée doit être effacée par
// l'appelant (Effacer) dès la signature faite.
func LireCle(brut, phrase []byte) (ed25519.PrivateKey, error) {
	var f FichierCle
	dec := json.NewDecoder(bytes.NewReader(brut))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("fichier de clé illisible : %w", err)
	}
	if f.Format != FormatCle || f.KDF != "argon2id" {
		return nil, errors.New("format de fichier de clé inconnu")
	}
	// Bornes : un fichier modifié ne doit pas faire consommer 100 Go de mémoire.
	if f.Temps < 1 || f.Temps > 20 || f.MemoireKio < 64*1024 || f.MemoireKio > 4*1024*1024 || f.Fils < 1 || f.Fils > 16 {
		return nil, errors.New("paramètres Argon2id hors bornes")
	}
	sel, err1 := base64.StdEncoding.DecodeString(f.Sel)
	nonce, err2 := base64.StdEncoding.DecodeString(f.Nonce)
	chiffre, err3 := base64.StdEncoding.DecodeString(f.Chiffre)
	pub, err4 := base64.StdEncoding.DecodeString(f.Publique)
	if err := errors.Join(err1, err2, err3, err4); err != nil || len(sel) != 16 || len(nonce) != 12 || len(pub) != ed25519.PublicKeySize {
		return nil, errors.New("fichier de clé abîmé")
	}
	aead, err := chiffreur(phrase, sel, f.Temps, f.MemoireKio, f.Fils)
	if err != nil {
		return nil, err
	}
	graine, err := aead.Open(nil, nonce, chiffre, f.donneesAssociees())
	if err != nil || len(graine) != ed25519.SeedSize {
		return nil, ErrPhrase
	}
	defer effacer(graine)
	priv := ed25519.NewKeyFromSeed(graine)
	if !bytes.Equal(priv.Public().(ed25519.PublicKey), pub) {
		Effacer(priv)
		return nil, ErrPhrase
	}
	return priv, nil
}

// Effacer met la clé privée à zéro (au mieux : Go peut en avoir copié des
// morceaux ailleurs en mémoire).
func Effacer(priv ed25519.PrivateKey) { effacer(priv) }

func effacer(b []byte) {
	for i := range b {
		b[i] = 0
	}
}

var formatVersion = regexp.MustCompile(`^[0-9]{1,5}\.[0-9]{1,5}\.[0-9]{1,5}$`)

// machinesELF : architecture annoncée → type de machine du binaire Linux.
var machinesELF = map[string]elf.Machine{"amd64": elf.EM_X86_64, "arm64": elf.EM_AARCH64}

// TailleMax : même limite que l'agent.
const TailleMax = 64 << 20

// Signer fabrique le manifeste signé d'un binaire. Le binaire doit être un
// exécutable Linux de l'architecture annoncée (évite de signer le mauvais
// fichier).
func Signer(priv ed25519.PrivateKey, binaire []byte, version, arch string, retourAutorise bool) (*protocole.Manifeste, error) {
	if !formatVersion.MatchString(version) {
		return nil, fmt.Errorf("version %q invalide (attendu 1.2.3)", version)
	}
	machine, ok := machinesELF[arch]
	if !ok {
		return nil, fmt.Errorf("architecture %q inconnue (amd64 ou arm64)", arch)
	}
	if len(binaire) == 0 || len(binaire) > TailleMax {
		return nil, errors.New("binaire vide ou trop gros")
	}
	e, err := elf.NewFile(bytes.NewReader(binaire))
	if err != nil {
		return nil, errors.New("ce fichier n'est pas un exécutable Linux")
	}
	if e.Machine != machine || e.Type != elf.ET_EXEC && e.Type != elf.ET_DYN {
		return nil, fmt.Errorf("exécutable pour %s, pas pour %s", e.Machine, arch)
	}
	h := sha256.Sum256(binaire)
	m := &protocole.Manifeste{Version: version, Architecture: arch, SHA256: hex.EncodeToString(h[:]),
		Taille: int64(len(binaire)), RetourAutorise: retourAutorise}
	m.Signature = base64.StdEncoding.EncodeToString(ed25519.Sign(priv, m.MessageSigne()))
	return m, nil
}

// Verifier contrôle un manifeste et son binaire avec une clé publique.
func Verifier(m *protocole.Manifeste, binaire []byte, publique string) error {
	pub, err := base64.StdEncoding.DecodeString(publique)
	if err != nil || len(pub) != ed25519.PublicKeySize {
		return errors.New("clé publique invalide")
	}
	sig, err := base64.StdEncoding.DecodeString(m.Signature)
	if err != nil || !ed25519.Verify(pub, m.MessageSigne(), sig) {
		return errors.New("signature invalide")
	}
	h := sha256.Sum256(binaire)
	if hex.EncodeToString(h[:]) != m.SHA256 || int64(len(binaire)) != m.Taille {
		return errors.New("le binaire ne correspond pas au manifeste")
	}
	return nil
}

// EmpreintePublique : 16 premiers caractères hexadécimaux du SHA-256 de la
// clé publique (octets bruts) ; même calcul que l'interface et le script
// d'installation, pour comparer d'un coup d'œil.
func EmpreintePublique(b64 string) string {
	k, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return "?"
	}
	h := sha256.Sum256(k)
	return hex.EncodeToString(h[:])[:16]
}
