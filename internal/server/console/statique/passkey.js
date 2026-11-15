// SPDX-License-Identifier: GPL-3.0-or-later
// mirador : passkeys (WebAuthn). Petit script sans dépendance.
//  - data-passkey="creer" : crée une passkey sur l'appareil (page « Mon compte ») ;
//  - data-passkey="utiliser" : signe la demande de connexion (étape du code 2FA).
// Le navigateur parle à l'appareil ; la réponse est mise dans le formulaire
// voisin (data-passkey-form), envoyé normalement avec son jeton CSRF.
// Toutes les vérifications sont faites par le serveur.
"use strict";

(function () {
  function versOctets(texte) {
    var s = texte.replace(/-/g, "+").replace(/_/g, "/");
    while (s.length % 4) { s += "="; }
    var brut = window.atob(s), out = new Uint8Array(brut.length);
    for (var i = 0; i < brut.length; i++) { out[i] = brut.charCodeAt(i); }
    return out;
  }
  function versTexte(tampon) {
    var o = new Uint8Array(tampon), s = "";
    for (var i = 0; i < o.length; i++) { s += String.fromCharCode(o[i]); }
    return window.btoa(s).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
  }
  function liste(texte) {
    return texte ? texte.split(",").map(function (id) { return { type: "public-key", id: versOctets(id) }; }) : [];
  }
  function dire(form, message) {
    var p = form.querySelector("[data-passkey-erreur]");
    if (p) { p.textContent = message; p.hidden = false; }
  }
  function explication(e) {
    var m;
    if (e && e.name === "NotAllowedError") { m = "Demande annulée, refusée par l'appareil, ou délai dépassé (réessayer)."; }
    else if (e && e.name === "InvalidStateError") { m = "Cette passkey est déjà enregistrée sur cet appareil."; }
    else if (e && e.name === "SecurityError") { m = "Refusé par le navigateur : la page doit être ouverte en HTTPS par son nom de domaine."; }
    else { m = "La passkey n'a pas pu être utilisée."; }
    // Nom technique de l'erreur : utile pour comprendre un refus sur un
    // téléphone (rien de secret).
    return e && e.name ? m + " (" + e.name + ")" : m;
  }

  // Connexion sans mot de passe : le défi est demandé au serveur DÈS
  // l'affichage de la page (puis toutes les 4 min, il vaut 5 min). Au clic,
  // la passkey est alors demandée tout de suite : Safari sur iPhone et iPad
  // refuse une demande faite après une attente réseau (le clic n'est plus
  // considéré comme « récent »).
  var defiPret = null; // { j: {defi, domaine}, le: date }
  var DUREE_DEFI_PRET = 240000;
  function preparerDefi() {
    if (!document.querySelector("[data-passkey=\"seule\"]")) { return; }
    fetch("/connexion/passkey-defi", { method: "POST", credentials: "same-origin", headers: { "X-Requested-With": "mirador" } })
      .then(function (r) { if (!r.ok) { throw new Error("defi"); } return r.json(); })
      .then(function (j) { defiPret = { j: j, le: Date.now() }; })
      .catch(function () { defiPret = null; });
  }
  preparerDefi();
  window.setInterval(preparerDefi, DUREE_DEFI_PRET);
  document.addEventListener("visibilitychange", function () {
    if (!document.hidden && (!defiPret || Date.now() - defiPret.le > DUREE_DEFI_PRET)) { preparerDefi(); }
  });
  // userVerification « preferred » et pas « required » : certains
  // gestionnaires sur iPhone (Bitwarden) échouent avec « required ». La
  // sécurité ne change pas : le serveur refuse une réponse sans le drapeau
  // « utilisateur vérifié » (code PIN, empreinte, visage).
  function demanderSeule(form, j) {
    form.elements.defi.value = j.defi;
    return navigator.credentials.get({ publicKey: {
      challenge: versOctets(j.defi), rpId: j.domaine, userVerification: "preferred", timeout: 120000
    } });
  }

  document.addEventListener("click", function (ev) {
    var b = ev.target.closest ? ev.target.closest("[data-passkey]") : null;
    if (!b) { return; }
    var form = b.closest("[data-passkey-form]");
    if (!form) { return; }
    if (!window.PublicKeyCredential || !navigator.credentials) {
      dire(form, "Ce navigateur ne connaît pas les passkeys (ou la page n'est pas en HTTPS).");
      return;
    }
    var d = function (n) { return b.getAttribute("data-" + n) || ""; };
    b.disabled = true;
    var demande;
    if (d("passkey") === "creer") {
      demande = navigator.credentials.create({ publicKey: {
        challenge: versOctets(d("defi")),
        rp: { id: d("domaine"), name: "mirador" },
        user: { id: versOctets(d("utilisateur")), name: d("compte"), displayName: d("compte") },
        pubKeyCredParams: d("algorithmes").split(",").map(function (a) { return { type: "public-key", alg: parseInt(a, 10) }; }),
        excludeCredentials: liste(d("exclure")),
        // Clé « découvrable » si l'appareil le permet : elle peut alors
        // servir seule, sans taper le nom du compte.
        authenticatorSelection: { residentKey: "preferred", userVerification: "preferred" },
        attestation: "none",
        timeout: 120000
      } }).then(function (cle) {
        form.elements.client.value = versTexte(cle.response.clientDataJSON);
        form.elements.attestation.value = versTexte(cle.response.attestationObject);
      });
    } else if (d("passkey") === "seule") {
      // Sans mot de passe : le défi est demandé au serveur, la clé choisit
      // le compte, et l'appareil doit vérifier l'utilisateur (code PIN,
      // empreinte, visage) : contrôlé par le serveur.
      var obtenue;
      if (defiPret && Date.now() - defiPret.le < DUREE_DEFI_PRET) {
        // Défi déjà là : la passkey est demandée pendant le clic même.
        var pret = defiPret.j;
        defiPret = null; // usage unique
        obtenue = demanderSeule(form, pret);
      } else {
        obtenue = fetch("/connexion/passkey-defi", { method: "POST", credentials: "same-origin", headers: { "X-Requested-With": "mirador" } })
          .then(function (r) { if (!r.ok) { throw new Error("defi"); } return r.json(); })
          .then(function (j) { return demanderSeule(form, j); });
      }
      demande = obtenue.then(function (cle) {
          form.elements.identifiant.value = versTexte(cle.rawId);
          form.elements.client.value = versTexte(cle.response.clientDataJSON);
          form.elements.authentificateur.value = versTexte(cle.response.authenticatorData);
          form.elements.signature.value = versTexte(cle.response.signature);
        });
    } else {
      demande = navigator.credentials.get({ publicKey: {
        challenge: versOctets(d("defi")),
        rpId: d("domaine"),
        allowCredentials: liste(d("ids")),
        userVerification: "preferred",
        timeout: 120000
      } }).then(function (cle) {
        form.elements.identifiant.value = versTexte(cle.rawId);
        form.elements.client.value = versTexte(cle.response.clientDataJSON);
        form.elements.authentificateur.value = versTexte(cle.response.authenticatorData);
        form.elements.signature.value = versTexte(cle.response.signature);
      });
    }
    demande.then(function () { form.submit(); }).catch(function (e) {
      b.disabled = false;
      dire(form, explication(e));
      preparerDefi(); // nouveau défi pour un nouvel essai
    });
  });
})();
