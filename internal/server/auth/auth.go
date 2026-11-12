// SPDX-License-Identifier: GPL-3.0-or-later

// Package auth : tout ce qui sert à savoir QUI est connecté à l'interface
// (SPEC § 18.2, § 18.22, § 18.27 à § 18.29), sans aucune page :
//
//   - comptes.go    : comptes et rôles en base (créer, désactiver, rôle…) ;
//   - motdepasse.go : empreintes Argon2id et règles des mots de passe ;
//   - totp.go       : double authentification (codes TOTP, QR code) ;
//   - sessions.go   : sessions en mémoire et cookie ;
//   - verrou.go     : limitation des essais (par adresse et par compte).
//
// Le paquet console (pages, formulaires, journal) s'en sert ; auth ne
// connaît pas console. Il se relit et se teste seul.
package auth
