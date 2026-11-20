// SPDX-License-Identifier: GPL-3.0-or-later

// Package envoi gère la file d'attente des lots et leur envoi en HTTPS.
//
// Règles (SPEC.md § 6.3, § 100) :
//   - la file est en mémoire et bornée en octets ;
//   - pleine, elle écrit ses lots les plus anciens dans le tampon disque
//     (s'il est activé, borné lui aussi) ; sans lui, ou au-delà, elle les
//     jette (et les compte) ;
//   - rien n'est écrit sur disque tant que les envois réussissent.
package envoi

import "sync"

// File est une file FIFO de lots (octets déjà compressés), bornée en taille.
type File struct {
	mu     sync.Mutex
	lots   [][]byte
	taille int // somme des longueurs des lots en attente
	max    int
	jetes  uint64 // lots jetés depuis le démarrage
	signal chan struct{}
	libres [][]byte // tampons rendus après envoi, réutilisés par Prendre
	// Tampon disque (SPEC § 100) : nil = désactivé.
	disque       *Debord
	depuisDisque []byte // lot du disque rendu par Premier (à retirer après envoi)
}

// AvecDisque : les lots qui débordent de la mémoire vont sur disque. Les
// lots déjà sur disque (avant un redémarrage) partiront en premier.
func (f *File) AvecDisque(d *Debord) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.disque = d
	if d != nil && len(d.fichiers) > 0 {
		select {
		case f.signal <- struct{}{}:
		default:
		}
	}
}

// Sauver : à l'arrêt de l'agent, les lots encore en mémoire (serveur
// injoignable) vont sur disque ; ils partiront au prochain démarrage.
func (f *File) Sauver() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.disque == nil {
		return
	}
	for _, l := range f.lots {
		n, _ := f.disque.ecrire(l)
		f.jetes += uint64(n)
	}
	f.lots, f.taille = nil, 0
}

// OctetsDisque : octets en attente sur disque (0 sans tampon disque).
func (f *File) OctetsDisque() int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.disque == nil {
		return 0
	}
	return f.disque.Octets()
}

// NewFile crée une file bornée à max octets.
func NewFile(max int) *File {
	return &File{max: max, signal: make(chan struct{}, 1)}
}

// Ajouter met un lot en fin de file. Si la place manque, les plus anciens sont
// jetés. Un lot plus gros que toute la file est refusé (renvoie false).
func (f *File) Ajouter(lot []byte) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(lot) > f.max {
		f.jetes++
		return false
	}
	for f.taille+len(lot) > f.max && len(f.lots) > 0 {
		f.taille -= len(f.lots[0])
		if f.disque != nil {
			// Débordement sur disque (SPEC § 100) ; jeté si le disque refuse.
			n, _ := f.disque.ecrire(f.lots[0])
			f.jetes += uint64(n)
		} else {
			f.jetes++
		}
		// Pas de recyclage ici : l'expéditeur est peut-être en train d'envoyer
		// ce lot. Le ramasse-miettes le libérera après.
		f.lots[0] = nil
		f.lots = f.lots[1:]
	}
	f.lots = append(f.lots, lot)
	f.taille += len(lot)
	select {
	case f.signal <- struct{}{}:
	default:
	}
	return true
}

// Premier renvoie le plus ancien lot sans le retirer (nil si la file est
// vide) : d'abord ceux du disque (plus anciens que ceux en mémoire).
func (f *File) Premier() []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.disque != nil {
		if b := f.disque.premier(); b != nil {
			f.depuisDisque = b
			return b
		}
	}
	if len(f.lots) == 0 {
		return nil
	}
	return f.lots[0]
}

// Retirer enlève le plus ancien lot s'il s'agit toujours de lot (il a pu être
// jeté entre-temps pour faire de la place).
func (f *File) Retirer(lot []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(lot) > 0 && len(f.depuisDisque) > 0 && &f.depuisDisque[0] == &lot[0] {
		f.disque.retirerPremier()
		f.depuisDisque = nil
		return
	}
	if len(lot) == 0 || len(f.lots) == 0 || len(f.lots[0]) == 0 || &f.lots[0][0] != &lot[0] {
		return
	}
	f.taille -= len(lot)
	f.recycler(lot)
	f.lots[0] = nil
	f.lots = f.lots[1:]
}

// Signal se déclenche quand un lot est ajouté.
func (f *File) Signal() <-chan struct{} { return f.signal }

// Etat renvoie le nombre de lots, les octets en attente et les lots jetés.
func (f *File) Etat() (lots, octets int, jetes uint64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.lots), f.taille, f.jetes
}

// Prendre renvoie un tampon libre à remplir (évite une allocation par cycle).
func (f *File) Prendre() []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	if n := len(f.libres); n > 0 {
		b := f.libres[n-1]
		f.libres = f.libres[:n-1]
		return b[:0]
	}
	return nil
}

// recycler garde au plus 4 tampons libres. Appelé avec le verrou pris.
func (f *File) recycler(b []byte) {
	if len(f.libres) < 4 {
		f.libres = append(f.libres, b[:0])
	}
}
