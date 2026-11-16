// SPDX-License-Identifier: GPL-3.0-or-later
// mirador : « service worker » des notifications push (SPEC § 35).
//
// Il ne fait QUE deux choses : afficher la notification reçue (et régler la
// pastille de l'icône), puis ouvrir la bonne page quand on la touche.
// Il n'intercepte aucune requête et ne garde rien en cache : aucune page de
// l'interface ne reste sur l'appareil, et l'interface marche pareil sans lui.
"use strict";

self.addEventListener("install", function () { self.skipWaiting(); });
self.addEventListener("activate", function (e) { e.waitUntil(self.clients.claim()); });

// chemin : seulement une page de ce site (« /alertes/12 »), sinon l'accueil.
function chemin(lien) {
  return typeof lien === "string" && lien.charAt(0) === "/" && lien.charAt(1) !== "/" && lien.indexOf("\\") < 0 ? lien : "/";
}

self.addEventListener("push", function (e) {
  var d = {};
  try { d = e.data ? e.data.json() : {}; } catch (err) { d = {}; }
  var taches = [self.registration.showNotification(typeof d.titre === "string" && d.titre ? d.titre : "mirador", {
    body: typeof d.corps === "string" ? d.corps : "",
    icon: "/statique/icones/icone-192.png",
    // Même groupe : la nouvelle notification remplace l'ancienne (et sonne de nouveau).
    tag: typeof d.groupe === "string" && d.groupe ? d.groupe : undefined,
    renotify: !!(typeof d.groupe === "string" && d.groupe),
    silent: d.son === false,
    data: { lien: chemin(d.lien) }
  })];
  // Pastille de l'icône : nombre d'alertes en cours (0 : retirée).
  if (typeof d.pastille === "number" && d.pastille >= 0 && self.navigator && self.navigator.setAppBadge) {
    taches.push((d.pastille > 0 ? self.navigator.setAppBadge(d.pastille) : self.navigator.clearAppBadge()).catch(function () {}));
  }
  e.waitUntil(Promise.all(taches));
});

self.addEventListener("notificationclick", function (e) {
  e.notification.close();
  var lien = chemin(e.notification.data && e.notification.data.lien);
  e.waitUntil(self.clients.matchAll({ type: "window", includeUncontrolled: true }).then(function (fenetres) {
    // Une fenêtre de mirador déjà ouverte : on s'en sert, sinon on en ouvre une.
    for (var i = 0; i < fenetres.length; i++) {
      var f = fenetres[i];
      if ("focus" in f && "navigate" in f) {
        return f.navigate(lien).then(function (c) { return (c || f).focus(); }).catch(function () { return self.clients.openWindow(lien); });
      }
    }
    return self.clients.openWindow(lien);
  }));
});
