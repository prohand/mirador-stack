// SPDX-License-Identifier: GPL-3.0-or-later

package syslog

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"mirador-stack/internal/agent/config"
)

// Ligne : un message reçu, prêt à envoyer. Hote est l'équipement de
// l'émetteur (liste des émetteurs), jamais ce que dit le message.
type Ligne struct {
	Hote, App, Niveau, Texte string
	Heure                    time.Time
}

// Options du serveur syslog.
type Options struct {
	Ecoute          string
	Emetteurs       []config.Emetteur
	GarderFilterlog bool
	Debit, Rafale   float64
	// Certificat : certificat TLS courant (rechargé sans couper les
	// connexions : appelé à chaque poignée de main).
	Certificat func(*tls.ClientHelloInfo) (*tls.Certificate, error)
	// Sortie reçoit chaque ligne ; false = file pleine (ligne perdue, comptée).
	Sortie     func(Ligne) bool
	Journal    *slog.Logger
	Maintenant func() time.Time
}

// Limites (SPEC § 23.3).
const (
	MaxConnexions       = 32
	MaxConnexionsParIP  = 4
	DelaiInactivite     = 5 * time.Minute
	DelaiPoigneeDeMain  = 10 * time.Second
	tailleTamponLecture = 16 << 10
	ecartHeureMaxPasse  = 24 * time.Hour
	ecartHeureMaxFutur  = 5 * time.Minute
)

// Motifs de refus comptés (métrique mirador_collecteur_syslog_refus_total).
const (
	RefusEmetteur  = "emetteur_inconnu"
	RefusConnexion = "trop_de_connexions"
	RefusTLS       = "tls"
	RefusTrame     = "trame"
	RefusFormat    = "format"
	RefusDebit     = "debit"
	RefusFilterlog = "filterlog"
	RefusFile      = "file_pleine"
)

// Motifs : tous les motifs (ordre d'affichage).
var Motifs = []string{RefusEmetteur, RefusConnexion, RefusTLS, RefusTrame, RefusFormat, RefusDebit, RefusFilterlog, RefusFile}

// Règles d'adresses (remplaçables dans les tests seulement : écoute et
// émetteurs en 127.0.0.1).
var (
	adresseAutorisee = config.AdressePrivee
	lireEmetteur     = config.LireEmetteur
)

type emetteur struct {
	reseau  netip.Prefix
	machine string
}

// Serveur : écoute syslog TLS.
type Serveur struct {
	opt       Options
	emetteurs []emetteur

	mu       sync.Mutex
	parIP    map[netip.Addr]int
	seaux    map[string]*seau
	lignes   map[string]uint64 // par équipement
	actives  atomic.Int64
	ouverte  atomic.Bool // écoute ouverte en ce moment (mesure)
	refus    map[string]*atomic.Uint64
	ecoute   net.Addr
	pret     chan struct{}
	pretUnef sync.Once
}

// Nouveau prépare le serveur (adresse et émetteurs vérifiés).
func Nouveau(opt Options) (*Serveur, error) {
	if opt.Maintenant == nil {
		opt.Maintenant = time.Now
	}
	if opt.Debit <= 0 {
		opt.Debit = config.SyslogDebitDefaut
	}
	if opt.Rafale <= 0 {
		opt.Rafale = config.SyslogRafaleDefaut
	}
	hote, _, err := net.SplitHostPort(opt.Ecoute)
	if err != nil {
		return nil, err
	}
	a, err := netip.ParseAddr(hote)
	if err != nil || !adresseAutorisee(a) {
		// Défense en profondeur (déjà vérifié dans la configuration) : jamais
		// d'écoute sur une adresse publique ni sur toutes les adresses.
		return nil, fmt.Errorf("syslog : écoute refusée sur %q (adresse privée du LAN seulement)", opt.Ecoute)
	}
	s := &Serveur{opt: opt, parIP: map[netip.Addr]int{}, seaux: map[string]*seau{}, lignes: map[string]uint64{},
		refus: map[string]*atomic.Uint64{}, pret: make(chan struct{})}
	for _, m := range Motifs {
		s.refus[m] = &atomic.Uint64{}
	}
	for _, e := range opt.Emetteurs {
		p, err := lireEmetteur(e.Adresse)
		if err != nil {
			return nil, err
		}
		s.emetteurs = append(s.emetteurs, emetteur{reseau: p, machine: e.Machine})
	}
	return s, nil
}

