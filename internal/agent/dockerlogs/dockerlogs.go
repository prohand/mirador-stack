// SPDX-License-Identifier: GPL-3.0-or-later

// Package dockerlogs suit les logs des conteneurs Docker par le proxy de
// socket en lecture seule (SPEC.md § 6.5), comme loki.source.docker d'Alloy.
//
// Fonctionnement :
//   - toutes les 30 s, la liste des conteneurs en marche est relue ;
//   - un flux « logs?follow=1 » est ouvert par conteneur (une goroutine
//     chacun, petit tampon) ;
//   - un flux coupé (redémarrage du conteneur, coupure du proxy) est rouvert
//     à partir de la dernière ligne reçue, sans doublon ;
//   - au premier passage, on part de « maintenant » : pas de relecture du passé.
//
// Rien n'est écrit sur le disque. Seules des requêtes GET sont envoyées.
package dockerlogs

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Traiter reçoit chaque ligne : heure, message, conteneur, flux (stdout/stderr).
type Traiter func(temps time.Time, msg, conteneur, flux string)

// Suiveur suit les logs de tous les conteneurs en marche.
type Suiveur struct {
	base       string
	client     *http.Client
	traiter    Traiter
	journal    *slog.Logger
	periode    time.Duration
	maintenant func() time.Time

	mu     sync.Mutex
	actifs map[string]context.CancelFunc // id → arrêt du flux
	depuis map[string]time.Time          // id → heure de la dernière ligne lue
}

// NewSuiveur crée le suiveur. adresse : comme le collecteur Docker
// (http://127.0.0.1:PORT ou unix:///chemin), déjà validée.
func NewSuiveur(adresse string, traiter Traiter, j *slog.Logger) (*Suiveur, error) {
	u, err := url.Parse(adresse)
	if err != nil {
		return nil, err
	}
	transport := &http.Transport{
		MaxIdleConnsPerHost:   4,
		ResponseHeaderTimeout: 15 * time.Second,
		IdleConnTimeout:       time.Minute,
	}
	s := &Suiveur{traiter: traiter, journal: j, periode: 30 * time.Second, maintenant: time.Now,
		actifs: map[string]context.CancelFunc{}, depuis: map[string]time.Time{}}
	switch u.Scheme {
	case "http":
		s.base = strings.TrimRight(adresse, "/")
	case "unix":
		chemin := u.Path
		transport.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", chemin)
		}
		s.base = "http://docker"
	default:
		return nil, fmt.Errorf("adresse Docker %q : http:// ou unix:// attendu", adresse)
	}
	// Pas de délai global : un flux de logs dure tant que le conteneur tourne.
	s.client = &http.Client{Transport: transport}
	return s, nil
}

// Tourner suit les conteneurs jusqu'à l'arrêt du contexte.
func (s *Suiveur) Tourner(ctx context.Context) {
	t := time.NewTicker(s.periode)
	defer t.Stop()
	for {
		if err := s.synchroniser(ctx); err != nil {
			s.journal.Debug("liste des conteneurs indisponible", "erreur", err)
		}
		select {
		case <-ctx.Done():
			s.toutArreter()
			return
		case <-t.C:
		}
	}
}

// synchroniser ouvre un flux pour chaque nouveau conteneur et ferme ceux des
// conteneurs disparus.
func (s *Suiveur) synchroniser(ctx context.Context) error {
	var liste []struct {
		ID    string   `json:"Id"`
		Names []string `json:"Names"`
	}
	ctxListe, annuler := context.WithTimeout(ctx, 10*time.Second)
	defer annuler()
	if err := s.get(ctxListe, "/containers/json", &liste); err != nil {
		return err
	}
	vus := make(map[string]bool, len(liste))
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range liste {
		if !idValide(c.ID) {
			continue
		}
		vus[c.ID] = true
		if _, actif := s.actifs[c.ID]; actif {
			continue
		}
		nom := c.ID[:12]
		if len(c.Names) > 0 {
			nom = strings.TrimPrefix(c.Names[0], "/")
		}
		if _, connu := s.depuis[c.ID]; !connu {
			s.depuis[c.ID] = s.maintenant() // pas de relecture du passé
		}
		ctxFlux, arret := context.WithCancel(ctx)
		s.actifs[c.ID] = arret
		go s.suivre(ctxFlux, c.ID, nom)
	}
	for id, arret := range s.actifs {
		if !vus[id] {
			arret()
			delete(s.actifs, id)
			delete(s.depuis, id)
		}
	}
	return nil
}

