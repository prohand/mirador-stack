// SPDX-License-Identifier: GPL-3.0-or-later

// mirador-release : signe les versions de mirador-agent, sur le poste de
// l'utilisateur (Windows). Jamais sur le serveur.
//
// Utilisation :
//
//	mirador-release cle [-sortie cle-mirador.json]
//	    crée la clé de signature, chiffrée par une phrase de passe ;
//	    affiche la clé PUBLIQUE à coller dans l'interface (Mises à jour des agents)
//	mirador-release signer -cle cle-mirador.json -version 1.2.3 -arch amd64 [-retour-autorise] BINAIRE
//	    écrit BINAIRE.manifeste.json (à publier avec le binaire sur le serveur)
//	mirador-release verifier -publique CLE_PUBLIQUE MANIFESTE BINAIRE
//	mirador-release -version
//
// La phrase de passe est toujours demandée au clavier (jamais en option, ni
// dans un fichier, ni dans une variable d'environnement).
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"golang.org/x/term"

	"mirador-stack/internal/protocole"
	"mirador-stack/internal/release"
	"mirador-stack/internal/version"
)

func main() {
	os.Exit(lancer(os.Args[1:], os.Stdout, os.Stderr, lirePhraseClavier))
}

// lirePhrase : remplaçable dans les tests.
type lirePhrase func(invite string) ([]byte, error)

func lirePhraseClavier(invite string) ([]byte, error) {
	fd := int(os.Stdin.Fd()) // #nosec G115 -- descripteur de l'entrée standard
	if !term.IsTerminal(fd) {
		return nil, errors.New("la phrase de passe doit être tapée au clavier (pas de redirection)")
	}
	fmt.Fprint(os.Stderr, invite)
	p, err := term.ReadPassword(fd)
	fmt.Fprintln(os.Stderr)
	return p, err
}

const aide = `mirador-release : signature des versions de mirador-agent

  cle [-sortie FICHIER]                                   créer la clé de signature
  signer -cle FICHIER -version X.Y.Z -arch amd64|arm64 [-retour-autorise] BINAIRE
  verifier -publique CLE MANIFESTE BINAIRE
  -version`

func lancer(args []string, sortie, erreurs io.Writer, phrase lirePhrase) int {
	if len(args) == 0 {
		fmt.Fprintln(sortie, aide)
		return 0
	}
	if args[0] == "-version" || args[0] == "--version" {
		fmt.Fprintln(sortie, "mirador-release", version.Complete())
		return 0
	}
	fs := flag.NewFlagSet(args[0], flag.ContinueOnError)
	fs.SetOutput(erreurs)
	sortieCle := fs.String("sortie", "cle-mirador.json", "fichier de clé à créer")
	fichierCle := fs.String("cle", "cle-mirador.json", "fichier de clé")
	ver := fs.String("version", "", "version (1.2.3)")
	arch := fs.String("arch", "amd64", "architecture (amd64, arm64)")
	retour := fs.Bool("retour-autorise", false, "autoriser l'installation par-dessus une version plus récente (retour arrière)")
	publique := fs.String("publique", "", "clé publique (base64)")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	var err error
	switch args[0] {
	case "cle":
		err = creerCle(*sortieCle, sortie, phrase)
	case "signer":
		if fs.NArg() != 1 {
			err = errors.New("un binaire attendu")
			break
		}
		err = signer(*fichierCle, fs.Arg(0), *ver, *arch, *retour, sortie, phrase)
	case "verifier":
		if fs.NArg() != 2 {
			err = errors.New("un manifeste et un binaire attendus")
			break
		}
		err = verifier(fs.Arg(0), fs.Arg(1), *publique, sortie)
	default:
		err = fmt.Errorf("commande inconnue %q\n%s", args[0], aide)
	}
	if err != nil {
		fmt.Fprintf(erreurs, "erreur : %v\n", err)
		return 1
	}
	return 0
}

