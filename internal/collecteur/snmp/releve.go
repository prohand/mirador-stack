// SPDX-License-Identifier: GPL-3.0-or-later

package snmp

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"math/big"
	"net"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gosnmp/gosnmp"

	"mirador-stack/internal/agent/config"
	"mirador-stack/internal/agent/metrics"
)

// Source : lecture SNMP (réseau en vrai, données simulées dans les tests).
type Source interface {
	Get(oids []string) (*gosnmp.SnmpPacket, error)
	BulkWalkAll(racine string) ([]gosnmp.SnmpPDU, error)
}

// Bornes d'un relevé.
const (
	maxOIDParGet   = 30
	maxLignesTable = 2000 // lignes par table (protection mémoire)
	maxTexte       = 128
)

// Releve lit les OID du préréglage et ajoute les métriques au lot.
func Releve(src Source, nomPrereglage string, b *metrics.Batch) error {
	p, ok := prereglages[nomPrereglage]
	if !ok {
		return fmt.Errorf("préréglage %q inconnu", nomPrereglage)
	}
	// Scalaires : GET par paquets de 30.
	trouves := 0
	for i := 0; i < len(p.scalaires); i += maxOIDParGet {
		lot := p.scalaires[i:min(i+maxOIDParGet, len(p.scalaires))]
		oids := make([]string, len(lot))
		parOID := map[string]scalaire{}
		for j, s := range lot {
			oids[j] = s.oid
			parOID[s.oid] = s
		}
		pkt, err := src.Get(oids)
		if err != nil {
			return err // pas de réponse (adresse, utilisateur, mots de passe…)
		}
		for _, pdu := range pkt.Variables {
			s, ok := parOID[strings.TrimPrefix(pdu.Name, ".")]
			if !ok {
				continue
			}
			if emettre(b, s.nom, s.texte, pdu, nil) {
				trouves++
			}
		}
	}
	// Tables : GETBULK sur chaque entrée ; colonnes d'étiquettes gardées.
	marches := map[string][]gosnmp.SnmpPDU{}
	marche := func(racine string) []gosnmp.SnmpPDU {
		if v, ok := marches[racine]; ok {
			return v
		}
		v, _ := src.BulkWalkAll(racine) // table absente : rien
		if len(v) > maxLignesTable*16 {
			v = v[:maxLignesTable*16]
		}
		marches[racine] = v
		return v
	}
	for _, t := range p.tables {
		// Étiquette de chaque index (lookup).
		etiq := map[string]string{}
		for _, pdu := range marche(t.colEtiq) {
			if idx, ok := strings.CutPrefix(strings.TrimPrefix(pdu.Name, "."), t.colEtiq+"."); ok {
				etiq[idx] = texte(pdu.Value)
			}
		}
		cols := map[string]colonne{}
		for _, c := range t.colonnes {
			cols[c.sous] = c
		}
		lignes := map[string]bool{}
		for _, pdu := range marche(t.entree) {
			reste, ok := strings.CutPrefix(strings.TrimPrefix(pdu.Name, "."), t.entree+".")
			if !ok {
				continue
			}
			col, idx, ok := strings.Cut(reste, ".")
			c, connu := cols[col]
			if !ok || !connu {
				continue
			}
			if !lignes[idx] && len(lignes) >= maxLignesTable {
				continue
			}
			lignes[idx] = true
			valeurEtiq := etiq[idx]
			if valeurEtiq == "" {
				valeurEtiq = idx
			}
			labels := []metrics.Label{metrics.L(t.etiquette, valeurEtiq)}
			if t.garderIndex != "" {
				labels = append(labels, metrics.L(t.garderIndex, idx))
			}
			if emettre(b, c.nom, c.texte, pdu, labels) {
				trouves++
			}
		}
	}
	if trouves == 0 {
		return errors.New("aucune valeur reçue (OID absents : préréglage ou droits de l'utilisateur SNMP ?)")
	}
	return nil
}

// emettre : une valeur SNMP en métrique (nombre, ou « info » pour un texte).
func emettre(b *metrics.Batch, nom string, estTexte bool, pdu gosnmp.SnmpPDU, labels []metrics.Label) bool {
	switch pdu.Type {
	case gosnmp.NoSuchObject, gosnmp.NoSuchInstance, gosnmp.EndOfMibView, gosnmp.Null:
		return false
	}
	if estTexte {
		t := texte(pdu.Value)
		if t == "" {
			return false
		}
		b.Add(nom, 1, append(labels, metrics.L(nom, t))...)
		return true
	}
	switch pdu.Type {
	case gosnmp.Integer, gosnmp.Counter32, gosnmp.Gauge32, gosnmp.TimeTicks, gosnmp.Counter64, gosnmp.Uinteger32:
	default:
		return false
	}
	v, _ := new(big.Float).SetInt(gosnmp.ToBigInt(pdu.Value)).Float64()
	if math.IsInf(v, 0) || math.IsNaN(v) {
		return false
	}
	b.Add(nom, v, labels...)
	return true
}

