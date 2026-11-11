// SPDX-License-Identifier: GPL-3.0-or-later

// Package web : les deux serveurs HTTP de mirador-server (SPEC.md § 5).
//
//   - écoute « agents » : API /api/agent/v1/… (token par machine) ;
//   - écoute « interface » : pages et API de l'interface (session + 2FA, étape 5).
//
// Les deux sont séparées : une faille ou une surcharge d'un côté ne donne pas
// accès à l'autre, et nginx peut appliquer des règles différentes (liste d'IP,
// filtrage géographique…).
package web

import (
	"context"
	"crypto/tls"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"time"
)

// Options des serveurs.
type Options struct {
	EcouteAgents    string
	EcouteInterface string
	TLSCertificat   string
	TLSCle          string
	Proxys          []netip.Prefix // adresses de nginx (X-Forwarded-For accepté)
	Journal         *slog.Logger
	// Agents et Interface : gestionnaires des routes (fournis par le serveur).
	Agents    http.Handler
	Interface http.Handler
}

// Serveurs : les deux serveurs HTTP.
type Serveurs struct {
	agents, iface *http.Server
	opt           Options
}

// Limites communes (protection contre les clients lents ou trop bavards).
const (
	tailleMaxEntetes = 16 << 10
	delaiEntetes     = 10 * time.Second
	delaiLecture     = 60 * time.Second
	delaiEcriture    = 60 * time.Second
	delaiInactif     = 120 * time.Second
)

// Nouveau prépare les deux serveurs (sans les démarrer).
func Nouveau(opt Options) *Serveurs {
	cfgTLS := &tls.Config{MinVersion: tls.VersionTLS13}
	nouveau := func(adr string, h http.Handler) *http.Server {
		return &http.Server{
			Addr:              adr,
			Handler:           h,
			ReadHeaderTimeout: delaiEntetes,
			ReadTimeout:       delaiLecture,
			WriteTimeout:      delaiEcriture,
			IdleTimeout:       delaiInactif,
			MaxHeaderBytes:    tailleMaxEntetes,
			TLSConfig:         cfgTLS,
			ErrorLog:          slog.NewLogLogger(opt.Journal.Handler(), slog.LevelDebug),
		}
	}
	ip := IPClient(opt.Proxys)
	return &Serveurs{
		opt:    opt,
		agents: nouveau(opt.EcouteAgents, ip(opt.Agents)),
		iface:  nouveau(opt.EcouteInterface, ip(EntetesSecurite(opt.Interface))),
	}
}

// Demarrer ouvre les deux écoutes ; une erreur d'ouverture est renvoyée tout
// de suite (port occupé…), les erreurs ultérieures vont dans le journal.
func (s *Serveurs) Demarrer() error {
	for _, srv := range []*http.Server{s.agents, s.iface} {
		l, err := net.Listen("tcp", srv.Addr)
		if err != nil {
			return err
		}
		go func(srv *http.Server, l net.Listener) {
			var err error
			if s.opt.TLSCertificat != "" {
				err = srv.ServeTLS(l, s.opt.TLSCertificat, s.opt.TLSCle)
			} else {
				err = srv.Serve(l)
			}
			if err != nil && !errors.Is(err, http.ErrServerClosed) {
				s.opt.Journal.Error("serveur HTTP arrêté", "adresse", srv.Addr, "erreur", err)
			}
		}(srv, l)
	}
	return nil
}

// Arreter laisse finir les requêtes en cours (au plus delai).
func (s *Serveurs) Arreter(delai time.Duration) {
	ctx, annuler := context.WithTimeout(context.Background(), delai)
	defer annuler()
	_ = s.agents.Shutdown(ctx)
	_ = s.iface.Shutdown(ctx)
}

// --- IP du client ------------------------------------------------------------------

type cleIP struct{}

// AdresseClient renvoie l'IP du client calculée par IPClient.
func AdresseClient(r *http.Request) netip.Addr {
	if a, ok := r.Context().Value(cleIP{}).(netip.Addr); ok {
		return a
	}
	return netip.Addr{}
}

// IPClient : l'adresse du client est celle de la connexion, SAUF si la
// connexion vient d'un proxy de confiance (nginx) : on prend alors la
// dernière adresse de X-Forwarded-For (celle ajoutée par nginx). Un client ne
// peut donc pas se faire passer pour une autre adresse en écrivant lui-même
// cet en-tête (SPEC.md § 11.2).
func IPClient(proxys []netip.Prefix) func(http.Handler) http.Handler {
	return func(suite http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ip := adresseConnexion(r.RemoteAddr)
			if deConfiance(ip, proxys) {
				if x := r.Header.Get("X-Forwarded-For"); x != "" {
					parties := strings.Split(x, ",")
					if a, err := netip.ParseAddr(strings.TrimSpace(parties[len(parties)-1])); err == nil {
						ip = a.Unmap()
					}
				}
			}
			suite.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), cleIP{}, ip)))
		})
	}
}

func adresseConnexion(remote string) netip.Addr {
	if ap, err := netip.ParseAddrPort(remote); err == nil {
		return ap.Addr().Unmap()
	}
	a, _ := netip.ParseAddr(remote)
	return a.Unmap()
}

func deConfiance(ip netip.Addr, proxys []netip.Prefix) bool {
	for _, p := range proxys {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}

// --- En-têtes de sécurité de l'interface -------------------------------------------

// CSP stricte : aucun script ni style en ligne, aucune ressource externe
// (tout est embarqué), pas d'intégration dans un cadre (SPEC.md § 11.1).
const csp = "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; " +
	"connect-src 'self'; font-src 'self'; object-src 'none'; base-uri 'none'; " +
	"form-action 'self'; frame-ancestors 'none'"

// EntetesSecurite ajoute les en-têtes de sécurité à toutes les réponses de
// l'interface.
func EntetesSecurite(suite http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", csp)
		h.Set("X-Frame-Options", "DENY")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "same-origin")
		h.Set("Permissions-Policy", "geolocation=(), microphone=(), camera=(), payment=()")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		h.Set("Cross-Origin-Resource-Policy", "same-origin")
		h.Set("Cache-Control", "no-store")
		// HSTS : nginx le pose déjà ; on le pose aussi si TLS est fait ici.
		if r.TLS != nil {
			h.Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		}
		suite.ServeHTTP(w, r)
	})
}