func creerCle(chemin string, sortie io.Writer, phrase lirePhrase) error {
	if _, err := os.Stat(chemin); err == nil { // #nosec G703 -- chemin tapé par l'utilisateur sur son propre poste
		return fmt.Errorf("%s existe déjà : jamais écrasé (une clé perdue ne se retrouve pas)", chemin)
	}
	p1, err := phrase("Phrase de passe (12 caractères minimum) : ")
	if err != nil {
		return err
	}
	p2, err := phrase("La même, encore une fois : ")
	if err != nil {
		return err
	}
	if !bytes.Equal(p1, p2) {
		return errors.New("les deux phrases sont différentes")
	}
	f, err := release.NouvelleCle(p1)
	if err != nil {
		return err
	}
	brut, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	// O_EXCL : jamais d'écrasement, même en cas de course.
	fd, err := os.OpenFile(chemin, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) // #nosec G304 G703 -- chemin choisi par l'utilisateur
	if err != nil {
		return err
	}
	if _, err := fd.Write(append(brut, '\n')); err != nil {
		return errors.Join(err, fd.Close())
	}
	if err := fd.Close(); err != nil {
		return err
	}
	fmt.Fprintf(sortie, "Clé créée : %s (chiffrée ; gardez-en une copie hors de ce PC, avec la phrase de passe à part).\n", chemin)
	fmt.Fprintf(sortie, "Clé PUBLIQUE (pas un secret), à coller dans l'interface : Mises à jour des agents › Clé publique :\n%s\n", f.Publique)
	fmt.Fprintf(sortie, "Empreinte : %s (la même s'affiche dans l'interface et à l'installation de l'agent)\n", release.EmpreintePublique(f.Publique))
	return nil
}

func signer(cheminCle, cheminBinaire, ver, arch string, retour bool, sortie io.Writer, phrase lirePhrase) error {
	brutCle, err := os.ReadFile(cheminCle) // #nosec G304 G703 -- chemin choisi par l'utilisateur
	if err != nil {
		return err
	}
	binaire, err := os.ReadFile(cheminBinaire) // #nosec G304 G703 -- chemin choisi par l'utilisateur
	if err != nil {
		return err
	}
	p, err := phrase("Phrase de passe de la clé : ")
	if err != nil {
		return err
	}
	priv, err := release.LireCle(brutCle, p)
	if err != nil {
		return err
	}
	m, err := release.Signer(priv, binaire, ver, arch, retour)
	release.Effacer(priv)
	if err != nil {
		return err
	}
	brut, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	chemin := cheminBinaire + ".manifeste.json"
	if err := os.WriteFile(chemin, append(brut, '\n'), 0o644); err != nil { // #nosec G306 G703 -- manifeste public
		return err
	}
	fmt.Fprintf(sortie, "Manifeste écrit : %s\n  version %s, %s, %d octets, SHA-256 %s", chemin, m.Version, m.Architecture, m.Taille, m.SHA256)
	if m.RetourAutorise {
		fmt.Fprint(sortie, "\n  RETOUR ARRIÈRE AUTORISÉ : les agents plus récents l'accepteront aussi")
	}
	fmt.Fprintln(sortie)
	return nil
}

func verifier(cheminManifeste, cheminBinaire, publique string, sortie io.Writer) error {
	brut, err := os.ReadFile(cheminManifeste) // #nosec G304 G703 -- chemin choisi par l'utilisateur
	if err != nil {
		return err
	}
	var m protocole.Manifeste
	dec := json.NewDecoder(bytes.NewReader(brut))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return fmt.Errorf("manifeste illisible : %w", err)
	}
	binaire, err := os.ReadFile(cheminBinaire) // #nosec G304 G703 -- chemin choisi par l'utilisateur
	if err != nil {
		return err
	}
	if err := release.Verifier(&m, binaire, publique); err != nil {
		return err
	}
	fmt.Fprintf(sortie, "OK : version %s (%s) signée par cette clé.\n", m.Version, m.Architecture)
	return nil
}
