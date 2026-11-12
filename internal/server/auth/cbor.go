// SPDX-License-Identifier: GPL-3.0-or-later

package auth

import (
	"encoding/binary"
	"errors"
)

// Petit lecteur CBOR (RFC 8949), limité à ce qu'envoie un authentificateur
// WebAuthn : entiers, octets, texte, tableaux, tables, booléens et nul, avec
// des longueurs définies. Tout le reste est refusé. Écrit ici pour ne pas
// ajouter de dépendance (SPEC.md § 20) ; borné en profondeur et en taille.

var errCBOR = errors.New("CBOR : donnée illisible ou non prise en charge")

const (
	cborProfondeurMax = 6
	cborElementsMax   = 64
)

// lireCBOR lit une valeur et renvoie ce qui reste après elle. Types rendus :
// int64, []byte, string, []any, map[any]any (clés int64 ou string), bool, nil.
func lireCBOR(b []byte, profondeur int) (any, []byte, error) {
	if profondeur > cborProfondeurMax || len(b) == 0 {
		return nil, nil, errCBOR
	}
	majeur, info := b[0]>>5, b[0]&31
	b = b[1:]
	// Argument : valeur directe (0 à 23) ou sur 1, 2, 4 ou 8 octets.
	var n uint64
	switch {
	case info < 24:
		n = uint64(info)
	case info == 24 && len(b) >= 1:
		n, b = uint64(b[0]), b[1:]
	case info == 25 && len(b) >= 2:
		n, b = uint64(binary.BigEndian.Uint16(b)), b[2:]
	case info == 26 && len(b) >= 4:
		n, b = uint64(binary.BigEndian.Uint32(b)), b[4:]
	case info == 27 && len(b) >= 8:
		n, b = binary.BigEndian.Uint64(b), b[8:]
	default:
		return nil, nil, errCBOR // longueur indéfinie ou donnée tronquée
	}
	switch majeur {
	case 0: // entier positif
		if n > 1<<62 {
			return nil, nil, errCBOR
		}
		return int64(n), b, nil
	case 1: // entier négatif : -1 - n
		if n > 1<<62 {
			return nil, nil, errCBOR
		}
		return -1 - int64(n), b, nil
	case 2, 3: // octets, texte
		if n > uint64(len(b)) {
			return nil, nil, errCBOR
		}
		if majeur == 2 {
			return b[:n:n], b[n:], nil
		}
		return string(b[:n]), b[n:], nil
	case 4: // tableau
		if n > cborElementsMax {
			return nil, nil, errCBOR
		}
		out := make([]any, 0, n)
		for i := uint64(0); i < n; i++ {
			v, reste, err := lireCBOR(b, profondeur+1)
			if err != nil {
				return nil, nil, err
			}
			out, b = append(out, v), reste
		}
		return out, b, nil
	case 5: // table
		if n > cborElementsMax {
			return nil, nil, errCBOR
		}
		out := make(map[any]any, n)
		for i := uint64(0); i < n; i++ {
			k, reste, err := lireCBOR(b, profondeur+1)
			if err != nil {
				return nil, nil, err
			}
			switch k.(type) {
			case int64, string:
			default:
				return nil, nil, errCBOR
			}
			v, reste, err := lireCBOR(reste, profondeur+1)
			if err != nil {
				return nil, nil, err
			}
			if _, double := out[k]; double {
				return nil, nil, errCBOR
			}
			out[k], b = v, reste
		}
		return out, b, nil
	case 7: // faux, vrai, nul (pas de nombre à virgule)
		switch info {
		case 20:
			return false, b, nil
		case 21:
			return true, b, nil
		case 22:
			return nil, b, nil
		}
	}
	return nil, nil, errCBOR
}
