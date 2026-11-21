// SPDX-License-Identifier: GPL-3.0-or-later

package collecteur

import (
	"context"
	"log/slog"
	"net/url"
	"path/filepath"
	"time"

	"mirador-stack/internal/agent/config"
	"mirador-stack/internal/agent/envoi"
	"mirador-stack/internal/agent/logs"
	"mirador-stack/internal/agent/metrics"
	"mirador-stack/internal/collecteur/syslog"
)

// Syslog TLS du collecteur (partie 4, SPEC § 23.14) : écoute (adresse
// privée) → lot de logs (job « syslog », hôte = équipement de l'émetteur)
// → même file et même envoi que les logs de l'agent.

// Envoi d'un lot : toutes les 2 s, ou dès 512 Kio ; file de 4 Mio en mémoire.
const (
	attenteLotSyslog = 2 * time.Second
	tailleLotSyslog  = 512 << 10
	tailleFileSyslog = 4 << 20
	tailleCanal      = 1000 // 8 Mio au pire (messages de 8 Kio), quelques centaines de Ko en vrai
)

// Syslog : état du syslog dans le collecteur.
type Syslog struct {
	cfg     config.Agent
	journal *slog.Logger
	CSR     string // demande de certificat, envoyée avec le battement
	certs   syslog.Certificats
	serveur *syslog.Serveur // nil : pas de certificat encore
	canal   chan syslog.Ligne
	reessai time.Duration // attente avant de rouvrir une écoute en échec (0 : reessaiEcoute)
	masque  *logs.Masqueur
	lot     *logs.Lot
	file    *envoi.File
	exp     *envoi.Expediteur
}

// NouveauSyslog prépare le syslog si la configuration le demande (nil
// sinon) : clé (créée au premier démarrage), demande de certificat,
// certificat reçu du serveur. Sans certificat valable, l'écoute n'ouvre pas
// (TLS obligatoire) : elle ouvrira au redémarrage qui suit sa réception.
func NouveauSyslog(cfg config.Agent, token string, journal *slog.Logger) (*Syslog, error) {
	sc := cfg.Collecteur.Syslog
	if sc == nil {
		return nil, nil
	}
	s := &Syslog{cfg: cfg, journal: journal, canal: make(chan syslog.Ligne, tailleCanal)}
	cle, err := syslog.ChargerOuCreerCle(filepath.Join(filepath.Dir(cfg.FichierConfigDistante), "syslog.key"))
	if err != nil {
		return nil, err
	}
	if s.CSR, err = syslog.DemandeCertificat(cle, sc.NomCertificat); err != nil {
		return nil, err
	}
	if sc.Certificat == "" {
		journal.Info("syslog : en attente du certificat (demande envoyée au serveur)", "nom", sc.NomCertificat)
	} else if err := s.certs.Definir(sc.Certificat, cle, sc.NomCertificat, time.Now()); err != nil {
		journal.Warn("syslog : certificat reçu inutilisable, nouvelle demande envoyée au serveur", "erreur", err)
	}
	if s.certs.Fin().IsZero() {
		return s, nil
	}
	if s.masque, err = logs.NewMasqueur(cfg.Masquage, cfg.MasquagePerso); err != nil {
		return nil, err
	}
	s.serveur, err = syslog.Nouveau(syslog.Options{Ecoute: sc.Ecoute, Emetteurs: sc.Emetteurs, GarderFilterlog: sc.GarderFilterlog,
		Debit: sc.Debit, Rafale: sc.Rafale, Certificat: s.certs.Obtenir, Journal: journal, Sortie: s.sortie})
	if err != nil {
		return nil, err
	}
	s.lot = logs.NewLot("", "syslog", "", "", tailleLotSyslog)
	s.file = envoi.NewFile(tailleFileSyslog)
	s.exp = envoi.NewExpediteur(s.file, envoi.Options{URL: urlLogs(cfg.URLLogs), Token: token,
		Entetes: cfg.EntetesMachine(map[string]string{"Content-Type": "application/stream+json", "Content-Encoding": "gzip"}),
		Journal: journal})
	return s, nil
}

// urlLogs : mêmes paramètres que les logs de l'agent (flux, heure, message).
func urlLogs(brute string) string {
	u, err := url.Parse(brute)
	if err != nil {
		return brute
	}
	q := u.Query()
	q.Set("_stream_fields", "host,job,site,env,level,ident")
	q.Set("_time_field", "_time")
	q.Set("_msg_field", "_msg")
	u.RawQuery = q.Encode()
	return u.String()
}