// texte : chaîne SNMP lisible (UTF-8 valide, caractères de contrôle retirés,
// 128 caractères au plus).
func texte(v any) string {
	var brut []byte
	switch x := v.(type) {
	case []byte:
		brut = x
	case string:
		brut = []byte(x)
	default:
		return ""
	}
	brut = bytes.ToValidUTF8(bytes.TrimRight(brut, "\x00"), []byte("?"))
	s := strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, strings.TrimSpace(string(brut)))
	for len(s) > maxTexte {
		_, n := utf8.DecodeLastRuneInString(s)
		s = s[:len(s)-n]
	}
	return s
}

// --- Relevé périodique d'une cible ---------------------------------------------------

// Releveur : relève une cible à son rythme et passe chaque lot à Envoyer.
type Releveur struct {
	Cible   config.CibleSNMP
	Envoyer func(*metrics.Batch)
	Journal *slog.Logger
	// Ouvrir : source SNMP (réseau par défaut ; remplaçable dans les tests).
	Ouvrir func(config.CibleSNMP) (Source, func(), error)
}

// Tourner : un relevé tout de suite, puis à chaque intervalle.
func (r *Releveur) Tourner(ctx context.Context) {
	intervalle := time.Duration(r.Cible.Intervalle)
	if intervalle == 0 {
		intervalle = config.IntervalleSNMPDefaut
	}
	ouvrir := r.Ouvrir
	if ouvrir == nil {
		ouvrir = OuvrirReseau
	}
	t := time.NewTicker(intervalle)
	defer t.Stop()
	dernierEchec := ""
	for {
		debut := time.Now()
		b := metrics.NewBatch(256, []metrics.Label{metrics.L("instance", r.Cible.Machine), metrics.L("job", "snmp")})
		b.Reset(debut.UnixMilli())
		err := func() error {
			src, fermer, err := ouvrir(r.Cible)
			if err != nil {
				return err
			}
			defer fermer()
			return Releve(src, r.Cible.Prereglage, b)
		}()
		up := 1.0
		if err != nil {
			up = 0
			if err.Error() != dernierEchec && r.Journal != nil {
				r.Journal.Warn("SNMP : relevé en échec", "equipement", r.Cible.Machine, "erreur", err)
			}
			dernierEchec = err.Error()
		} else {
			dernierEchec = ""
		}
		b.Add("up", up)
		b.Add("snmp_scrape_duration_seconds", time.Since(debut).Seconds())
		r.Envoyer(b)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// OuvrirReseau : session SNMP v3 authPriv vers la cible (UDP, 5 s, 1 essai
// de plus). Un secret encore chiffré (le serveur n'a pas pu le déchiffrer)
// est refusé.
func OuvrirReseau(c config.CibleSNMP) (Source, func(), error) {
	if strings.HasPrefix(c.MotDePasse, config.PrefixeSecretChiffre) || strings.HasPrefix(c.CleChiffre, config.PrefixeSecretChiffre) {
		return nil, nil, errors.New("mots de passe SNMP non déchiffrés par le serveur")
	}
	hote, port, err := net.SplitHostPort(c.Adresse)
	if err != nil {
		hote, port = c.Adresse, "161"
	}
	n, err := strconv.ParseUint(port, 10, 16)
	if err != nil {
		return nil, nil, err
	}
	auth := map[string]gosnmp.SnmpV3AuthProtocol{"SHA": gosnmp.SHA, "SHA256": gosnmp.SHA256, "SHA512": gosnmp.SHA512}[c.Auth]
	priv := map[string]gosnmp.SnmpV3PrivProtocol{"AES": gosnmp.AES, "AES256": gosnmp.AES256}[c.Chiffrement]
	g := &gosnmp.GoSNMP{Target: hote, Port: uint16(n), Version: gosnmp.Version3, Timeout: 5 * time.Second, Retries: 1,
		MaxRepetitions: 25, SecurityModel: gosnmp.UserSecurityModel, MsgFlags: gosnmp.AuthPriv,
		SecurityParameters: &gosnmp.UsmSecurityParameters{UserName: c.Utilisateur, AuthenticationProtocol: auth,
			AuthenticationPassphrase: c.MotDePasse, PrivacyProtocol: priv, PrivacyPassphrase: c.CleChiffre}}
	if err := g.Connect(); err != nil {
		return nil, nil, err
	}
	return g, func() { _ = g.Conn.Close() }, nil
}
