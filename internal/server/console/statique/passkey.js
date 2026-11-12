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
    if (e && e.name === "NotAllowedError") { return "Demande annulée ou refusée par l'appareil."; }
    if (e && e.name === "InvalidStateError") { return "Cette passkey est déjà enregistrée sur cet appareil."; }
    if (e && e.name === "SecurityError") { return "Refusé par le navigateur : la page doit être ouverte en HTTPS par son nom de domaine."; }
    return "La passkey n'a pas pu être utilisée.";
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
        authenticatorSelection: { residentKey: "preferred", userVerification: "preferred" },
        attestation: "none",
        timeout: 120000
      } }).then(function (cle) {
        form.elements.client.value = versTexte(cle.response.clientDataJSON);
        form.elements.attestation.value = versTexte(cle.response.attestationObject);
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
    demande.then(function () { form.submit(); }).catch(function (e) { b.disabled = false; dire(form, explication(e)); });
  });
})();