// reessaiEcoute : attente avant de rouvrir une écoute syslog en échec.
const reessaiEcoute = 30 * time.Second

// sortie : file d'attente vers le lot (jamais bloquante : pleine, la ligne
// est perdue et comptée par le serveur syslog).
func (s *Syslog) sortie(l syslog.Ligne) bool {
	select {
	case s.canal <- l:
		return true
	default:
		return false
	}
}

// Tourner : écoute, mise en lot et envoi jusqu'à l'arrêt de ctx ; l'envoi
// continue jusqu'à l'arrêt de ctxEnvoi.
func (s *Syslog) Tourner(ctx, ctxEnvoi context.Context) {
	if s.serveur == nil {
		return
	}
	go s.exp.Tourner(ctxEnvoi)
	// Écoute impossible (adresse pas encore posée au démarrage, DHCP lent,
	// port pris…) ou arrêtée : nouvel essai toutes les 30 s (relecture du
	// 21/11 : une seule erreur, puis plus de syslog jusqu'au redémarrage).
	// Une erreur qui se répète n'est écrite qu'une fois.
	reessai := s.reessai
	if reessai <= 0 {
		reessai = reessaiEcoute
	}
	go func() {
		derniere := ""
		for {
			err := s.serveur.Tourner(ctx)
			if ctx.Err() != nil {
				return
			}
			if err != nil && err.Error() != derniere {
				derniere = err.Error()
				s.journal.Error("syslog : écoute impossible ou arrêtée, nouvel essai toutes les 30 s", "erreur", err)
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(reessai):
			}
		}
	}()
	t := time.NewTicker(attenteLotSyslog)
	defer t.Stop()
	for {
		select {
		case l := <-s.canal:
			s.lot.Ajouter(l.Heure, s.masque.Masquer(l.Texte), logs.Champs{{"host", l.Hote}, {"ident", l.App}, {"level", l.Niveau}})
			if s.lot.Plein() {
				s.vider()
			}
		case <-t.C:
			s.vider()
		case <-ctx.Done():
			// Lignes encore dans la file : mises dans le dernier lot
			// (relecture du 21/11 : jusqu'à 1 000 perdues à l'arrêt).
			for {
				select {
				case l := <-s.canal:
					s.lot.Ajouter(l.Heure, s.masque.Masquer(l.Texte), logs.Champs{{"host", l.Hote}, {"ident", l.App}, {"level", l.Niveau}})
					continue
				default:
				}
				break
			}
			s.vider()
			return
		}
	}
}

// Vider : attend (au plus delai) que les lots syslog en file soient partis.
// Sans syslog (s nil ou non configuré) : rien.
func (s *Syslog) Vider(delai time.Duration) {
	if s == nil || s.file == nil {
		return
	}
	fin := time.Now().Add(delai)
	for time.Now().Before(fin) {
		if lots, _, _ := s.file.Etat(); lots == 0 {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func (s *Syslog) vider() {
	if donnees, n := s.lot.Vider(); n > 0 {
		if !s.file.Ajouter(donnees) {
			s.journal.Warn("syslog : lot trop gros pour la file, abandonné", "lignes", n)
		}
	}
}

// Collecteur de mesures du syslog.
func (s *Syslog) Nom() string { return "syslog" }
func (s *Syslog) Lent() bool  { return false }

func (s *Syslog) Collecter(b *metrics.Batch) error {
	if fin := s.certs.Fin(); !fin.IsZero() {
		b.Add("mirador_collecteur_syslog_certificat_fin_timestamp_seconds", float64(fin.Unix()))
	}
	if s.serveur == nil {
		b.Add("mirador_collecteur_syslog_ecoute", 0)
		return nil
	}
	ouverte := 0.0
	if s.serveur.Ouverte() {
		ouverte = 1
	}
	b.Add("mirador_collecteur_syslog_ecoute", ouverte)
	e := s.serveur.Etat()
	b.Add("mirador_collecteur_syslog_connexions", float64(e.Connexions))
	for eq, n := range e.Lignes {
		b.Add("mirador_collecteur_syslog_lignes_total", float64(n), metrics.L("equipement", eq))
	}
	for _, motif := range syslog.Motifs {
		b.Add("mirador_collecteur_syslog_refus_total", float64(e.Refus[motif]), metrics.L("motif", motif))
	}
	return nil
}