// machineDe : équipement de l'adresse (le réseau le plus précis l'emporte).
func (s *Serveur) machineDe(a netip.Addr) string {
	a = a.Unmap()
	s.mu.Lock()
	defer s.mu.Unlock()
	best, bits := "", -1
	for _, e := range s.emetteurs {
		if e.reseau.Contains(a) && e.reseau.Bits() > bits {
			best, bits = e.machine, e.reseau.Bits()
		}
	}
	return best
}

// Pret : fermé quand l'écoute est ouverte (tests).
func (s *Serveur) Pret() <-chan struct{} { return s.pret }

// Adresse : adresse réellement écoutée (tests : port 0).
func (s *Serveur) Adresse() net.Addr { return s.ecoute }

// Tourner écoute jusqu'à l'arrêt du contexte.
// Ouverte : l'écoute est ouverte en ce moment.
func (s *Serveur) Ouverte() bool { return s.ouverte.Load() }

func (s *Serveur) Tourner(ctx context.Context) error {
	var lc net.ListenConfig
	l, err := lc.Listen(ctx, "tcp", s.opt.Ecoute)
	if err != nil {
		return fmt.Errorf("syslog : écoute impossible sur %s : %w", s.opt.Ecoute, err)
	}
	s.ecoute = l.Addr()
	s.ouverte.Store(true)
	defer s.ouverte.Store(false)
	s.pretUnef.Do(func() { close(s.pret) })
	if s.opt.Journal != nil {
		s.opt.Journal.Info("syslog TLS : écoute ouverte", "adresse", l.Addr().String(), "emetteurs", len(s.emetteurs))
	}
	go func() { <-ctx.Done(); _ = l.Close() }()
	cfgTLS := &tls.Config{MinVersion: tls.VersionTLS12, GetCertificate: s.opt.Certificat}
	var wg sync.WaitGroup
	defer wg.Wait()
	for {
		conn, err := l.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			if errors.Is(err, net.ErrClosed) {
				return err
			}
			time.Sleep(100 * time.Millisecond) // erreur passagère (trop de fichiers ouverts…)
			continue
		}
		ip := adresseDe(conn.RemoteAddr())
		machine := s.machineDe(ip)
		if machine == "" {
			// Émetteur inconnu : fermé avant toute poignée de main TLS.
			s.refus[RefusEmetteur].Add(1)
			_ = conn.Close()
			continue
		}
		if !s.prendrePlace(ip) {
			s.refus[RefusConnexion].Add(1)
			_ = conn.Close()
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer s.rendrePlace(ip)
			s.servir(ctx, tls.Server(conn, cfgTLS), machine)
		}()
	}
}

func adresseDe(a net.Addr) netip.Addr {
	if t, ok := a.(*net.TCPAddr); ok {
		ip, _ := netip.AddrFromSlice(t.IP)
		return ip.Unmap()
	}
	return netip.Addr{}
}

func (s *Serveur) prendrePlace(ip netip.Addr) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.actives.Load() >= MaxConnexions || s.parIP[ip] >= MaxConnexionsParIP {
		return false
	}
	s.parIP[ip]++
	s.actives.Add(1)
	return true
}

func (s *Serveur) rendrePlace(ip netip.Addr) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.parIP[ip]--; s.parIP[ip] <= 0 {
		delete(s.parIP, ip)
	}
	s.actives.Add(-1)
}