// Un identifiant Docker : 64 caractères hexadécimaux.
func idValide(id string) bool {
	if len(id) != 64 {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func (s *Suiveur) toutArreter() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, arret := range s.actifs {
		arret()
		delete(s.actifs, id)
	}
}

// suivre lit le flux d'un conteneur et le rouvre s'il se coupe.
func (s *Suiveur) suivre(ctx context.Context, id, nom string) {
	defer func() {
		s.mu.Lock()
		if arret := s.actifs[id]; arret != nil && ctx.Err() == nil {
			// Le flux s'est arrêté seul (conteneur arrêté, proxy coupé) : la
			// prochaine synchronisation le rouvrira à partir de la dernière ligne.
			arret()
			delete(s.actifs, id)
		}
		s.mu.Unlock()
	}()
	var detail struct {
		Config struct {
			Tty bool `json:"Tty"`
		} `json:"Config"`
	}
	ctxDet, annuler := context.WithTimeout(ctx, 10*time.Second)
	err := s.get(ctxDet, "/containers/"+id+"/json", &detail)
	annuler()
	if err != nil {
		return
	}
	if err := s.lireFlux(ctx, id, nom, detail.Config.Tty); err != nil && ctx.Err() == nil {
		s.journal.Debug("flux de logs coupé", "conteneur", nom, "erreur", err)
	}
}

// lireFlux ouvre « /logs?follow=1 » à partir de la dernière ligne lue.
func (s *Suiveur) lireFlux(ctx context.Context, id, nom string, tty bool) error {
	s.mu.Lock()
	depuis := s.depuis[id]
	s.mu.Unlock()
	q := url.Values{}
	q.Set("follow", "1")
	q.Set("stdout", "1")
	q.Set("stderr", "1")
	q.Set("timestamps", "1")
	q.Set("since", strconv.FormatFloat(float64(depuis.UnixNano())/1e9, 'f', 9, 64))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.base+"/containers/"+id+"/logs?"+q.Encode(), nil)
	if err != nil {
		return err
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	lire := bufio.NewReaderSize(resp.Body, 16<<10)
	ligne := func(brut []byte, flux string) {
		t, msg, ok := couperHorodatage(brut)
		if !ok {
			return
		}
		// Une ligne déjà vue (reprise après coupure) n'est pas renvoyée.
		s.mu.Lock()
		if !t.After(s.depuis[id]) {
			s.mu.Unlock()
			return
		}
		s.depuis[id] = t
		s.mu.Unlock()
		s.traiter(t, msg, nom, flux)
	}
	if tty {
		return lireBrut(lire, ligne)
	}
	return lireMultiplexe(lire, ligne)
}

// tailleMaxTrame : une trame plus grosse est jetée (protection mémoire).
const tailleMaxTrame = 1 << 20

// lireMultiplexe lit le format « multiplexé » de Docker (conteneur sans TTY) :
// en-tête de 8 octets [flux, 0, 0, 0, taille sur 4 octets], puis le contenu.
func lireMultiplexe(r *bufio.Reader, ligne func([]byte, string)) error {
	var entete [8]byte
	var contenu []byte
	for {
		if _, err := io.ReadFull(r, entete[:]); err != nil {
			return finFlux(err)
		}
		taille := binary.BigEndian.Uint32(entete[4:])
		flux := "stdout"
		if entete[0] == 2 {
			flux = "stderr"
		}
		if taille > tailleMaxTrame {
			if _, err := io.CopyN(io.Discard, r, int64(taille)); err != nil {
				return finFlux(err)
			}
			continue
		}
		if cap(contenu) < int(taille) {
			contenu = make([]byte, taille)
		}
		contenu = contenu[:taille]
		if _, err := io.ReadFull(r, contenu); err != nil {
			return finFlux(err)
		}
		for _, l := range strings.Split(strings.TrimRight(string(contenu), "\n"), "\n") {
			ligne([]byte(l), flux)
		}
	}
}

// lireBrut lit un flux de conteneur avec TTY : simples lignes (tout en stdout).
func lireBrut(r *bufio.Reader, ligne func([]byte, string)) error {
	for {
		l, err := r.ReadSlice('\n')
		if len(l) > 0 && err != bufio.ErrBufferFull {
			ligne([]byte(strings.TrimRight(string(l), "\r\n")), "stdout")
		}
		if err != nil && err != bufio.ErrBufferFull {
			return finFlux(err)
		}
	}
}

func finFlux(err error) error {
	if errors.Is(err, io.EOF) || errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

// couperHorodatage sépare « 2026-11-08T10:00:00.123456789Z message ».
func couperHorodatage(b []byte) (time.Time, string, bool) {
	i := strings.IndexByte(string(b), ' ')
	if i <= 0 {
		return time.Time{}, "", false
	}
	t, err := time.Parse(time.RFC3339Nano, string(b[:i]))
	if err != nil {
		return time.Time{}, "", false
	}
	return t, string(b[i+1:]), true
}

// tailleMaxReponse borne une réponse JSON de l'API.
const tailleMaxReponse = 4 << 20

func (s *Suiveur) get(ctx context.Context, chemin string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.base+chemin, nil)
	if err != nil {
		return err
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("API Docker %s : HTTP %d", chemin, resp.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, tailleMaxReponse)).Decode(v)
}

// Suivis renvoie le nombre de flux ouverts (métrique interne).
func (s *Suiveur) Suivis() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.actifs)
}
