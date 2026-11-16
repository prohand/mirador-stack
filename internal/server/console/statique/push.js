// SPDX-License-Identifier: GPL-3.0-or-later
// mirador : notifications push et pastille de l'icône (SPEC § 35).
//  - sur chaque page : la pastille de l'icône de l'application installée
//    reçoit le nombre d'alertes en cours (attribut data-alertes du <body>) ;
//  - page « Mon compte » : bouton data-push="activer" (abonne cet appareil,
//    puis envoie l'abonnement par le formulaire voisin, jeton CSRF compris) ;
//    ligne de « cet appareil » marquée ; « Retirer » le désabonne aussi ici.
// Toutes les vérifications sont faites par le serveur.
"use strict";

(function () {
  // --- Pastille ------------------------------------------------------------
  function pastille(n) {
    if (!navigator.setAppBadge || isNaN(n)) { return; }
    (n > 0 ? navigator.setAppBadge(n) : navigator.clearAppBadge()).catch(function () { /* pas installée : rien */ });
  }
  pastille(parseInt(document.body.getAttribute("data-alertes"), 10));
  document.addEventListener("mirador:fragment", function () {
    var z = document.querySelector("[data-nb-alertes]");
    if (z) { pastille(parseInt(z.getAttribute("data-nb-alertes"), 10)); }
  });

  var possible = "serviceWorker" in navigator && "PushManager" in window && "Notification" in window;
  // Notifications déjà acceptées : le service worker est tenu à jour.
  if (possible && Notification.permission === "granted") {
    navigator.serviceWorker.register("/sw.js").catch(function () {});
  }

  // --- « Mon compte » ---------------------------------------------------------
  var zone = document.querySelector("[data-push-zone]");
  if (!zone) { return; }
  var form = zone.querySelector("[data-push-form]");
  var bouton = zone.querySelector("[data-push=\"activer\"]");
  function dire(texte) {
    var p = zone.querySelector("[data-push-erreur]");
    if (p) { p.textContent = texte; p.hidden = false; }
  }
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
  // Début du SHA-256 de l'adresse de l'abonnement : comme le serveur, pour
  // reconnaître « cet appareil » sans que la page contienne l'adresse.
  function empreinte(adresse) {
    if (!window.crypto || !window.crypto.subtle) { return Promise.resolve(""); }
    return window.crypto.subtle.digest("SHA-256", new TextEncoder().encode(adresse)).then(function (h) {
      var o = new Uint8Array(h), s = "";
      for (var i = 0; i < 8; i++) { s += (o[i] < 16 ? "0" : "") + o[i].toString(16); }
      return s;
    });
  }
  function abonnementLocal() {
    if (!possible) { return Promise.resolve(null); }
    return navigator.serviceWorker.getRegistration("/").then(function (r) { return r ? r.pushManager.getSubscription() : null; }).catch(function () { return null; });
  }

  if (!possible) {
    if (bouton) { bouton.disabled = true; }
    dire("Ce navigateur ne propose pas les notifications ici. Sur iPhone ou iPad : installer d'abord mirador sur l'écran d'accueil (Partager › Sur l'écran d'accueil), puis ouvrir cette page depuis l'application.");
    return;
  }

  // Ligne de cet appareil marquée ; bouton changé s'il est déjà abonné.
  abonnementLocal().then(function (a) {
    if (!a) { return; }
    empreinte(a.endpoint).then(function (e) {
      var ligne = e && zone.querySelector("[data-push-empreinte=\"" + e + "\"]");
      if (!ligne) { return; }
      ligne.classList.add("cet-appareil");
      var m = ligne.querySelector("[data-push-ici]");
      if (m) { m.hidden = false; }
      if (bouton) { bouton.textContent = "Renouveler l'abonnement de cet appareil"; bouton.classList.add("secondaire"); }
    });
  });

  if (bouton && form) {
    bouton.addEventListener("click", function () {
      bouton.disabled = true;
      // La demande d'autorisation doit suivre le clic (règle des navigateurs).
      Notification.requestPermission().then(function (reponse) {
        if (reponse !== "granted") { throw new Error("refus"); }
        return navigator.serviceWorker.register("/sw.js");
      }).then(function () { return navigator.serviceWorker.ready; }).then(function (r) {
        return r.pushManager.subscribe({ userVisibleOnly: true, applicationServerKey: versOctets(bouton.getAttribute("data-cle")) });
      }).then(function (a) {
        form.elements.adresse.value = a.endpoint;
        form.elements.p256dh.value = versTexte(a.getKey("p256dh"));
        form.elements.auth.value = versTexte(a.getKey("auth"));
        // Envoi par le bouton caché du formulaire : s'il faut le code 2FA
        // (mode administration fermé), la boîte habituelle le demande.
        var envoyer = form.querySelector("[data-push-envoyer]");
        if (form.requestSubmit) { form.requestSubmit(envoyer); } else { form.submit(); }
      }).catch(function (e) {
        bouton.disabled = false;
        dire(e && e.message === "refus" ? "Notifications refusées sur cet appareil : les autoriser pour ce site dans les réglages du navigateur ou du téléphone, puis recommencer."
          : "L'abonnement de cet appareil a échoué" + (e && e.name ? " (" + e.name + ")" : "") + ".");
      });
    });
  }

  // « Retirer » la ligne de cet appareil : il est aussi désabonné ici.
  zone.addEventListener("submit", function (ev) {
    var f = ev.target;
    if (!f.matches || !f.matches("[data-push-retirer]") || f.getAttribute("data-fait")) { return; }
    var ligne = f.closest("[data-push-empreinte]");
    if (!ligne || !ligne.classList.contains("cet-appareil")) { return; }
    ev.preventDefault();
    f.setAttribute("data-fait", "1");
    abonnementLocal().then(function (a) { return a ? a.unsubscribe() : null; }).catch(function () {}).then(function () { f.submit(); });
  });
})();