// servir lit les messages d'une connexion (fermée après 5 min sans rien).
func (s *Serveur) servir(ctx context.Context, c *tls.Conn, machine string) {
	defer c.Close()
	// Fermeture à l'arrêt du collecteur ; la surveillance s'arrête avec la
	// connexion (relecture du 21/11 : une goroutine restait par connexion
	// jusqu'au redémarrage, ~14 Kio chacune).
	arret := context.AfterFunc(ctx, func() { _ = c.Close() })
	defer arret()
	_ = c.SetDeadline(time.Now().Add(DelaiPoigneeDeMain))
	if err := c.HandshakeContext(ctx); err != nil {
		s.refus[RefusTLS].Add(1)
		return
	}
	r := bufio.NewReaderSize(c, tailleTamponLecture)
	for {
		_ = c.SetReadDeadline(time.Now().Add(DelaiInactivite))
		trame, err := LireTrame(r)
		if err != nil {
			if errors.Is(err, ErrTrame) {
				s.refus[RefusTrame].Add(1)
			}
			return
		}
		s.recevoir(machine, trame)
	}
}

// recevoir : un message lu (débit, filterlog, format, heure).
func (s *Serveur) recevoir(machine string, trame []byte) {
	maintenant := s.opt.Maintenant()
	if !s.seau(machine).prendre(maintenant) {
		s.refus[RefusDebit].Add(1)
		return
	}
	m, ok := Analyser(trame)
	if !ok {
		s.refus[RefusFormat].Add(1)
		return
	}
	if m.App == "filterlog" && !s.opt.GarderFilterlog {
		s.refus[RefusFilterlog].Add(1)
		return
	}
	heure := m.Heure
	// Heure annoncée gardée seulement si elle est plausible (RFC 5424 avec
	// fuseau) ; sinon heure de réception (RFC 3164 n'a ni année ni fuseau).
	if heure.IsZero() || heure.Year() == 0 || heure.Before(maintenant.Add(-ecartHeureMaxPasse)) || heure.After(maintenant.Add(ecartHeureMaxFutur)) {
		heure = maintenant
	}
	if !s.opt.Sortie(Ligne{Hote: machine, App: m.App, Niveau: m.Niveau, Texte: m.Texte, Heure: heure}) {
		s.refus[RefusFile].Add(1)
		return
	}
	s.mu.Lock()
	s.lignes[machine]++
	s.mu.Unlock()
}

// Etat : compteurs pour les métriques du collecteur.
type Etat struct {
	Lignes     map[string]uint64 // par équipement
	Refus      map[string]uint64 // par motif
	Connexions int64
}

// Etat renvoie une copie des compteurs.
func (s *Serveur) Etat() Etat {
	e := Etat{Lignes: map[string]uint64{}, Refus: map[string]uint64{}, Connexions: s.actives.Load()}
	s.mu.Lock()
	for k, v := range s.lignes {
		e.Lignes[k] = v
	}
	s.mu.Unlock()
	for k, v := range s.refus {
		e.Refus[k] = v.Load()
	}
	return e
}

// --- Débit par équipement (seau à jetons) ------------------------------------

type seau struct {
	mu            sync.Mutex
	jetons        float64
	debit, rafale float64
	dernier       time.Time
}

func (s *Serveur) seau(machine string) *seau {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.seaux[machine]
	if !ok {
		b = &seau{jetons: s.opt.Rafale, debit: s.opt.Debit, rafale: s.opt.Rafale}
		s.seaux[machine] = b
	}
	return b
}

func (b *seau) prendre(maintenant time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.dernier.IsZero() {
		b.jetons = min(b.rafale, b.jetons+maintenant.Sub(b.dernier).Seconds()*b.debit)
	}
	b.dernier = maintenant
	if b.jetons < 1 {
		return false
	}
	b.jetons--
	return true
}
